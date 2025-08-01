// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package optbuilder

import (
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/concurrency/isolation"
	"github.com/cockroachdb/cockroach/pkg/server/telemetry"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/colinfo"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/schemaexpr"
	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/cat"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/memo"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/props"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/cast"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/idxtype"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlerrors"
	"github.com/cockroachdb/cockroach/pkg/sql/sqltelemetry"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/errorutil/unimplemented"
	"github.com/cockroachdb/cockroach/pkg/util/intsets"
	"github.com/cockroachdb/errors"
)

// mutationBuilder is a helper struct that supports building Insert, Update,
// Upsert, and Delete operators in stages.
type mutationBuilder struct {
	b  *Builder
	md *opt.Metadata

	// opName is the statement's name, used in error messages.
	opName string

	// tab is the target table.
	tab cat.Table

	// tabID is the metadata ID of the table.
	tabID opt.TableID

	// alias is the table alias specified in the mutation statement, or just the
	// resolved table name if no alias was specified.
	alias tree.TableName

	// outScope contains the current set of columns that are in scope, as well as
	// the output expression as it is incrementally built. Once the final mutation
	// expression is completed, it will be contained in outScope.expr.
	outScope *scope

	// fetchScope contains the set of columns fetched from the target table.
	fetchScope *scope

	// insertExpr is the expression that produces the values which will be
	// inserted into the target table. It is only populated for INSERT
	// expressions. It is currently used to inline constant insert values into
	// uniqueness checks.
	//
	// insertExpr may not be set (e.g. if there are INSERT triggers).
	insertExpr memo.RelExpr

	// targetColList is an ordered list of IDs of the table columns into which
	// values will be inserted, or which will be updated with new values. It is
	// incrementally built as the mutation operator is built.
	targetColList opt.ColList

	// targetColSet contains the same column IDs as targetColList, but as a set.
	targetColSet opt.ColSet

	// insertColIDs lists the input column IDs providing values to insert. Its
	// length is always equal to the number of columns in the target table,
	// including mutation columns. Table columns which will not have values
	// inserted are set to 0 (e.g. delete-only mutation columns). insertColIDs
	// is empty if this is not an Insert/Upsert operator.
	insertColIDs opt.OptionalColList

	// implicitInsertCols contains columns in insertColIDs which were not given
	// explicit values in the insert statement, if b.trackSchemaDeps is true.
	// It does not include columns that were explicitly given the value of
	// DEFAULT, e.g., INSERT INTO t VALUES (1, DEFAULT).
	implicitInsertCols opt.ColSet

	// fetchColIDs lists the input column IDs storing values which are fetched
	// from the target table in order to provide existing values that will form
	// lookup and update values. Its length is always equal to the number of
	// columns in the target table, including mutation columns. Table columns
	// which do not need to be fetched are set to 0. fetchColIDs is empty if
	// this is an Insert operator.
	fetchColIDs opt.OptionalColList

	// updateColIDs lists the input column IDs providing update values. Its
	// length is always equal to the number of columns in the target table,
	// including mutation columns. Table columns which do not need to be
	// updated are set to 0.
	updateColIDs opt.OptionalColList

	// upsertColIDs lists the input column IDs that choose between an insert or
	// update column using a CASE expression:
	//
	//   CASE WHEN canary_col IS NULL THEN ins_col ELSE upd_col END
	//
	// These columns are used to compute constraints and to return result rows.
	// The length of upsertColIDs is always equal to the number of columns in
	// the target table, including mutation columns. Table columns which do not
	// need to be updated are set to 0. upsertColIDs is empty if this is not
	// an Upsert operator.
	upsertColIDs opt.OptionalColList

	// checkColIDs lists the input column IDs storing the boolean results of
	// evaluating check constraint expressions defined on the target table. Its
	// length is always equal to the number of check constraints on the table
	// (see opt.Table.CheckCount).
	checkColIDs opt.OptionalColList

	// partialIndexPutColIDs lists the input column IDs storing the boolean
	// results of evaluating partial index predicate expressions of the target
	// table. The predicate expressions are evaluated with their variables
	// assigned from newly inserted or updated row values. When these columns
	// evaluate to true, it signifies that the inserted or updated row should be
	// added to the corresponding partial index. The length of
	// partialIndexPutColIDs is always equal to the number of partial indexes on
	// the table.
	partialIndexPutColIDs opt.OptionalColList

	// partialIndexDelColIDs lists the input column IDs storing the boolean
	// results of evaluating partial index predicate expressions of the target
	// table. The predicate expressions are evaluated with their variables
	// assigned from existing row values of deleted or updated rows. When these
	// columns evaluate to true, it signifies that the deleted or updated row
	// should be removed from the corresponding partial index. The length of
	// partialIndexPutColIDs is always equal to the number of partial indexes on
	// the table.
	partialIndexDelColIDs opt.OptionalColList

	// vectorIndexDelPartitionColIDs lists the input column IDs storing the keys
	// for the partitions that the deleted or updated rows should be removed from.
	// The length is always equal to the number of vector indexes on the table.
	vectorIndexDelPartitionColIDs opt.OptionalColList

	// vectorIndexPutPartitionColIDs lists the input column IDs storing the keys
	// for the partitions that the inserted or updated rows should be added to.
	// The length is always equal to the number of vector indexes on the table.
	vectorIndexPutPartitionColIDs opt.OptionalColList

	// vectorIndexPutQuantizedVecColIDs lists the input column IDs storing the
	// quantized and encoded vectors that should be inserted into the index. Note
	// that the quantized vectors are not needed for deletions. The length is
	// always equal to the number of vector indexes on the table.
	vectorIndexPutQuantizedVecColIDs opt.OptionalColList

	// triggerColIDs is the set of column IDs used to project the OLD and NEW rows
	// for row-level AFTER triggers, and possibly also contains the canary column.
	// It is only populated if the mutation statement has row-level AFTER
	// triggers.
	//
	// NOTE: triggerColIDs may contain columns both contained and not contained in
	// the lists above.
	triggerColIDs opt.ColSet

	// canaryColID is the ID of the column that is used to decide whether to
	// insert or update each row. If the canary column's value is null, then it's
	// an insert; otherwise it's an update.
	canaryColID opt.ColumnID

	// arbiters is the set of indexes and unique constraints that are used to
	// detect conflicts for UPSERT and INSERT ON CONFLICT statements.
	arbiters arbiterSet

	// subqueries temporarily stores subqueries that were built during initial
	// analysis of SET expressions. They will be used later when the subqueries
	// are joined into larger LEFT OUTER JOIN expressions.
	subqueries []*scope

	// parsedColComputedExprs is a cached set of parsed computed expressions
	// from the table schema. These are parsed once and cached for reuse.
	parsedColComputedExprs []tree.Expr

	// parsedColDefaultExprs is a cached set of parsed default expressions
	// from the table schema. These are parsed once and cached for reuse.
	parsedColDefaultExprs []tree.Expr

	// parsedColOnUpdateExprs is a cached set of parsed ON UPDATE expressions from
	// the table schema. These are parsed once and cached for reuse.
	parsedColOnUpdateExprs []tree.Expr

	// parsedIndexExprs is a cached set of parsed partial index predicate
	// expressions from the table schema. These are parsed once and cached for
	// reuse.
	parsedIndexExprs []tree.Expr

	// parsedUniqueConstraintExprs is a cached set of parsed partial unique
	// constraint predicate expressions from the table schema. These are parsed
	// once and cached for reuse.
	parsedUniqueConstraintExprs []tree.Expr

	// uniqueChecks contains unique check queries; see buildUnique* methods.
	uniqueChecks memo.UniqueChecksExpr

	// fastPathUniqueChecks contains fast path unique check queries which are used for
	// insert fast path; see buildInsertionCheck.
	fastPathUniqueChecks memo.FastPathUniqueChecksExpr

	// fkChecks contains foreign key check queries; see buildFK* methods.
	fkChecks memo.FKChecksExpr

	// cascades contains foreign key check cascades; see buildFK* methods.
	cascades memo.FKCascades

	// afterTriggers contains AFTER triggers; see buildRowLevelAfterTriggers.
	afterTriggers *memo.AfterTriggers

	// withID is nonzero if we need to buffer the input for FK or uniqueness
	// checks.
	withID opt.WithID

	// extraAccessibleCols stores all the columns that are available to the
	// mutation that are not part of the target table. This is useful for
	// UPDATE ... FROM queries and DELETE ... USING queries, as the columns
	// from the FROM and USING tables must be made accessible to the
	// RETURNING clause, respectively.
	extraAccessibleCols []scopeColumn

	// fkCheckHelper is used to prevent allocating the helper separately.
	fkCheckHelper fkCheckHelper

	// uniqueCheckHelper is used to prevent allocating the helper separately.
	uniqueCheckHelper uniqueCheckHelper

	// arbiterPredicateHelper is used to prevent allocating the helper
	// separately.
	arbiterPredicateHelper arbiterPredicateHelper

	// inputForInsertExpr stores the result of outscope.expr from the most
	// recent call to buildInputForInsert.
	inputForInsertExpr memo.RelExpr

	// uniqueWithTombstoneIndexes is the set of unique indexes that ensure uniqueness
	// by writing tombstones to all partitions
	uniqueWithTombstoneIndexes intsets.Fast

	// regionColExplicitlyMutated is true if the target table is regional-by-row
	// and the value for the region column is explicitly specified for insert or
	// update. Example:
	//   INSERT INTO t (a, b, region) VALUES (1, 2, 'us-east-1');
	regionColExplicitlyMutated bool
}

func (mb *mutationBuilder) init(b *Builder, opName string, tab cat.Table, alias tree.TableName) {
	// This initialization pattern ensures that fields are not unwittingly
	// reused. Field reuse must be explicit.
	*mb = mutationBuilder{
		b:      b,
		md:     b.factory.Metadata(),
		opName: opName,
		tab:    tab,
		alias:  alias,
	}

	tabCols := tab.ColumnCount()
	mb.targetColList = make(opt.ColList, 0, tabCols)

	// Allocate segmented array of column IDs.
	numPartialIndexes := partialIndexCount(tab)
	numVectorIndexes := vectorIndexCount(tab)
	numChecks := tab.CheckCount()
	colIDs := make(opt.OptionalColList, 4*tabCols+numChecks+2*numPartialIndexes+3*numVectorIndexes)

	var start int
	getSlice := func(n int) opt.OptionalColList {
		slice := colIDs[start : start+n]
		start += n
		return slice
	}
	mb.insertColIDs = getSlice(tabCols)
	mb.fetchColIDs = getSlice(tabCols)
	mb.updateColIDs = getSlice(tabCols)
	mb.upsertColIDs = getSlice(tabCols)
	mb.checkColIDs = getSlice(numChecks)
	mb.partialIndexPutColIDs = getSlice(numPartialIndexes)
	mb.partialIndexDelColIDs = getSlice(numPartialIndexes)
	mb.vectorIndexPutPartitionColIDs = getSlice(numVectorIndexes)
	mb.vectorIndexPutQuantizedVecColIDs = getSlice(numVectorIndexes)
	mb.vectorIndexDelPartitionColIDs = getSlice(numVectorIndexes)

	// Add the table and its columns (including mutation columns) to metadata.
	mb.tabID = mb.md.AddTable(tab, &mb.alias)
}

// setFetchColIDs sets the list of columns that are fetched in order to provide
// values to the mutation operator. The given columns must come from buildScan.
func (mb *mutationBuilder) setFetchColIDs(cols []scopeColumn) {
	for i := range cols {
		// Ensure that we don't add system columns to the fetch columns.
		if cols[i].kind != cat.System {
			mb.fetchColIDs[cols[i].tableOrdinal] = cols[i].id
		}
	}
}

// buildInputForUpdate constructs a Select expression from the fields in
// the Update operator, similar to this:
//
//	SELECT <cols>
//	FROM <table>
//	WHERE <where>
//	ORDER BY <order-by>
//	LIMIT <limit>
//
// All columns from the table to update are added to fetchColList.
// If a FROM clause is defined, we build out each of the table
// expressions required and JOIN them together (LATERAL joins between
// the tables are allowed). We then JOIN the result with the target
// table (the FROM tables can't reference this table) and apply the
// appropriate WHERE conditions.
//
// It is the responsibility of the user to guarantee that the JOIN
// produces a maximum of one row per row of the target table. If multiple
// are found, an arbitrary one is chosen (this row is not readily
// predictable, consistent with the POSTGRES implementation).
// buildInputForUpdate stores the columns of the FROM tables in the
// mutation builder so they can be made accessible to other parts of
// the query (RETURNING clause).
// TODO(andyk): Do needed column analysis to project fewer columns if possible.
func (mb *mutationBuilder) buildInputForUpdate(
	inScope *scope,
	texpr tree.TableExpr,
	from tree.TableExprs,
	where *tree.Where,
	whereColRefs *opt.ColSet,
	limit *tree.Limit,
	orderBy tree.OrderBy,
) {
	var indexFlags *tree.IndexFlags
	if source, ok := texpr.(*tree.AliasedTableExpr); ok && source.IndexFlags != nil {
		indexFlags = source.IndexFlags
		telemetry.Inc(sqltelemetry.IndexHintUseCounter)
		telemetry.Inc(sqltelemetry.IndexHintUpdateUseCounter)
	}

	if mb.b.evalCtx.SessionData().AvoidFullTableScansInMutations {
		if indexFlags == nil {
			indexFlags = &tree.IndexFlags{}
		}
		indexFlags.AvoidFullScan = true
	}

	// Fetch columns from different instance of the table metadata, so that it's
	// possible to remap columns, as in this example:
	//
	//   UPDATE abc SET a=b
	//
	// NOTE: Include mutation columns, but be careful to never use them for any
	//       reason other than as "fetch columns". See buildScan comment.
	mb.fetchScope = mb.b.buildScan(
		mb.b.addTable(mb.tab, &mb.alias),
		tableOrdinals(mb.tab, columnKinds{
			includeMutations: true,
			includeSystem:    true,
			includeInverted:  false,
		}),
		indexFlags,
		noRowLocking,
		inScope,
		false, /* disableNotVisibleIndex */
		cat.PolicyScopeUpdate,
	)

	// Set list of columns that will be fetched by the input expression.
	mb.setFetchColIDs(mb.fetchScope.cols)

	// If there is a FROM clause present, we must join all the tables
	// together with the table being updated.
	fromClausePresent := len(from) > 0
	if fromClausePresent {
		fromScope := mb.b.buildFromTables(from, noLocking, inScope)

		// Check that the same table name is not used multiple times.
		mb.b.validateJoinTableNames(mb.fetchScope, fromScope)

		// The FROM table columns can be accessed by the RETURNING clause of the
		// query and so we have to make them accessible.
		mb.extraAccessibleCols = fromScope.cols

		// Add the columns in the FROM scope.
		// We create a new scope so that fetchScope is not modified. It will be
		// used later to build partial index predicate expressions, and we do
		// not want ambiguities with column names in the FROM clause.
		mb.outScope = mb.fetchScope.replace()
		mb.outScope.appendColumnsFromScope(mb.fetchScope)
		mb.outScope.appendColumnsFromScope(fromScope)

		left := mb.fetchScope.expr
		right := fromScope.expr
		mb.outScope.expr = mb.b.factory.ConstructInnerJoin(left, right, memo.TrueFilter, memo.EmptyJoinPrivate)
	} else {
		mb.outScope = mb.fetchScope
	}

	// WHERE
	mb.b.buildWhere(where, mb.outScope, whereColRefs)

	// SELECT + ORDER BY (which may add projected expressions)
	projectionsScope := mb.outScope.replace()
	projectionsScope.appendColumnsFromScope(mb.outScope)
	orderByScope := mb.b.analyzeOrderBy(orderBy, mb.outScope, projectionsScope,
		exprKindOrderByUpdate, tree.RejectGenerators|tree.RejectAggregates)
	mb.b.buildOrderBy(mb.outScope, projectionsScope, orderByScope)
	mb.b.constructProjectForScope(mb.outScope, projectionsScope)

	// LIMIT
	if limit != nil {
		mb.b.buildLimit(limit, inScope, projectionsScope)
	}

	mb.outScope = projectionsScope

	// Build a distinct-on operator on the primary key columns to ensure there
	// is at most one row in the joined output for every row in the target
	// table.
	if fromClausePresent {
		var pkCols opt.ColSet
		primaryIndex := mb.tab.Index(cat.PrimaryIndex)
		for i := 0; i < primaryIndex.KeyColumnCount(); i++ {
			col := primaryIndex.Column(i)
			pkCols.Add(mb.fetchColIDs[col.Ordinal()])
		}
		mb.outScope = mb.b.buildDistinctOn(
			pkCols, mb.outScope, false /* nullsAreDistinct */, "" /* errorOnDup */)
	}
}

// buildInputForDelete constructs a Select expression from the fields in
// the Delete operator, similar to this:
//
//	SELECT <cols>
//	FROM <table> [, <using-tables>]
//	WHERE <where>
//	ORDER BY <order-by>
//	LIMIT <limit>
//
// All columns from the table to update are added to fetchColList.
// TODO(andyk): Do needed column analysis to project fewer columns if possible.
func (mb *mutationBuilder) buildInputForDelete(
	inScope *scope,
	texpr tree.TableExpr,
	where *tree.Where,
	using tree.TableExprs,
	limit *tree.Limit,
	orderBy tree.OrderBy,
) {
	var indexFlags *tree.IndexFlags
	if source, ok := texpr.(*tree.AliasedTableExpr); ok && source.IndexFlags != nil {
		indexFlags = source.IndexFlags
		telemetry.Inc(sqltelemetry.IndexHintUseCounter)
		telemetry.Inc(sqltelemetry.IndexHintDeleteUseCounter)
	}

	if mb.b.evalCtx.SessionData().AvoidFullTableScansInMutations {
		if indexFlags == nil {
			indexFlags = &tree.IndexFlags{}
		}
		indexFlags.AvoidFullScan = true
	}

	// Fetch columns from different instance of the table metadata, so that it's
	// possible to remap columns, as in this example:
	//
	//   DELETE FROM abc WHERE a=b
	//
	// NOTE: Include mutation columns, but be careful to never use them for any
	//       reason other than as "fetch columns". See buildScan comment.
	// TODO(andyk): Why does execution engine need mutation columns for Delete?
	mb.fetchScope = mb.b.buildScan(
		mb.b.addTable(mb.tab, &mb.alias),
		tableOrdinals(mb.tab, columnKinds{
			includeMutations: true,
			includeSystem:    true,
			includeInverted:  false,
		}),
		indexFlags,
		noRowLocking,
		inScope,
		false, /* disableNotVisibleIndex */
		cat.PolicyScopeDelete,
	)

	// Set list of columns that will be fetched by the input expression.
	mb.setFetchColIDs(mb.fetchScope.cols)

	// USING
	usingClausePresent := len(using) > 0
	if usingClausePresent {
		usingScope := mb.b.buildFromTables(using, noLocking, inScope)

		// Check that the same table name is not used multiple times.
		mb.b.validateJoinTableNames(mb.fetchScope, usingScope)

		// The USING table columns can be accessed by the RETURNING clause of the
		// query and so we have to make them accessible.
		mb.extraAccessibleCols = usingScope.cols

		// Add the columns to the USING scope.
		// We create a new scope so that fetchScope is not modified
		// as fetchScope contains the set of columns from the target
		// table specified by USING. This will be used later with partial
		// index predicate expressions and will prevent ambiguities with
		// column names in the USING clause.
		mb.outScope = mb.fetchScope.replace()
		mb.outScope.appendColumnsFromScope(mb.fetchScope)
		mb.outScope.appendColumnsFromScope(usingScope)

		left := mb.fetchScope.expr
		right := usingScope.expr

		mb.outScope.expr = mb.b.factory.ConstructInnerJoin(left, right, memo.TrueFilter, memo.EmptyJoinPrivate)
	} else {
		mb.outScope = mb.fetchScope
	}

	// WHERE
	mb.b.buildWhere(where, mb.outScope, nil /* colRefs */)

	// SELECT + ORDER BY (which may add projected expressions)
	projectionsScope := mb.outScope.replace()
	projectionsScope.appendColumnsFromScope(mb.outScope)
	orderByScope := mb.b.analyzeOrderBy(orderBy, mb.outScope, projectionsScope,
		exprKindOrderByDelete, tree.RejectGenerators|tree.RejectAggregates)
	mb.b.buildOrderBy(mb.outScope, projectionsScope, orderByScope)
	mb.b.constructProjectForScope(mb.outScope, projectionsScope)

	// LIMIT
	if limit != nil {
		mb.b.buildLimit(limit, inScope, projectionsScope)
	}

	mb.outScope = projectionsScope

	// Build a distinct on to ensure there is at most one row in the joined output
	// for every row in the table.
	if usingClausePresent {
		var pkCols opt.ColSet

		// We need to ensure that the join has a maximum of one row for every row
		// in the table and we ensure this by constructing a distinct on the primary
		// key columns.
		primaryIndex := mb.tab.Index(cat.PrimaryIndex)
		for i := 0; i < primaryIndex.KeyColumnCount(); i++ {
			col := primaryIndex.Column(i)
			pkCols.Add(mb.fetchColIDs[col.Ordinal()])
		}

		mb.outScope = mb.b.buildDistinctOn(
			pkCols, mb.outScope, false /* nullsAreDistinct */, "" /* errorOnDup */)
	}
}

// addTargetColsByName adds one target column for each of the names in the given
// list.
func (mb *mutationBuilder) addTargetColsByName(names tree.NameList) {
	for _, name := range names {
		// Determine the ordinal position of the named column in the table and
		// add it as a target column.
		if ord := findPublicTableColumnByName(mb.tab, name); ord != -1 {
			// System columns are invalid target columns.
			if mb.tab.Column(ord).Kind() == cat.System {
				panic(pgerror.Newf(pgcode.InvalidColumnReference, "cannot modify system column %q", name))
			}
			mb.addTargetCol(ord)
			continue
		}
		panic(colinfo.NewUndefinedColumnError(string(name)))
	}
}

// addTargetCol adds a target column by its ordinal position in the target
// table. It raises an error if a mutation or computed column is targeted, or if
// the same column is targeted multiple times.
func (mb *mutationBuilder) addTargetCol(ord int) {
	tabCol := mb.tab.Column(ord)

	// Don't allow targeting of mutation columns.
	if tabCol.IsMutation() {
		panic(makeBackfillError(tabCol.ColName()))
	}

	// Computed columns cannot be targeted with input values.
	if tabCol.IsComputed() {
		panic(schemaexpr.CannotWriteToComputedColError(string(tabCol.ColName())))
	}

	// Ensure that the name list does not contain duplicates.
	colID := mb.tabID.ColumnID(ord)
	if mb.targetColSet.Contains(colID) {
		panic(pgerror.Newf(pgcode.Syntax,
			"multiple assignments to the same column %q", tabCol.ColName()))
	}
	mb.targetColSet.Add(colID)

	mb.targetColList = append(mb.targetColList, colID)
}

// extractValuesInput tests whether the given input is a VALUES clause with no
// WITH, ORDER BY, or LIMIT modifier. If so, it's returned, otherwise nil is
// returned.
func (mb *mutationBuilder) extractValuesInput(inputRows *tree.Select) *tree.ValuesClause {
	if inputRows == nil {
		return nil
	}

	// Only extract a simple VALUES clause with no modifiers.
	if inputRows.With != nil || inputRows.OrderBy != nil || inputRows.Limit != nil {
		return nil
	}

	// Discard parentheses.
	if parens, ok := inputRows.Select.(*tree.ParenSelect); ok {
		return mb.extractValuesInput(parens.Select)
	}

	if values, ok := inputRows.Select.(*tree.ValuesClause); ok {
		return values
	}

	return nil
}

// setRegionColExplicitlyMutated should be called for the insert and update
// columns of an INSERT, UPDATE, or UPSERT statement before building the
// synthesized columns. It keeps track of whether the region column is
// explicitly mutated in the statement (e.g. with user-provided values).
func (mb *mutationBuilder) setRegionColExplicitlyMutated(explicitCols opt.OptionalColList) {
	if !mb.tab.IsRegionalByRow() {
		return
	}
	// The region column is always the first column in the primary index.
	regionColOrd := mb.tab.Index(cat.PrimaryIndex).Column(0).Ordinal()
	if explicitCols[regionColOrd] != 0 {
		mb.regionColExplicitlyMutated = true
	}
}

// replaceDefaultExprs looks for DEFAULT specifiers in input value expressions
// and replaces them with the corresponding default value expression for the
// corresponding column. This is only possible when the input is a VALUES
// clause. For example:
//
//	INSERT INTO t (a, b) (VALUES (1, DEFAULT), (DEFAULT, 2))
//
// Here, the two DEFAULT specifiers are replaced by the default value expression
// for the a and b columns, respectively.
//
// replaceDefaultExprs returns a VALUES expression with replaced DEFAULT values,
// or just the unchanged input expression if there are no DEFAULT values.
func (mb *mutationBuilder) replaceDefaultExprs(inRows *tree.Select) (outRows *tree.Select) {
	values := mb.extractValuesInput(inRows)
	if values == nil || len(values.Rows) == 0 {
		return inRows
	}

	// Ensure that the number of input columns exactly matches the number of
	// target columns.
	numCols := len(values.Rows[0])
	mb.checkNumCols(len(mb.targetColList), numCols)

	var newRows []tree.Exprs
	for irow, tuple := range values.Rows {
		if len(tuple) != numCols {
			reportValuesLenError(numCols, len(tuple))
		}

		// Scan list of tuples in the VALUES row, looking for DEFAULT specifiers.
		var newTuple tree.Exprs
		for itup, val := range tuple {
			if _, ok := val.(tree.DefaultVal); ok {
				// Found DEFAULT, so lazily create new rows and tuple lists.
				if newRows == nil {
					newRows = make([]tree.Exprs, irow, len(values.Rows))
					copy(newRows, values.Rows[:irow])
				}

				if newTuple == nil {
					newTuple = make(tree.Exprs, itup, numCols)
					copy(newTuple, tuple[:itup])
				}

				val = mb.parseDefaultExpr(mb.targetColList[itup])
			}
			if newTuple != nil {
				newTuple = append(newTuple, val)
			}
		}

		if newRows != nil {
			if newTuple != nil {
				newRows = append(newRows, newTuple)
			} else {
				newRows = append(newRows, tuple)
			}
		}
	}

	if newRows != nil {
		return &tree.Select{Select: &tree.ValuesClause{Rows: newRows}}
	}
	return inRows
}

// addSynthesizedDefaultCols is a helper method for addSynthesizedColsForInsert
// and addSynthesizedColsForUpdate that scans the list of Ordinary and WriteOnly
// table columns, looking for any that are not computed and do not yet have
// values provided by the input expression. New columns are synthesized for any
// missing columns.
//
// Values are synthesized for columns based on checking these rules, in order:
//  1. If column has a default value specified for it, use that as its value.
//  2. If column is nullable, use NULL as its value.
//  3. If column is currently being added or dropped (i.e. a mutation column),
//     use a default value (0 for INT column, "" for STRING column, etc). Note
//     that the existing "fetched" value returned by the scan cannot be used,
//     since it may not have been initialized yet by the backfiller.
//
// If includeOrdinary is false, then only WriteOnly columns are considered.
//
// NOTE: colIDs is updated with the column IDs of any synthesized columns which
// are added to mb.outScope.
func (mb *mutationBuilder) addSynthesizedDefaultCols(
	colIDs opt.OptionalColList, includeOrdinary bool, applyOnUpdate bool,
) {
	// We will construct a new Project operator that will contain the newly
	// synthesized column(s).
	pb := makeProjectionBuilder(mb.b, mb.outScope)

	for i, n := 0, mb.tab.ColumnCount(); i < n; i++ {
		tabCol := mb.tab.Column(i)
		if kind := tabCol.Kind(); kind == cat.WriteOnly {
			// Always include WriteOnly columns.
		} else if tabCol.UseOnUpdate(mb.b.evalCtx.SessionData()) && applyOnUpdate {
			// Use ON UPDATE columns if specified.
		} else if includeOrdinary && kind == cat.Ordinary {
			// Include Ordinary columns if indicated.
		} else {
			// Wrong kind.
			continue
		}
		if tabCol.IsComputed() {
			continue
		}
		// Skip columns that are already specified.
		if colIDs[i] != 0 {
			continue
		}

		// Use ON UPDATE expression if specified, default otherwise
		tabColID := mb.tabID.ColumnID(i)
		var mutationSuffix string
		var expr tree.Expr
		if tabCol.UseOnUpdate(mb.b.evalCtx.SessionData()) && applyOnUpdate {
			mutationSuffix = "on_update"
			expr = mb.parseOnUpdateExpr(tabColID)
		} else {
			mutationSuffix = "default"
			expr = mb.parseDefaultExpr(tabColID)
		}

		// Add synthesized column. It is important to use the real column
		// reference name, as this column may later be referred to by a computed
		// column.
		colName := scopeColName(tabCol.ColName()).WithMetadataName(
			string(tabCol.ColName()) + "_" + mutationSuffix,
		)
		newCol, _ := pb.Add(colName, expr, tabCol.DatumType())

		// Remember id of newly synthesized column.
		colIDs[i] = newCol

		// Track columns that were not explicitly set in the insert statement.
		if mb.b.trackSchemaDeps {
			mb.implicitInsertCols.Add(newCol)
		}

		// Add corresponding target column.
		mb.targetColList = append(mb.targetColList, tabColID)
		mb.targetColSet.Add(tabColID)
	}

	mb.outScope = pb.Finish()
}

// addSynthesizedComputedCols is a helper method for addSynthesizedColsForInsert
// and addSynthesizedColsForUpdate that scans the list of table columns, looking
// for any that are computed and do not yet have values provided by the input
// expression. New columns are synthesized for any missing columns using the
// computed column expression.
//
// NOTE: colIDs is updated with the column IDs of any synthesized columns which
// are added to mb.outScope. If restrict is true, only columns that depend on
// columns that were already in the list (plus all write-only columns) are
// updated.
func (mb *mutationBuilder) addSynthesizedComputedCols(colIDs opt.OptionalColList, restrict bool) {
	// We will construct a new Project operator that will contain the newly
	// synthesized column(s).
	pb := makeProjectionBuilder(mb.b, mb.outScope)
	var updatedColSet opt.ColSet
	if restrict {
		updatedColSet = colIDs.ToSet()
	}

	for i, n := 0, mb.tab.ColumnCount(); i < n; i++ {
		tabCol := mb.tab.Column(i)
		kind := tabCol.Kind()
		if kind != cat.Ordinary && kind != cat.WriteOnly {
			// Wrong kind.
			continue
		}
		if !tabCol.IsComputed() {
			continue
		}

		// Skip columns that are already specified (this is possible for upserts).
		if colIDs[i] != 0 {
			continue
		}

		// Create a new scope for resolving column references in computed column
		// expressions. We cannot use mb.outScope because columns in that scope
		// may be ambiguous, by design. We build a scope that contains a single
		// column for each column in the target table, representing either an
		// existing value (a column from mb.fetchColIDs) or a new value (a
		// column from mb.upsertColIDs, mb.updateColIDs, or mb.insertColIDs).
		if !pb.HasResolveScope() {
			pb.SetResolveScope(mb.computedColumnScope())
		}

		tabColID := mb.tabID.ColumnID(i)
		expr := mb.parseComputedExpr(tabColID)

		// Add synthesized column.
		colName := scopeColName(tabCol.ColName()).WithMetadataName(
			string(tabCol.ColName()) + "_comp",
		)
		newCol, scalar := pb.Add(colName, expr, tabCol.DatumType())

		if restrict && kind != cat.WriteOnly {
			// Check if any of the columns referred to in the computed column
			// expression are being updated.
			var refCols opt.ColSet
			if scalar == nil {
				// When the expression is a simple column reference, we don't build a
				// new scalar; we just use the same column ID.
				refCols.Add(newCol)
			} else {
				var p props.Shared
				memo.BuildSharedProps(scalar, &p, mb.b.evalCtx)
				refCols = p.OuterCols
			}
			if !refCols.Intersects(updatedColSet) {
				// Normalization rules will clean up the unnecessary projection.
				continue
			}
		}

		// Remember id of newly synthesized column.
		colIDs[i] = newCol

		// Track columns that were not explicitly set in the insert statement.
		if mb.b.trackSchemaDeps && mb.b.evalCtx.SessionData().UseImprovedRoutineDependencyTracking {
			mb.implicitInsertCols.Add(newCol)
		}

		// Add corresponding target column.
		mb.targetColList = append(mb.targetColList, tabColID)
		mb.targetColSet.Add(tabColID)
	}

	mb.outScope = pb.Finish()
}

// maybeAddRegionColLookup adds a lookup join to the target table of a foreign
// key constraint specified by the "infer_rbr_region_col_using_constraint"
// storage param, if any. It is used by INSERT, UPDATE, and UPSERT statements to
// determine the correct value of the region column for a REGIONAL BY ROW table.
func (mb *mutationBuilder) maybeAddRegionColLookup(op opt.Operator) {
	switch op {
	case opt.InsertOp, opt.UpdateOp, opt.UpsertOp:
	default:
		panic(errors.AssertionFailedf("maybeAddRegionColLookup called with unexpected operator %s", op))
	}
	if !mb.tab.IsRegionalByRow() {
		return
	}
	if mb.regionColExplicitlyMutated {
		// Allow the user to explicitly set the region column value, overriding the
		// storage param.
		return
	}
	lookupFK := mb.tab.RegionalByRowUsingConstraint()
	if lookupFK == nil {
		return
	}
	// An UPDATE may not be mutating any of the foreign-key columns, in which case
	// we can stop early.
	if op == opt.UpdateOp {
		fkColIsMutated := false
		for colIdx := range lookupFK.ColumnCount() {
			if mb.updateColIDs[lookupFK.OriginColumnOrdinal(mb.tab, colIdx)] != 0 {
				fkColIsMutated = true
				break
			}
		}
		if !fkColIsMutated {
			return
		}
	}
	// Resolve the referenced table.
	refTabDescID := int64(lookupFK.ReferencedTableID())
	refTab := mb.b.resolveTableRef(&tree.TableRef{TableID: refTabDescID}, privilege.SELECT)
	refTabMeta := mb.b.addTable(refTab, tree.NewUnqualifiedTableName(refTab.Name()))
	refTabID := refTabMeta.MetaID

	// Use the foreign-key columns (apart from the region column, if present) to
	// plan a join against the referenced table. The schema changer has already
	// verified that the foreign-key contains the region column, so performing a
	// lookup using the remaining columns allows us to infer the correct value for
	// the region column.
	//
	// NOTE: The region column is always the first column in the primary index.
	f := mb.b.factory
	joinCond := make(memo.FiltersExpr, 0, lookupFK.ColumnCount())
	originRegionColOrd := mb.tab.Index(cat.PrimaryIndex).Column(0).Ordinal()
	var refLookupCols opt.ColSet
	var originRegionColID, lookupRegionColID opt.ColumnID
	for colIdx := range lookupFK.ColumnCount() {
		originColID := mb.mapToReturnColID(lookupFK.OriginColumnOrdinal(mb.tab, colIdx))
		refColID := refTabID.ColumnID(lookupFK.ReferencedColumnOrdinal(refTab, colIdx))
		if lookupFK.OriginColumnOrdinal(mb.tab, colIdx) == originRegionColOrd {
			originRegionColID = originColID
			lookupRegionColID = refColID
			continue
		}
		eqExpr := f.ConstructEq(f.ConstructVariable(originColID), f.ConstructVariable(refColID))
		joinCond = append(joinCond, f.ConstructFiltersItem(eqExpr))
		refLookupCols.Add(refColID)
	}
	if len(joinCond) == 0 {
		panic(errors.AssertionFailedf(
			"unable to determine lookup columns using constraint %q", lookupFK.Name()))
	}
	if originRegionColID == 0 || lookupRegionColID == 0 {
		panic(errors.AssertionFailedf(
			"expected region column to be part of foreign key constraint %q", lookupFK.Name()))
	}
	md := mb.b.factory.Metadata()
	if !md.ColumnMeta(originRegionColID).Type.Identical(md.ColumnMeta(lookupRegionColID).Type) {
		panic(errors.AssertionFailedf("expected parent and child region column types to be identical"))
	}
	// For non-serializable isolation (or when the var is set), take a shared lock
	// when reading from the parent table. This prevents concurrent transactions
	// from invalidating the looked-up region column value. This isn't necessary
	// for correctness since FK checks still run, but avoids returning an error
	// unnecessarily to the user.
	locking := noRowLocking
	if mb.b.evalCtx.TxnIsoLevel != isolation.Serializable ||
		mb.b.evalCtx.SessionData().ImplicitFKLockingForSerializable {
		locking = lockingSpec{
			&lockingItem{
				item: &tree.LockingItem{
					Strength:   tree.ForShare,
					WaitPolicy: tree.LockWaitBlock,
				},
			},
		}
	}
	refScope := mb.b.buildScan(
		refTabMeta,
		tableOrdinals(refTab, columnKinds{
			includeMutations: false,
			includeSystem:    false,
			includeInverted:  false,
		}),
		&tree.IndexFlags{
			IgnoreForeignKeys: true,
			AvoidFullScan:     mb.b.evalCtx.SessionData().AvoidFullTableScansInMutations,
		},
		locking,
		mb.b.allocScope(),
		true, /* disableNotVisibleIndex */
		// The scan is exempt from RLS to maintain data integrity.
		cat.PolicyScopeExempt,
	)
	if !refScope.expr.Relational().FuncDeps.ColsAreLaxKey(refLookupCols) {
		// The lookup columns must be a lax key, otherwise the join may return
		// multiple rows for a single row in the target table. This should already
		// be enforced by the foreign-key constraint.
		panic(errors.AssertionFailedf(
			"lookup columns using constraint %q must be a lax key", lookupFK.Name()))
	}
	var joinFlags memo.JoinFlags
	if mb.b.evalCtx.SessionData().PreferLookupJoinsForFKs {
		joinFlags = memo.PreferLookupJoinIntoRight
	}
	mb.outScope.expr = mb.b.factory.ConstructLeftJoin(
		mb.outScope.expr, refScope.expr, joinCond, &memo.JoinPrivate{Flags: joinFlags},
	)
	// Build a CASE expression to determine the final value of the region column.
	// Use the looked-up value if non-NULL, and otherwise use the default value
	// which was already projected in the input.
	regionColType := md.ColumnMeta(originRegionColID).Type
	caseExpr := mb.b.factory.ConstructCase(
		memo.TrueSingleton,
		memo.ScalarListExpr{
			f.ConstructWhen(
				f.ConstructIs(f.ConstructVariable(lookupRegionColID), f.ConstructNull(regionColType)),
				f.ConstructVariable(originRegionColID),
			)},
		f.ConstructVariable(lookupRegionColID),
	)
	regionColName := mb.tab.Column(originRegionColOrd).ColName()
	colName := scopeColName(regionColName).WithMetadataName(
		fmt.Sprintf("fk_lookup_%s", regionColName),
	)
	newOutScope := mb.outScope.replace()
	newOutScope.appendColumnsFromScope(mb.outScope)
	regionCol := mb.b.synthesizeColumn(newOutScope, colName, regionColType, nil /* expr */, caseExpr)
	mb.b.constructProjectForScope(mb.outScope, newOutScope)
	mb.outScope = newOutScope

	// Whether a row is inserted or updated, it will use the newly calculated
	// value for the region column.
	if op == opt.InsertOp || op == opt.UpsertOp {
		mb.insertColIDs[originRegionColOrd] = regionCol.id
	}
	if op == opt.UpdateOp || op == opt.UpsertOp {
		mb.updateColIDs[originRegionColOrd] = regionCol.id
	}
}

// addCheckConstraintCols synthesizes a boolean output column for each check
// constraint defined on the target table. The mutation operator will report a
// constraint violation error if the value of the column is false.
//
// Synthesized check columns are not necessary for UPDATE mutations if the
// columns referenced in the check expression are not being mutated. If isUpdate
// is true, check columns that do not reference mutation columns are not added
// to checkColIDs, which allows pruning normalization rules to remove the
// unnecessary projected column.
func (mb *mutationBuilder) addCheckConstraintCols(
	isUpdate bool, policyCmdScope cat.PolicyCommandScope, includeSelectPolicies bool,
) {
	if mb.tab.CheckCount() != 0 {
		projectionsScope := mb.outScope.replace()
		projectionsScope.appendColumnsFromScope(mb.outScope)
		mutationCols := mb.mutationColumnIDs()
		var seenRLSConstraint bool

		for i, n := 0, mb.tab.CheckCount(); i < n; i++ {
			check := mb.tab.Check(i)

			referencedCols := &opt.ColSet{}
			var scopeCol *scopeColumn

			// For tables with RLS enabled, we create a synthetic check constraint
			// to enforce the policies. Since this check varies based on the role
			// and command used, it must be generated each time it is needed rather
			// than being included with the table's actual check constraints.
			if check.IsRLSConstraint() {
				if seenRLSConstraint {
					panic(errors.AssertionFailedf("a table should only have one RLS constraint"))
				}
				seenRLSConstraint = true

				var rlsScalar opt.ScalarExpr
				rlsScalar, check = mb.buildRLSCheckConstraint(policyCmdScope, includeSelectPolicies, referencedCols)
				colName := scopeColName("").WithMetadataName("rls")
				scopeCol = mb.b.synthesizeColumn(projectionsScope, colName, rlsScalar.DataType(), nil /* expr */, rlsScalar)
			} else {
				expr, err := parser.ParseExpr(check.Constraint())
				if err != nil {
					panic(err)
				}

				texpr := mb.outScope.resolveAndRequireType(expr, types.Bool)

				// Use an anonymous name because the column cannot be referenced
				// in other expressions.
				colName := scopeColName("").WithMetadataName(fmt.Sprintf("check%d", i+1))
				scopeCol = projectionsScope.addColumn(colName, texpr)

				// TODO(ridwanmsharif): Maybe we can avoid building constraints here
				// and instead use the constraints stored in the table metadata.
				mb.b.buildScalar(texpr, mb.outScope, projectionsScope, scopeCol, referencedCols)
			}

			// For non-UPDATE mutations, track the synthesized check columns in
			// checkColIDs. For UPDATE mutations, track the check columns in two
			// scenarios:
			// - If the check expression is a real check constraint and the columns
			//   referenced in the check expression are being mutated.
			// - If the check expression is a synthetic one used for row-level
			//   security (RLS). Since it's not a real check expression, different
			//   expressions can exist for read and write operations. This means it's
			//   possible to read a row whose column values would violate the write
			//   expression.
			if !isUpdate || check.IsRLSConstraint() || referencedCols.Intersects(mutationCols) {
				mb.checkColIDs[i] = scopeCol.id

				// TODO(michae2): Under weaker isolation levels we need to use shared
				// locking to enforce multi-column-family check constraints. Disallow it
				// for now.
				//
				// When do we need the locking? If:
				// - The check constraint involves a column family that is updated
				//   (otherwise we don't need to do anything to maintain this constraint)
				// - And the check constraint involves a column family that is *not*
				//   updated, but *is* read. In this case we don't have an intent, so
				//   we need a lock. But we're not currently taking that lock.
				if mb.b.evalCtx.TxnIsoLevel != isolation.Serializable {
					// Find the columns referenced in the check constraint that are being
					// read and updated.
					var readColOrds, updateColOrds intsets.Fast
					for j, n := 0, check.ColumnCount(); j < n; j++ {
						ord := check.ColumnOrdinal(j)
						if mb.fetchColIDs[ord] != 0 {
							readColOrds.Add(ord)
						}
						if mb.updateColIDs[ord] != 0 {
							updateColOrds.Add(ord)
						}
					}
					// If some of the check constraint column families are being updated
					// but others are only being read, return an error.
					if updateColOrds.Len() > 0 {
						readColFamilies := getColumnFamilySet(readColOrds, mb.tab)
						updateColFamilies := getColumnFamilySet(updateColOrds, mb.tab)
						if readColFamilies.Difference(updateColFamilies).Len() > 0 {
							panic(unimplemented.NewWithIssuef(112488,
								"multi-column-family check constraints are not yet supported under read committed isolation",
							))
						}
					}
				}
			}
		}

		mb.b.constructProjectForScope(mb.outScope, projectionsScope)
		mb.outScope = projectionsScope
	}
}

// buildRLSCheckConstraint returns a RLS specific check constraint that is used
// to enforce the policies on write.
func (mb *mutationBuilder) buildRLSCheckConstraint(
	cmdScope cat.PolicyCommandScope, includeSelectPolicies bool, referencedCols *opt.ColSet,
) (opt.ScalarExpr, *rlsCheckConstraint) {
	tabMeta := mb.md.TableMeta(mb.tabID)
	scalar := mb.buildRLSCheckExpr(tabMeta, cmdScope, includeSelectPolicies, referencedCols)

	// Build a CheckConstraint so the caller knows what columns were referenced.
	check := rlsCheckConstraint{
		colIDs: mb.b.getColIDsFromPoliciesUsed(tabMeta),
		tab:    mb.tab,
	}
	return scalar, &check
}

// buildRLSCheckExpr constructs the scalar expression that enforces row-level
// security (RLS) policies via a synthetic check constraint. The resulting
// expression is used during data mutation operations (e.g., INSERT, UPDATE, UPSERT).
//
// The includeSelectPolicies parameter controls whether SELECT policies are also
// enforced in the check constraint:
//   - For INSERT: if set, SELECT policies are applied to the newly inserted rows
//     (e.g., for INSERT ... RETURNING to ensure returned rows are visible).
//   - For UPDATE: if set, SELECT policies are applied if any SET clause, WHERE clause,
//     or RETURNING clause references a column from the table (i.e., when existing
//     rows need to be checked for visibility).
//   - For UPSERT: this parameter is ignored because UPSERT enforces SELECT policies
//     internally based on conflict detection.
//
// The referencedCols is updated to reflect the columns that are referenced in
// all applied policy expressions.
func (mb *mutationBuilder) buildRLSCheckExpr(
	tabMeta *opt.TableMeta,
	cmdScope cat.PolicyCommandScope,
	includeSelectPolicies bool,
	referencedCols *opt.ColSet,
) opt.ScalarExpr {
	if mb.b.isExemptFromRLSPolicies(tabMeta, cmdScope) {
		return memo.TrueSingleton
	}

	var scalar opt.ScalarExpr
	switch cmdScope {
	case cat.PolicyScopeInsert:
		scalar = mb.genPolicyWithCheckExpr(tabMeta, cat.PolicyScopeInsert, referencedCols)
		// Only apply select policies if requested.
		if includeSelectPolicies {
			// Note: we use mb.outScope because we want the policies applied to the newly
			// inserted rows. For example, INSERT ... RETURNING must ensure the returned
			// rows are visible.
			scalar = mb.b.factory.ConstructAnd(
				mb.genPolicyUsingExpr(tabMeta, cat.PolicyScopeSelect, mb.outScope, referencedCols),
				scalar,
			)
		}
	case cat.PolicyScopeUpdate:
		scalar = mb.genPolicyWithCheckExpr(tabMeta, cat.PolicyScopeUpdate, referencedCols)
		// Only apply select policies if requested.
		if includeSelectPolicies {
			scalar = mb.b.factory.ConstructAnd(
				mb.genPolicyUsingExpr(tabMeta, cat.PolicyScopeSelect, mb.outScope, referencedCols),
				scalar,
			)
		}
	case cat.PolicyScopeUpsert:
		// For UPSERT, the applied RLS policies depend on whether the operation results in
		// an INSERT or an UPDATE. We determine this by checking if the canary column is NULL:
		//   - If it IS NULL → no conflict occurred → this is an INSERT
		//   - If it is NOT NULL → conflict occurred → this is an UPDATE
		//
		// The expression below enforces:
		//   - On conflict (UPDATE):
		//       * SELECT + UPDATE policies on the existing row (fetchScope)
		//       * SELECT + UPDATE policies on the updated row (outScope)
		//   - On no conflict (INSERT):
		//       * SELECT + INSERT policies on the inserted row (outScope)
		//
		// This is expressed as:
		//   (isConflict AND all UPDATE-related policies)
		//   OR
		//   (isNotConflict AND all INSERT-related policies)
		isNotConflict := mb.b.factory.ConstructIs(
			mb.b.factory.ConstructVariable(mb.canaryColID),
			memo.NullSingleton,
		)
		isConflict := mb.b.factory.ConstructNot(isNotConflict)
		scalar = mb.b.factory.ConstructOr(
			// CASE 1: apply all UPDATE-related policies. Note: we use mb.fetchScope
			// to apply policies against columns fetched during conflict detection.
			// We don't filter out rows that violate SELECT policies (as we would in
			// a normal query), because we want the UPSERT to fail if a conflict occurs
			// but the user does not have visibility into the conflicting row.
			mb.b.factory.ConstructAnd(
				isConflict,
				mb.b.factory.ConstructAnd(
					mb.genPolicyUsingExpr(tabMeta, cat.PolicyScopeSelect, mb.fetchScope, referencedCols),
					mb.b.factory.ConstructAnd(
						mb.genPolicyUsingExpr(tabMeta, cat.PolicyScopeUpdate, mb.fetchScope, referencedCols),
						mb.b.factory.ConstructAnd(
							mb.genPolicyUsingExpr(tabMeta, cat.PolicyScopeSelect, mb.outScope, referencedCols),
							mb.genPolicyWithCheckExpr(tabMeta, cat.PolicyScopeUpdate, referencedCols),
						),
					),
				),
			),
			// CASE 2: apply all INSERT-related policies
			mb.b.factory.ConstructAnd(
				isNotConflict,
				mb.b.factory.ConstructAnd(
					mb.genPolicyUsingExpr(tabMeta, cat.PolicyScopeSelect, mb.outScope, referencedCols),
					mb.genPolicyWithCheckExpr(tabMeta, cat.PolicyScopeInsert, referencedCols),
				),
			),
		)
	default:
		panic(errors.AssertionFailedf("unsupported policy command scope for check expr: %v", cmdScope))
	}

	mb.b.factory.Metadata().GetRLSMeta().RefreshNoPoliciesAppliedForTable(tabMeta.MetaID)
	return scalar
}

// genPolicyWithCheckExpr will build a WITH CHECK expression for the
// given policy command. If no policy applies, then the 'false' expression is
// returned.
func (mb *mutationBuilder) genPolicyWithCheckExpr(
	tabMeta *opt.TableMeta, cmdScope cat.PolicyCommandScope, referencedCols *opt.ColSet,
) opt.ScalarExpr {
	scalar := mb.genPolicyExpr(tabMeta, cmdScope, mb.outScope, referencedCols, false /* forceUsingExpr */)
	if scalar == nil {
		return memo.FalseSingleton
	}
	return scalar
}

// genPolicyUsingExpr generates a USING expression for the given policy command.
// If no applicable policies are found, it returns 'false'. Otherwise, it returns
// the generated scalar expression.
func (mb *mutationBuilder) genPolicyUsingExpr(
	tabMeta *opt.TableMeta,
	cmdScope cat.PolicyCommandScope,
	exprScope *scope,
	referencedCols *opt.ColSet,
) opt.ScalarExpr {
	scalar := mb.genPolicyExpr(tabMeta, cmdScope, exprScope, referencedCols, true /* forceUsingExpr */)
	if scalar == nil {
		return memo.FalseSingleton
	}
	return scalar
}

// genPolicyExpr constructs a scalar expression representing the RLS (row-level
// security) policy checks to enforce for a given command scope (INSERT, UPDATE,
// etc.).
//
// Typically, RLS policies are enforced using the WITH CHECK expression, which
// ensures that written rows comply with the defined policies. However, in
// certain scenarios, the USING expression is used instead—most notably during
// conflict resolution in UPSERTs. In those cases, we don't filter out invisible
// rows during scans; instead, we enforce visibility by requiring the row to
// satisfy the USING expression. If it doesn't, the statement fails via a
// constraint violation.
//
// The `forceUsingExpr` flag controls this behaviour:
//   - If false: the WITH CHECK expression is used (if present).
//   - If true: the USING expression is used instead, even if a WITH CHECK
//     expression is defined.
//
// This function returns a scalar expression composed of all applicable policies
// (both permissive and restrictive), and records which policies were applied in
// the RLS metadata.
//
// The final expression has the form:
//
//	(permissive1 OR permissive2 OR ...) AND restrictive1 AND restrictive2 AND ...
//
// This structure allows permissive policies to grant access if *any* are
// satisfied, while all restrictive policies must be satisfied to allow the
// operation.
func (mb *mutationBuilder) genPolicyExpr(
	tabMeta *opt.TableMeta,
	cmdScope cat.PolicyCommandScope,
	exprScope *scope,
	referencedCols *opt.ColSet,
	forceUsingExpr bool,
) opt.ScalarExpr {
	var scalar opt.ScalarExpr
	var policiesUsed opt.PolicyIDSet
	policies := tabMeta.Table.Policies()

	// Create a closure to handle building the expression for one policy.
	buildForPolicy := func(p cat.Policy, combineScalars func(opt.ScalarExpr, opt.ScalarExpr) opt.ScalarExpr) {
		if !p.AppliesToRole(mb.b.ctx, mb.b.catalog, mb.b.checkPrivilegeUser) || !policyAppliesToCommandScope(p, cmdScope) {
			return
		}
		policiesUsed.Add(p.ID)

		expr := p.WithCheckExpr
		if expr == "" || forceUsingExpr {
			// The USING expression is used in two scenarios:
			// - When the WITH CHECK expression is not defined
			// - When the caller explicitly requests only the USING expression (e.g.,
			// during UPSERT)
			expr = p.UsingExpr
		}
		if expr == "" {
			// If both expressions are missing, the policy does not apply and can
			// be skipped.
			return
		}
		pexpr, err := parser.ParseExpr(expr)
		if err != nil {
			panic(err)
		}
		texpr := exprScope.resolveAndRequireType(pexpr, types.Bool)
		singleExprScalar := mb.b.buildScalar(texpr, mb.outScope, nil, nil, referencedCols)

		// Build up a scalar expression of all singleExprScalar's combined.
		if scalar != nil {
			scalar = combineScalars(scalar, singleExprScalar)
		} else {
			scalar = singleExprScalar
		}
	}

	for _, policy := range policies.Permissive {
		buildForPolicy(policy, mb.b.factory.ConstructOr)
	}
	// If no permissive policies apply, then we will add a false check as
	// nothing is allowed to be written.
	if scalar == nil {
		return memo.FalseSingleton
	}
	for _, policy := range policies.Restrictive {
		buildForPolicy(policy, mb.b.factory.ConstructAnd)
	}

	if scalar == nil {
		panic(errors.AssertionFailedf("at least one applicable policy should have been included"))
	}
	mb.b.factory.Metadata().GetRLSMeta().AddPoliciesUsed(tabMeta.MetaID, policiesUsed, false /* applyFilterExpr */)
	return scalar
}

// getColumnFamilySet gets the set of column families represented in colOrdinals.
func getColumnFamilySet(colOrdinals intsets.Fast, tab cat.Table) intsets.Fast {
	families := intsets.Fast{}
	for i := 0; i < tab.FamilyCount(); i++ {
		fam := tab.Family(i)
		for j := 0; j < fam.ColumnCount(); j++ {
			if colOrdinals.Contains(fam.Column(j).Ordinal) {
				families.Add(i)
			}
		}
	}
	return families
}

// mutationColumnIDs returns the set of all column IDs that will be mutated.
func (mb *mutationBuilder) mutationColumnIDs() opt.ColSet {
	cols := opt.ColSet{}
	for _, col := range mb.insertColIDs {
		if col != 0 {
			cols.Add(col)
		}
	}
	for _, col := range mb.updateColIDs {
		if col != 0 {
			cols.Add(col)
		}
	}
	for _, col := range mb.upsertColIDs {
		if col != 0 {
			cols.Add(col)
		}
	}
	return cols
}

// projectPartialIndexPutCols builds a Project that synthesizes boolean PUT
// columns for each partial index defined on the target table. See
// partialIndexPutColIDs for more info on these columns.
func (mb *mutationBuilder) projectPartialIndexPutCols() {
	mb.projectPartialIndexColsImpl(mb.outScope, nil /* delScope */)
}

// projectPartialIndexDelCols builds a Project that synthesizes boolean DEL
// columns for each partial index defined on the target table. See
// partialIndexDelColIDs for more info on these columns.
func (mb *mutationBuilder) projectPartialIndexDelCols() {
	mb.projectPartialIndexColsImpl(nil /* putScope */, mb.fetchScope)
}

// projectPartialIndexPutAndDelCols builds a Project that synthesizes boolean
// PUT and DEL columns for each partial index defined on the target table. See
// partialIndexPutColIDs and partialIndexDelColIDs for more info on these
// columns.
func (mb *mutationBuilder) projectPartialIndexPutAndDelCols() {
	mb.projectPartialIndexColsImpl(mb.outScope, mb.fetchScope)
}

// projectPartialIndexColsImpl builds a Project that synthesizes boolean PUT and
// DEL columns  for each partial index defined on the target table. PUT columns
// are only projected if putScope is non-nil and DEL columns are only projected
// if delScope is non-nil.
//
// NOTE: This function should only be called via projectPartialIndexPutCols,
// projectPartialIndexDelCols, or projectPartialIndexPutAndDelCols.
func (mb *mutationBuilder) projectPartialIndexColsImpl(putScope, delScope *scope) {
	if partialIndexCount(mb.tab) > 0 {
		projectionScope := mb.outScope.replace()
		projectionScope.appendColumnsFromScope(mb.outScope)

		ord := 0
		for i, n := 0, mb.tab.DeletableIndexCount(); i < n; i++ {
			index := mb.tab.Index(i)

			// Skip non-partial indexes.
			if _, isPartial := index.Predicate(); !isPartial {
				continue
			}

			expr := mb.parsePartialIndexPredicateExpr(i)

			// Build synthesized PUT columns.
			if putScope != nil {
				texpr := putScope.resolveAndRequireType(expr, types.Bool)

				// Use an anonymous name because the column cannot be referenced
				// in other expressions.
				colName := scopeColName("").WithMetadataName(fmt.Sprintf("partial_index_put%d", ord+1))
				scopeCol := projectionScope.addColumn(colName, texpr)

				mb.b.buildScalar(texpr, putScope, projectionScope, scopeCol, nil)
				mb.partialIndexPutColIDs[ord] = scopeCol.id
			}

			// Build synthesized DEL columns.
			if delScope != nil {
				texpr := delScope.resolveAndRequireType(expr, types.Bool)

				// Use an anonymous name because the column cannot be referenced
				// in other expressions.
				colName := scopeColName("").WithMetadataName(fmt.Sprintf("partial_index_del%d", ord+1))
				scopeCol := projectionScope.addColumn(colName, texpr)

				mb.b.buildScalar(texpr, delScope, projectionScope, scopeCol, nil)
				mb.partialIndexDelColIDs[ord] = scopeCol.id
			}

			ord++
		}

		mb.b.constructProjectForScope(mb.outScope, projectionScope)
		mb.outScope = projectionScope
	}
}

// projectVectorIndexColsForInsert builds VectorMutationSearch operators for the input
// of an INSERT mutation. See projectVectorIndexColsImpl for details.
func (mb *mutationBuilder) projectVectorIndexColsForInsert() {
	mb.projectVectorIndexColsImpl(opt.InsertOp /* op */)

	// Execution expects each list to have one entry for each vector index. Ensure
	// this is the case by projecting NULL values as necessary.
	mb.replaceUnsetColsWithNulls(mb.vectorIndexPutPartitionColIDs)
	mb.replaceUnsetColsWithNulls(mb.vectorIndexPutQuantizedVecColIDs)
}

// projectVectorIndexColsForUpsert builds VectorMutationSearch operators for the input
// of an UPSERT mutation. See projectVectorIndexColsImpl for details.
func (mb *mutationBuilder) projectVectorIndexColsForUpsert() {
	mb.projectVectorIndexColsImpl(opt.UpsertOp /* op */)

	// Execution expects each list to have one entry for each vector index. Ensure
	// this is the case by projecting NULL values as necessary.
	mb.replaceUnsetColsWithNulls(mb.vectorIndexPutPartitionColIDs)
	mb.replaceUnsetColsWithNulls(mb.vectorIndexPutQuantizedVecColIDs)
	mb.replaceUnsetColsWithNulls(mb.vectorIndexDelPartitionColIDs)
}

// projectVectorIndexColsForUpdate builds VectorMutationSearch operators for the input
// of an UPDATE mutation. See projectVectorIndexColsImpl for details.
func (mb *mutationBuilder) projectVectorIndexColsForUpdate() {
	mb.projectVectorIndexColsImpl(opt.UpdateOp /* op */)

	// Execution expects each list to have one entry for each vector index. Ensure
	// this is the case by projecting NULL values as necessary.
	mb.replaceUnsetColsWithNulls(mb.vectorIndexPutPartitionColIDs)
	mb.replaceUnsetColsWithNulls(mb.vectorIndexPutQuantizedVecColIDs)
	mb.replaceUnsetColsWithNulls(mb.vectorIndexDelPartitionColIDs)
}

// projectVectorIndexColsForDelete builds VectorMutationSearch operators for the
// input of a DELETE mutation. See projectVectorIndexColsImpl for details.
func (mb *mutationBuilder) projectVectorIndexColsForDelete() {
	mb.projectVectorIndexColsImpl(opt.DeleteOp /* op */)

	// Execution expects each list to have one entry for each vector index. Ensure
	// this is the case by projecting NULL values as necessary.
	mb.replaceUnsetColsWithNulls(mb.vectorIndexDelPartitionColIDs)
}

// replaceUnsetColsWithNulls checks the given OptionalColList for unset column
// IDs, and replaces any found with a new column that projects a NULL value.
func (mb *mutationBuilder) replaceUnsetColsWithNulls(cols opt.OptionalColList) {
	// We will construct a new Project operator that will contain the newly
	// synthesized column(s).
	pb := makeProjectionBuilder(mb.b, mb.outScope)

	for i, colID := range cols {
		if colID == 0 {
			// Add synthesized column that projects a NULL value. Update the cols list
			// to include the new column ID.
			colName := scopeColName("").WithMetadataName(fmt.Sprintf("null%d", i+1))
			cols[i], _ = pb.Add(colName, tree.DNull, types.Unknown)
		}
	}

	mb.outScope = pb.Finish()
}

// projectVectorIndexColsImpl builds VectorMutationSearch operators that project
// partitions to be the target of index insertions and deletions for each vector
// index defined on the target table. This is needed because vector indexes must
// perform a search to determine which partition a given vector belongs to.
func (mb *mutationBuilder) projectVectorIndexColsImpl(op opt.Operator) {
	if vectorIndexCount(mb.tab) > 0 {
		addCol := func(name string, typ *types.T) opt.ColumnID {
			colName := scopeColName("").WithMetadataName(name)
			sc := mb.b.synthesizeColumn(mb.outScope, colName, typ, nil /* expr */, nil /* expr */)
			return sc.id
		}
		idxOrd := 0
		for i := range mb.tab.DeletableIndexCount() {
			index := mb.tab.Index(i)

			// Skip non-vector indexes.
			if index.Type() != idxtype.VECTOR {
				continue
			}

			// Determine whether index PUT and DEL operations will be necessary.
			indexColIsUpdated := false
			if op == opt.UpsertOp || op == opt.UpdateOp {
				// UPSERT and UPDATE statements can target specific columns for update.
				// Check if any columns from the index are being updated.
				for colIndexOrd := 0; colIndexOrd < index.ColumnCount(); colIndexOrd++ {
					colTableOrd := index.Column(colIndexOrd).Ordinal()
					if mb.upsertColIDs[colTableOrd] != 0 || mb.updateColIDs[colTableOrd] != 0 {
						indexColIsUpdated = true
						break
					}
				}
			}
			// It is possible for a vector index to be the target of both PUT and DEL
			// operations, in which case two search operators are needed in order to
			// locate the old index entry, as well as the partition for the new one.
			//
			// TODO(drewk): we may be able to avoid the DEL for updates to stored
			// columns (once they're supported).
			if op == opt.DeleteOp || indexColIsUpdated {
				const isIndexPut = false
				partitionCol := addCol(fmt.Sprintf("vector_index_del_partition%d", idxOrd+1), types.Int)
				mb.outScope.expr = mb.buildVectorMutationSearch(
					mb.outScope.expr, index, partitionCol, 0 /* encVectorCol */, isIndexPut,
				)
				mb.vectorIndexDelPartitionColIDs[idxOrd] = partitionCol
			}
			if op == opt.InsertOp || op == opt.UpsertOp || indexColIsUpdated {
				const isIndexPut = true
				partitionCol := addCol(fmt.Sprintf("vector_index_put_partition%d", idxOrd+1), types.Int)
				quantizedVecCol := addCol(fmt.Sprintf("vector_index_put_quantized_vec%d", idxOrd+1), types.Bytes)
				mb.outScope.expr = mb.buildVectorMutationSearch(
					mb.outScope.expr, index, partitionCol, quantizedVecCol, isIndexPut,
				)
				mb.vectorIndexPutPartitionColIDs[idxOrd] = partitionCol
				mb.vectorIndexPutQuantizedVecColIDs[idxOrd] = quantizedVecCol
			}
			idxOrd++
		}
	}
}

// buildVectorMutationSearch builds a VectorMutationSearch operator that will
// find the partition (and quantized vector, if requested) for vectors in the
// given queryVectorCol.
func (mb *mutationBuilder) buildVectorMutationSearch(
	input memo.RelExpr, index cat.Index, partitionCol, quantizedVecCol opt.ColumnID, isIndexPut bool,
) memo.RelExpr {
	if index.IsTemporaryIndexForBackfill() {
		panic(unimplemented.NewWithIssue(144443, "Cannot write to a vector index while it is being built"))
	}
	getCol := func(colOrd int) (colID opt.ColumnID) {
		// Check in turn if the column is being upserted, inserted, updated, or
		// fetched.
		if isIndexPut {
			colID = mb.upsertColIDs[colOrd]
			if colID == 0 {
				colID = mb.insertColIDs[colOrd]
			}
			if colID == 0 {
				colID = mb.updateColIDs[colOrd]
			}
		}
		if colID == 0 {
			colID = mb.fetchColIDs[colOrd]
		}
		if colID == 0 {
			panic(errors.AssertionFailedf("column %d not found", colOrd))
		}
		return colID
	}
	prefixCols := make(opt.ColList, 0, index.PrefixColumnCount())
	for colIdx := range index.PrefixColumnCount() {
		prefixCols = append(prefixCols, getCol(index.Column(colIdx).Ordinal()))
	}
	var suffixCols opt.ColList
	if !isIndexPut {
		// Index DEL operations must specify the full key to ensure the correct
		// index entry is deleted.
		suffixStart := index.PrefixColumnCount() + 1
		suffixCols = make(opt.ColList, 0, index.KeyColumnCount()-suffixStart)
		for colIdx := suffixStart; colIdx < index.KeyColumnCount(); colIdx++ {
			suffixCols = append(suffixCols, getCol(index.Column(colIdx).Ordinal()))
		}
	}
	private := memo.VectorMutationSearchPrivate{
		Table:              mb.tabID,
		Index:              index.Ordinal(),
		PrefixKeyCols:      prefixCols,
		QueryVectorCol:     getCol(index.VectorColumn().Ordinal()),
		SuffixKeyCols:      suffixCols,
		PartitionCol:       partitionCol,
		QuantizedVectorCol: quantizedVecCol,
		IsIndexPut:         isIndexPut,
	}
	return mb.b.factory.ConstructVectorMutationSearch(input, &private)
}

// computedColumnScope returns a new scope that can be used to build computed
// column expressions. Columns will never be ambiguous because each column in
// the returned scope maps to a single column in the target table.
//
// The columns included in the scope depend on the state of mb.upsertColIDs,
// mb.updateColIDs, mb.fetchColIDs, and mb.insertColIDs, using the same order of
// preference as disambiguateColumns (see mapToReturnColID). Therefore, this
// function will return different scopes at different stages of building a
// mutation statement. For example, when building the scan portion of an UPDATE,
// the scope will include columns from mb.fetchColIDs, while it will include
// columns from mb.updateColIDs or mb.fetchColIDs when building the SET portion
// of an UPDATE.
func (mb *mutationBuilder) computedColumnScope() *scope {
	s := mb.b.allocScope()
	for i, n := 0, mb.tab.ColumnCount(); i < n; i++ {
		colID := mb.mapToReturnColID(i)
		if colID == 0 {
			continue
		}
		col := mb.outScope.getColumn(colID)
		if col == nil {
			panic(errors.AssertionFailedf("expected to find column %d in scope", colID))
		}
		targetCol := mb.tab.Column(i)
		s.cols = append(s.cols, scopeColumn{
			name: scopeColName(targetCol.ColName()),
			typ:  col.typ,
			id:   col.id,
		})
	}
	return s
}

// disambiguateColumns ranges over the scope and ensures that at most one column
// has each table column name, and that name refers to the column with the final
// value that the mutation applies.
func (mb *mutationBuilder) disambiguateColumns() {
	// Determine the set of input columns that will have their names preserved.
	var preserve opt.ColSet
	for i, n := 0, mb.tab.ColumnCount(); i < n; i++ {
		if colID := mb.mapToReturnColID(i); colID != 0 {
			preserve.Add(colID)
		}
	}

	// Clear names of all non-preserved columns.
	for i := range mb.outScope.cols {
		if !preserve.Contains(mb.outScope.cols[i].id) {
			mb.outScope.cols[i].clearName()
		}
	}
}

// makeMutationPrivate builds a MutationPrivate struct containing the table and
// column metadata needed for the mutation operator.
//
// - vectorInsert indicates that the mutation operator is an Insert with a
// specialized vectorized implementation for Copy.
func (mb *mutationBuilder) makeMutationPrivate(
	needResults, vectorInsert bool,
) *memo.MutationPrivate {
	// Helper function that returns nil if there are no non-zero column IDs in a
	// given list. A zero column ID indicates that column does not participate
	// in this mutation operation.
	checkEmptyList := func(colIDs opt.OptionalColList) opt.OptionalColList {
		if colIDs.IsEmpty() {
			return nil
		}
		return colIDs
	}

	private := &memo.MutationPrivate{
		Table:                          mb.tabID,
		InsertCols:                     checkEmptyList(mb.insertColIDs),
		FetchCols:                      checkEmptyList(mb.fetchColIDs),
		UpdateCols:                     checkEmptyList(mb.updateColIDs),
		CanaryCol:                      mb.canaryColID,
		ArbiterIndexes:                 mb.arbiters.IndexOrdinals(),
		ArbiterConstraints:             mb.arbiters.UniqueConstraintOrdinals(),
		CheckCols:                      checkEmptyList(mb.checkColIDs),
		PartialIndexPutCols:            checkEmptyList(mb.partialIndexPutColIDs),
		PartialIndexDelCols:            checkEmptyList(mb.partialIndexDelColIDs),
		VectorIndexPutPartitionCols:    checkEmptyList(mb.vectorIndexPutPartitionColIDs),
		VectorIndexPutQuantizedVecCols: checkEmptyList(mb.vectorIndexPutQuantizedVecColIDs),
		VectorIndexDelPartitionCols:    checkEmptyList(mb.vectorIndexDelPartitionColIDs),
		TriggerCols:                    mb.triggerColIDs,
		FKCascades:                     mb.cascades,
		AfterTriggers:                  mb.afterTriggers,
		UniqueWithTombstoneIndexes:     mb.uniqueWithTombstoneIndexes.Ordered(),
		VectorInsert:                   vectorInsert,
	}

	// If we didn't actually plan any checks, cascades, or triggers, don't buffer
	// the input.
	if len(mb.uniqueChecks) > 0 || len(mb.fkChecks) > 0 ||
		len(mb.cascades) > 0 || mb.afterTriggers != nil {
		private.WithID = mb.withID
	}

	if needResults {
		private.ReturnCols = make(opt.OptionalColList, mb.tab.ColumnCount())
		for i, n := 0, mb.tab.ColumnCount(); i < n; i++ {
			if kind := mb.tab.Column(i).Kind(); kind != cat.Ordinary {
				// Only non-mutation and non-system columns are output columns.
				continue
			}
			retColID := mb.mapToReturnColID(i)
			if retColID == 0 {
				panic(errors.AssertionFailedf("column %d is not available in the mutation input", i))
			}
			private.ReturnCols[i] = retColID
		}
	}

	return private
}

// mapToReturnColID returns the ID of the input column that provides the final
// value for the column at the given ordinal position in the table. This value
// might mutate the column, or it might be returned by the mutation statement,
// or it might not be used at all. Columns take priority in this order:
//
//	upsert, update, fetch, insert
//
// If an upsert column is available, then it already combines an update/fetch
// value with an insert value, so it takes priority. If an update column is
// available, then it overrides any fetch value. Finally, the relative priority
// of fetch and insert columns doesn't matter, since they're only used together
// in the upsert case where an upsert column would be available.
//
// If the column is never referenced by the statement, then mapToReturnColID
// returns 0. This would be the case for delete-only columns in an Insert
// statement, because they're neither fetched nor mutated.
func (mb *mutationBuilder) mapToReturnColID(tabOrd int) opt.ColumnID {
	switch {
	case mb.upsertColIDs[tabOrd] != 0:
		return mb.upsertColIDs[tabOrd]

	case mb.updateColIDs[tabOrd] != 0:
		return mb.updateColIDs[tabOrd]

	case mb.fetchColIDs[tabOrd] != 0:
		return mb.fetchColIDs[tabOrd]

	case mb.insertColIDs[tabOrd] != 0:
		return mb.insertColIDs[tabOrd]

	default:
		// Column is never referenced by the statement.
		return 0
	}
}

// buildReturning wraps the input expression with a Project operator that
// projects the given RETURNING expressions. The inScope and outScope parameters
// should be built with buildReturningScopes.
func (mb *mutationBuilder) buildReturning(
	returning *tree.ReturningExprs, inScope, outScope *scope,
) {
	// Handle case of no RETURNING clause.
	if returning == nil {
		// Create an empty scope and add the built expression to it.
		expr := mb.outScope.expr
		mb.outScope = mb.b.allocScope()
		mb.outScope.expr = expr
		return
	}

	// Construct the Project operator that projects the RETURNING expressions.
	inScope.expr = mb.outScope.expr
	mb.b.constructProjectForScope(inScope, outScope)
	mb.outScope = outScope
}

// buildReturningScopes builds the input and output scopes for the RETURNING
// clause. If the RETURNING clause is nil, both returned scopes are nil.
func (mb *mutationBuilder) buildReturningScopes(
	returning *tree.ReturningExprs, colRefs *opt.ColSet,
) (inScope, outScope *scope) {
	if returning == nil {
		return nil, nil
	}

	// Start out by constructing a scope containing one column for each non-
	// mutation column in the target table, in the same order, and with the
	// same names. These columns can be referenced by the RETURNING clause.
	//
	//   1. Project only non-mutation columns.
	//   2. Alias columns to use table column names.
	//   3. Mark hidden columns.
	//   4. Project columns in same order as defined in table schema.
	//
	inScope = mb.outScope.replace()
	mb.b.appendOrdinaryColumnsFromTable(inScope, mb.md.TableMeta(mb.tabID), &mb.alias)

	// extraAccessibleCols contains all the columns that the RETURNING
	// clause can refer to in addition to the table columns. This is useful for
	// UPDATE ... FROM and DELETE ... USING statements, where all columns from
	// tables in the FROM clause and USING clause are in scope for the RETURNING
	// clause, respectively.
	inScope.appendColumns(mb.extraAccessibleCols)

	// Build the projections of the RETURNING expressions.
	outScope = inScope.replace()
	mb.b.analyzeReturningList(returning, nil /* desiredTypes */, inScope, outScope)
	mb.b.buildProjectionList(inScope, outScope, colRefs)
	return inScope, outScope
}

// checkNumCols raises an error if the expected number of columns does not match
// the actual number of columns.
func (mb *mutationBuilder) checkNumCols(expected, actual int) {
	if actual != expected {
		more, less := "expressions", "target columns"
		if actual < expected {
			more, less = less, more
		}

		panic(pgerror.Newf(pgcode.Syntax,
			"%s has more %s than %s, %d expressions for %d targets",
			strings.ToUpper(mb.opName), more, less, actual, expected))
	}
}

// parseComputedExpr parses the computed expression for the given table column,
// and caches it for reuse.
func (mb *mutationBuilder) parseComputedExpr(colID opt.ColumnID) tree.Expr {
	if mb.parsedColComputedExprs == nil {
		mb.parsedColComputedExprs = make([]tree.Expr, mb.tab.ColumnCount())
	}

	ord := mb.tabID.ColumnOrdinal(colID)
	return mb.parseColExpr(
		colID,
		mb.parsedColComputedExprs,
		mb.tab.Column(ord).ComputedExprStr(),
	)
}

// parseDefaultExpr parses the default (including nullable) expression for the
// given table column, and caches it for reuse.
func (mb *mutationBuilder) parseDefaultExpr(colID opt.ColumnID) tree.Expr {
	if mb.parsedColDefaultExprs == nil {
		mb.parsedColDefaultExprs = make([]tree.Expr, mb.tab.ColumnCount())
	}

	ord := mb.tabID.ColumnOrdinal(colID)
	col := mb.tab.Column(ord)
	exprStr := col.DefaultExprStr()

	// If no default expression, return NULL or a default value.
	if exprStr == "" {
		if col.IsMutation() && !col.IsNullable() {
			// Synthesize default value for NOT NULL mutation column so that it can be
			// set when in the write-only state. This is only used when no other value
			// is possible (no default value available, NULL not allowed).
			datum, err := tree.NewDefaultDatum(&mb.b.evalCtx.CollationEnv, col.DatumType())
			if err != nil {
				panic(err)
			}
			return datum
		}

		return tree.DNull
	}

	return mb.parseColExpr(
		colID,
		mb.parsedColDefaultExprs,
		exprStr,
	)
}

// parseOnUpdateExpr parses the on update (including nullable) expression for
// the given table column, and caches it for reuse.
func (mb *mutationBuilder) parseOnUpdateExpr(colID opt.ColumnID) tree.Expr {
	if mb.parsedColOnUpdateExprs == nil {
		mb.parsedColOnUpdateExprs = make([]tree.Expr, mb.tab.ColumnCount())
	}

	ord := mb.tabID.ColumnOrdinal(colID)
	return mb.parseColExpr(
		colID,
		mb.parsedColOnUpdateExprs,
		mb.tab.Column(ord).OnUpdateExprStr(),
	)
}

func (mb *mutationBuilder) parseColExpr(
	colID opt.ColumnID, cache []tree.Expr, exprStr string,
) tree.Expr {
	// Return expression from cache, if it was already parsed previously.
	ord := mb.tabID.ColumnOrdinal(colID)
	if cache[ord] != nil {
		return cache[ord]
	}

	expr, err := parser.ParseExpr(exprStr)
	if err != nil {
		panic(err)
	}

	cache[ord] = expr
	return expr
}

// parsePartialIndexPredicateExpr parses the partial index predicate for the
// given index and caches it for reuse. This function panics if the index at the
// given ordinal is not a partial index.
func (mb *mutationBuilder) parsePartialIndexPredicateExpr(idx cat.IndexOrdinal) tree.Expr {
	index := mb.tab.Index(idx)

	predStr, isPartial := index.Predicate()
	if !isPartial {
		panic(errors.AssertionFailedf("index at ordinal %d is not a partial index", idx))
	}

	if mb.parsedIndexExprs == nil {
		mb.parsedIndexExprs = make([]tree.Expr, mb.tab.DeletableIndexCount())
	}

	// Return expression from the cache, if it was already parsed previously.
	if mb.parsedIndexExprs[idx] != nil {
		return mb.parsedIndexExprs[idx]
	}

	expr, err := parser.ParseExpr(predStr)
	if err != nil {
		panic(err)
	}

	mb.parsedIndexExprs[idx] = expr
	return expr
}

// parseUniqueConstraintPredicateExpr parses the predicate of the given partial
// unique constraint and caches it for reuse. This function panics if the unique
// constraint at the given ordinal is not partial.
func (mb *mutationBuilder) parseUniqueConstraintPredicateExpr(uniq cat.UniqueOrdinal) tree.Expr {
	uniqueConstraint := mb.tab.Unique(uniq)

	predStr, isPartial := uniqueConstraint.Predicate()
	if !isPartial {
		panic(errors.AssertionFailedf("unique constraint at ordinal %d is not a partial unique constraint", uniq))
	}

	if mb.parsedUniqueConstraintExprs == nil {
		mb.parsedUniqueConstraintExprs = make([]tree.Expr, mb.tab.UniqueCount())
	}

	// Return expression from the cache, if it was already parsed previously.
	if mb.parsedUniqueConstraintExprs[uniq] != nil {
		return mb.parsedUniqueConstraintExprs[uniq]
	}

	expr, err := parser.ParseExpr(predStr)
	if err != nil {
		panic(err)
	}

	mb.parsedUniqueConstraintExprs[uniq] = expr
	return expr
}

// getIndexLaxKeyOrdinals returns the ordinals of all lax key columns in the
// given index. A column's ordinal is the ordered position of that column in the
// owning table.
func getIndexLaxKeyOrdinals(index cat.Index) intsets.Fast {
	var keyOrds intsets.Fast
	for i, n := 0, index.LaxKeyColumnCount(); i < n; i++ {
		keyOrds.Add(index.Column(i).Ordinal())
	}
	return keyOrds
}

// getUniqueConstraintOrdinals returns the ordinals of all columns in the given
// unique constraint. A column's ordinal is the ordered position of that column
// in the owning table.
func getUniqueConstraintOrdinals(tab cat.Table, uc cat.UniqueConstraint) intsets.Fast {
	var ucOrds intsets.Fast
	for i, n := 0, uc.ColumnCount(); i < n; i++ {
		ucOrds.Add(uc.ColumnOrdinal(tab, i))
	}
	return ucOrds
}

// getExplicitPrimaryKeyOrdinals returns the ordinals of the primary key
// columns, excluding any implicit partitioning or hash-shard columns in the
// primary index.
func getExplicitPrimaryKeyOrdinals(tab cat.Table) intsets.Fast {
	index := tab.Index(cat.PrimaryIndex)
	skipCols := index.ImplicitColumnCount()
	var keyOrds intsets.Fast
	for i, n := skipCols, index.LaxKeyColumnCount(); i < n; i++ {
		keyOrds.Add(index.Column(i).Ordinal())
	}
	return keyOrds
}

// findNotNullIndexCol finds the first not-null column in the given index and
// returns its ordinal position in the owner table. There must always be such a
// column, even if it turns out to be an implicit primary key column.
func findNotNullIndexCol(index cat.Index) int {
	for i, n := 0, index.KeyColumnCount(); i < n; i++ {
		indexCol := index.Column(i)
		if !indexCol.IsNullable() {
			return indexCol.Ordinal()
		}
	}
	panic(errors.AssertionFailedf("should have found not null column in index"))
}

// resultsNeeded determines whether a statement that might have a RETURNING
// clause needs to provide values for result rows for a downstream plan.
func resultsNeeded(r tree.ReturningClause) bool {
	switch t := r.(type) {
	case *tree.ReturningExprs:
		return true
	case *tree.ReturningNothing, *tree.NoReturningClause:
		return false
	default:
		panic(errors.AssertionFailedf("unexpected ReturningClause type: %T", t))
	}
}

// addAssignmentCasts builds a projection that wraps columns in srcCols with
// assignment casts when necessary so that the resulting columns have types
// identical to their target column types.
//
// srcCols should be either insertColIDs, updateColIDs, or upsertColsIDs where
// the length of srcCols is equal to the number of columns in the target table.
// The columns in srcCols are updated with new column IDs of the projected
// assignment casts.
//
// If there is no valid assignment cast from a column type in srcCols to its
// corresponding target column type, then this function throws an error.
func (mb *mutationBuilder) addAssignmentCasts(srcCols opt.OptionalColList) {
	var projectionScope *scope
	for ord, colID := range srcCols {
		if colID == 0 {
			// Column not mutated, so nothing to do.
			continue
		}

		srcType := mb.md.ColumnMeta(colID).Type
		targetCol := mb.tab.Column(ord)
		targetType := mb.tab.Column(ord).DatumType()

		// An assignment cast is not necessary if the source and target types
		// are identical.
		if srcType.Identical(targetType) {
			continue
		}

		// Check if an assignment cast is available from the inScope column
		// type to the out type.
		if !cast.ValidCast(srcType, targetType, cast.ContextAssignment) {
			panic(sqlerrors.NewInvalidAssignmentCastError(srcType, targetType, string(targetCol.ColName())))
		}

		// Create the cast expression.
		variable := mb.b.factory.ConstructVariable(colID)
		cast := mb.b.factory.ConstructAssignmentCast(variable, targetType)

		// Lazily create the new scope.
		if projectionScope == nil {
			projectionScope = mb.outScope.replace()
			projectionScope.appendColumnsFromScope(mb.outScope)
		}

		// Update the scope column to be casted.
		//
		// When building an UPDATE..FROM expression the projectionScope may have
		// two columns with different names but the same ID. To get the correct
		// column, we perform a lookup with the ID and the name. See #61520.
		scopeCol := projectionScope.getColumnWithIDAndReferenceName(colID, targetCol.ColName())
		scopeCol.name = scopeCol.name.WithMetadataName(fmt.Sprintf("%s_cast", targetCol.ColName()))
		mb.b.populateSynthesizedColumn(scopeCol, cast)

		// Replace old source column with the new one.
		srcCols[ord] = scopeCol.id
	}

	if projectionScope != nil {
		projectionScope.expr = mb.b.constructProject(mb.outScope.expr, projectionScope.cols)
		mb.outScope = projectionScope
	}
}

// partialIndexCount returns the number of public, write-only, and delete-only
// partial indexes defined on the table.
func partialIndexCount(tab cat.Table) int {
	count := 0
	for i, n := 0, tab.DeletableIndexCount(); i < n; i++ {
		if _, ok := tab.Index(i).Predicate(); ok {
			count++
		}
	}
	return count
}

func vectorIndexCount(tab cat.Table) int {
	count := 0
	for i, n := 0, tab.DeletableIndexCount(); i < n; i++ {
		if tab.Index(i).Type() == idxtype.VECTOR {
			count++
		}
	}
	return count
}

type checkInputScanType uint8

const (
	checkInputScanNewVals checkInputScanType = iota
	checkInputScanFetchedVals
)

// buildCheckInputScan constructs an expression that produces the new values of
// rows during a mutation. It is used in expressions that generate rows for
// checking for FK and uniqueness violations. It returns either a WithScan that
// iterates over the input to the mutation operator, or a Values expression with
// constant insert values inlined.
//
// If a WithScan expression is returned, it will scan either the new values or
// the fetched values for the given table ordinals (which correspond to FK or
// unique columns).
//
// Returns a scope containing the WithScan or Values expression and the output
// columns from the WithScan. The output columns map 1-to-1 to tabOrdinals. Also
// returns the subset of these columns that can be assumed to be not null
// (either because they are not null in the mutation input or because they are
// non-nullable table columns).
//
// isFK should be true when building inputs for FK checks, and false otherwise.
func (mb *mutationBuilder) buildCheckInputScan(
	typ checkInputScanType, tabOrdinals []int, isFK bool,
) (outScope *scope, notNullOutCols opt.ColSet) {
	// inputCols are the column IDs from the mutation input that we are scanning.
	inputCols := make(opt.ColList, len(tabOrdinals))

	outScope = mb.b.allocScope()
	outScope.cols = make([]scopeColumn, len(inputCols))

	for i, tabOrd := range tabOrdinals {
		if typ == checkInputScanNewVals {
			inputCols[i] = mb.mapToReturnColID(tabOrd)
		} else {
			inputCols[i] = mb.fetchColIDs[tabOrd]
		}
		if inputCols[i] == 0 {
			panic(errors.AssertionFailedf("no value for check input column (tabOrd=%d)", tabOrd))
		}

		// Synthesize a new output column for the input column, using the name
		// of the column in the underlying table. The table's column names are
		// used because partial unique constraint checks must filter the
		// WithScan or Values rows with a predicate expression that references
		// the table's columns.
		tableCol := mb.b.factory.Metadata().Table(mb.tabID).Column(tabOrd)
		outCol := mb.md.AddColumn(string(tableCol.ColName()), tableCol.DatumType())
		outScope.cols[i] = scopeColumn{
			id:   outCol,
			name: scopeColName(tableCol.ColName()),
			typ:  tableCol.DatumType(),
		}

		// If a table column is not nullable, NULLs cannot be inserted (the
		// mutation will fail). So for the purposes of checks, we can treat
		// these columns as not null.
		if mb.outScope.expr.Relational().NotNullCols.Contains(inputCols[i]) ||
			!mb.tab.Column(tabOrd).IsNullable() {
			notNullOutCols.Add(outCol)
		}
	}

	// If the check is not an FK check, attempt to inline the insert values in
	// the check input. This avoids buffering the mutation input and scanning it
	// with a WithScan. The inlined values may allow for further optimization of
	// the check.
	//
	// TODO(mgartner): We do not currently inline constants for FK checks
	// because this would break the insert fast path. The fast path can
	// currently only be planned when FK checks are built with WithScans.
	//
	// We also do not inline constants for checks that have row-level triggers
	// because the triggers may modify the values that are being checked.
	if !isFK && mb.insertExpr != nil &&
		!cat.HasRowLevelTriggers(mb.tab, tree.TriggerActionTimeBefore, tree.TriggerEventInsert) {
		// Find the constant columns produced by the insert expression. All
		// input columns must be constant in order to inline them.
		constCols := memo.FindInlinableConstants(mb.insertExpr)
		if inputCols.ToSet().SubsetOf(constCols) {
			elems := make(memo.ScalarListExpr, len(inputCols))
			colTypes := make([]*types.T, len(inputCols))
			for i, colID := range inputCols {
				elem := memo.ExtractColumnFromProjectOrValues(mb.insertExpr, colID)
				elems[i] = elem
				colTypes[i] = elem.DataType()
			}

			// Create a Values expression as the input to the check.
			tupleTyp := types.MakeTuple(colTypes)
			row := mb.b.factory.ConstructTuple(elems, tupleTyp)
			outScope.expr = mb.b.factory.ConstructValues(memo.ScalarListExpr{row}, &memo.ValuesPrivate{
				Cols: outScope.colList(),
				ID:   mb.b.factory.Metadata().NextUniqueID(),
			})

			return outScope, notNullOutCols
		}
	}

	mb.ensureWithID()
	outScope.expr = mb.b.factory.ConstructWithScan(&memo.WithScanPrivate{
		With:       mb.withID,
		InCols:     inputCols,
		OutCols:    outScope.colList(),
		ID:         mb.b.factory.Metadata().NextUniqueID(),
		CheckInput: true,
	})

	return outScope, notNullOutCols
}
