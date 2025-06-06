// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package xform

import (
	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/cat"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/constraint"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/idxconstraint"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/memo"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/ordering"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/props"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/props/physical"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/idxtype"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/errors"
)

// LimitScanPrivate constructs a new ScanPrivate value that is based on the
// given ScanPrivate. The new private's HardLimit is set to the given limit,
// which must be a constant int datum value. The other fields are inherited from
// the existing private.
func (c *CustomFuncs) LimitScanPrivate(
	scanPrivate *memo.ScanPrivate, limit tree.Datum, required props.OrderingChoice,
) *memo.ScanPrivate {
	// Determine the scan direction necessary to provide the required ordering.
	_, reverse := ordering.ScanPrivateCanProvide(c.e.mem.Metadata(), scanPrivate, &required)

	newScanPrivate := *scanPrivate
	newScanPrivate.HardLimit = memo.MakeScanLimit(int64(*limit.(*tree.DInt)), reverse)
	return &newScanPrivate
}

// CanLimitFilteredScan returns true if the given scan has not already been
// limited, and is constrained or scans a partial index. This is only possible
// when the required ordering of the rows to be limited can be satisfied by the
// Scan operator.
//
// NOTE: Limiting unconstrained, non-partial index scans is done by the
//
//	GenerateLimitedScans rule, since that can require IndexJoin operators
//	to be generated.
func (c *CustomFuncs) CanLimitFilteredScan(
	scanPrivate *memo.ScanPrivate, required props.OrderingChoice,
) bool {
	if scanPrivate.HardLimit != 0 {
		// Don't push limit into scan if scan is already limited. This would
		// usually only happen when normalizations haven't run, as otherwise
		// redundant Limit operators would be discarded.
		return false
	}

	md := c.e.mem.Metadata()
	if scanPrivate.Constraint == nil && scanPrivate.PartialIndexPredicate(md) == nil {
		// This is not a constrained scan nor a partial index scan, so skip it.
		// The GenerateLimitedScans rule is responsible for limited
		// unconstrained scans on non-partial indexes.
		return false
	}
	// Virtual indexes are not sorted, but are presented to the optimizer as
	// sorted, and have a sort automatically applied to them on the output of the
	// scan. Since this implicit sort happens after the scan, we can't place a
	// hard limit on the scan if the query semantics require a sort order on the
	// entire relation.
	if scanPrivate.IsVirtualTable(md) && !required.Any() {
		return false
	}
	ok, _ := ordering.ScanPrivateCanProvide(md, scanPrivate, &required)
	return ok
}

// PushLimitIntoProjectFilteredScanEnabled returns true if its eponymous rule is
// enabled via its session setting.
func (c *CustomFuncs) PushLimitIntoProjectFilteredScanEnabled() bool {
	return c.e.evalCtx.SessionData().OptimizerPushLimitIntoProjectFilteredScan
}

// GenerateLimitedScans enumerates all non-inverted and non-partial secondary
// indexes on the Scan operator's table and tries to create new limited Scan
// operators from them. Since this only needs to be done once per table,
// GenerateLimitedScans should only be called on the original unaltered primary
// index Scan operator (i.e. not constrained or limited).
//
// For a secondary index that "covers" the columns needed by the scan, a single
// limited Scan operator is created. For a non-covering index, an IndexJoin is
// constructed to add missing columns to the limited Scan.
//
// Inverted index scans are not guaranteed to produce a specific number
// of result rows because they contain multiple entries for a single row
// indexed. Therefore, they cannot be considered for limited scans.
//
// Partial indexes do not index every row in the table and they can only be used
// in cases where a query filter implies the partial index predicate.
// GenerateLimitedScans deals with limits, but no filters, so it cannot generate
// limited partial index scans. Limiting partial indexes is done by the
// PushLimitIntoFilteredScans rule.
func (c *CustomFuncs) GenerateLimitedScans(
	grp memo.RelExpr,
	required *physical.Required,
	scanPrivate *memo.ScanPrivate,
	limit tree.Datum,
	requiredOrdering props.OrderingChoice,
) {
	// Virtual indexes are not sorted, but are presented to the optimizer as
	// sorted, and have a sort automatically applied to them on the output of the
	// scan. Since this implicit sort happens after the scan, we can't place a
	// hard limit on the scan if the query semantics require a sort order on the
	// entire relation.
	if scanPrivate.IsVirtualTable(c.e.mem.Metadata()) && !requiredOrdering.Any() {
		return
	}
	limitVal := int64(*limit.(*tree.DInt))

	var pkCols opt.ColSet
	var sb indexScanBuilder
	sb.Init(c, scanPrivate.Table)

	// Iterate over all non-inverted, non-partial, non-vector indexes, looking for
	// those that can be limited.
	var iter scanIndexIter
	reject := rejectInvertedIndexes | rejectPartialIndexes | rejectVectorIndexes
	iter.Init(c.e.evalCtx, c.e, c.e.mem, &c.im, scanPrivate, nil /* filters */, reject)
	iter.ForEach(func(index cat.Index, filters memo.FiltersExpr, indexCols opt.ColSet, isCovering bool, constProj memo.ProjectionsExpr) {
		// The iterator rejects partial indexes because there are no filters to
		// imply a partial index predicate. constProj is a projection of
		// constant values based on a partial index predicate. It should always
		// be empty because we iterate only on non-partial indexes. If it is
		// not, we panic to avoid performing a logically incorrect
		// transformation.
		if len(constProj) != 0 {
			panic(errors.AssertionFailedf("expected constProj to be empty"))
		}

		newScanPrivate := *scanPrivate
		newScanPrivate.Distribution.Regions = nil
		newScanPrivate.Index = index.Ordinal()

		// If the alternate index does not conform to the ordering, then skip it.
		// If reverse=true, then the scan needs to be in reverse order to match
		// the requiredOrdering ordering.
		ok, reverse := ordering.ScanPrivateCanProvide(c.e.mem.Metadata(), &newScanPrivate, &requiredOrdering)
		if !ok {
			return
		}
		newScanPrivate.HardLimit = memo.MakeScanLimit(limitVal, reverse)

		// If the alternate index includes the set of needed columns, then construct
		// a new Scan operator using that index.
		if isCovering {
			sb.SetScan(&newScanPrivate)
			sb.Build(grp)
			return
		}

		// Otherwise, try to construct an IndexJoin operator that provides the
		// columns missing from the index.
		if scanPrivate.Flags.NoIndexJoin {
			return
		}

		// Calculate the PK columns once.
		if pkCols.Empty() {
			pkCols = c.PrimaryKeyCols(scanPrivate.Table)
		}

		// Scan whatever columns we need which are available from the index, plus
		// the PK columns.
		newScanPrivate.Cols = indexCols.Intersection(scanPrivate.Cols)
		newScanPrivate.Cols.UnionWith(pkCols)
		sb.SetScan(&newScanPrivate)

		// The Scan operator will go into its own group (because it projects a
		// different set of columns), and the IndexJoin operator will be added to
		// the same group as the original Limit operator.
		sb.AddIndexJoin(scanPrivate.Cols)

		sb.Build(grp)
	})
}

// ScanIsLimited returns true if the scan operator with the given ScanPrivate is
// limited.
func (c *CustomFuncs) ScanIsLimited(sp *memo.ScanPrivate) bool {
	return sp.HardLimit != 0
}

// ScanIsInverted returns true if the index of the given ScanPrivate is an
// inverted index.
func (c *CustomFuncs) ScanIsInverted(sp *memo.ScanPrivate) bool {
	md := c.e.mem.Metadata()
	idx := md.Table(sp.Table).Index(sp.Index)
	return idx.Type() == idxtype.INVERTED
}

// SplitLimitedScanIntoUnionScans returns a UnionAll tree of Scan operators with
// hard limits that each scan over a single key from the original Scan's
// constraints. If no such UnionAll of Scans can be found, ok=false is returned.
// This is beneficial in cases where the original Scan had to scan over many
// rows but had relatively few keys to scan over.
// TODO(drewk): handle inverted scans.
func (c *CustomFuncs) SplitLimitedScanIntoUnionScans(
	limitOrdering props.OrderingChoice, scan memo.RelExpr, sp *memo.ScanPrivate, limit tree.Datum,
) (_ memo.RelExpr, ok bool) {
	return c.SplitLimitedSelectIntoUnionScansOrSelects(limitOrdering, scan, sp, limit, nil)
}

// SplitLimitedSelectIntoUnionScansOrSelects returns a UnionAll tree of Scans
// with limits or Selects from Scans with limit hints that each scan over a
// single key from the Select's original Scan constraints. If no such UnionAll
// of Scans or Selects can be found, ok=false is returned. This is beneficial in
// cases where the original Select had to scan over many rows but had relatively
// few keys to scan over. The original Select is not a parameter. Instead,
// parameters scan and filters, the children of the original Select, are passed.
// Selects whose first child is not a Scan are not applicable.
// If filters is nil, Scans are produced, otherwise Selects.
func (c *CustomFuncs) SplitLimitedSelectIntoUnionScansOrSelects(
	limitOrdering props.OrderingChoice,
	scan memo.RelExpr,
	sp *memo.ScanPrivate,
	limit tree.Datum,
	filters memo.FiltersExpr,
) (_ memo.RelExpr, ok bool) {

	cons, ok := c.getKnownScanConstraint(sp)
	if !ok {
		// No valid constraint was found.
		return nil, false
	}

	// Find the length of the prefix of index columns preceding the first limit
	// ordering column. We will verify later that the entire ordering sequence is
	// represented in the index. Ex:
	//
	//     Index: +1/+2/-3, Limit Ordering: +3 => Prefix Length: 2
	//
	if len(limitOrdering.Columns) == 0 {
		// This case can be handled by GenerateLimitedScans.
		return nil, false
	}
	keyPrefixLength := cons.Columns.Count()
	for i := 0; i < cons.Columns.Count(); i++ {
		if limitOrdering.Group(0).Contains(cons.Columns.Get(i).ID()) {
			keyPrefixLength = i
			break
		}
	}
	if keyPrefixLength == 0 {
		// This case can be handled by GenerateLimitedScans.
		return nil, false
	}

	limitVal := int(*limit.(*tree.DInt))
	return c.splitScanIntoUnionScansOrSelects(limitOrdering, scan, sp, cons, limitVal, keyPrefixLength, filters)
}

// MakeTopKPrivate returns a TopKPrivate operator with a constant, positive
// integer limit and an order.
func (c *CustomFuncs) MakeTopKPrivate(
	limit tree.Datum, ordering props.OrderingChoice,
) *memo.TopKPrivate {
	limitVal := int64(*limit.(*tree.DInt))
	return &memo.TopKPrivate{
		K:        limitVal,
		Ordering: ordering,
	}
}

// GenerateLimitedTopKScans enumerates all non-inverted secondary indexes on the
// given Scan operator's table and generates an alternate Scan operator for
// each index that includes a partial set of needed columns specified in the
// ScanOpDef. An IndexJoin is constructed to add missing columns. A TopK is also
// constructed to make an equivalent expression for the memo.
//
// For cases where the Scan's secondary index covers all needed columns, see
// GenerateIndexScans, which does not construct an IndexJoin.
func (c *CustomFuncs) GenerateLimitedTopKScans(
	grp memo.RelExpr, required *physical.Required, sp *memo.ScanPrivate, tp *memo.TopKPrivate,
) {
	requiredOrdering := tp.Ordering
	// If the ordering was already optimized out (e.g., there is only one possible
	// value for what would have been the requiredOrdering ordering), then there is no
	// benefit to exploring limited top K.
	if len(requiredOrdering.Columns) == 0 {
		return
	}
	// Iterate over all non-inverted and non-vector secondary indexes.
	var pkCols opt.ColSet
	var iter scanIndexIter
	var sb indexScanBuilder
	sb.Init(c, sp.Table)
	reject := rejectPrimaryIndex | rejectInvertedIndexes | rejectVectorIndexes
	iter.Init(c.e.evalCtx, c.e, c.e.mem, &c.im, sp, nil /* filters */, reject)
	iter.ForEach(func(index cat.Index, filters memo.FiltersExpr, indexCols opt.ColSet, isCovering bool, constProj memo.ProjectionsExpr) {
		// The iterator only produces pseudo-partial indexes (the predicate is
		// true) because no filters are passed to iter.Init to imply a partial
		// index predicate. constProj is a projection of constant values based
		// on a partial index predicate. It should always be empty because a
		// pseudo-partial index cannot hold a column constant. If it is not, we
		// panic to avoid performing a logically incorrect transformation.
		if len(constProj) != 0 {
			panic(errors.AssertionFailedf("expected constProj to be empty"))
		}

		// If the secondary index includes the set of needed columns, then this
		// case does not need a limited top K and will be covered in
		// GenerateIndexScans.
		if isCovering {
			return
		}

		// Otherwise, try to construct an IndexJoin operator that provides the
		// columns missing from the index.
		if sp.Flags.NoIndexJoin {
			return
		}

		// Calculate the PK columns once.
		if pkCols.Empty() {
			pkCols = c.PrimaryKeyCols(sp.Table)
		}

		// If the first index column and ordering column are not the same, then
		// there is no benefit to exploring this index.
		if col := sp.Table.ColumnID(index.Column(0).Ordinal()); !requiredOrdering.Columns[0].Group.Contains(col) {
			return
		}

		// If the index doesn't contain any of the requiredOrdering order columns, then
		// there is no benefit to exploring this index.
		if !requiredOrdering.Any() && !requiredOrdering.Group(0).Intersects(indexCols) {
			return
		}
		// Scan whatever columns we need which are available from the index.
		newScanPrivate := *sp
		newScanPrivate.Distribution.Regions = nil
		newScanPrivate.Index = index.Ordinal()
		newScanPrivate.Cols = indexCols.Intersection(sp.Cols)
		// If the index is not covering, scan the needed index columns plus
		// primary key columns.
		newScanPrivate.Cols.UnionWith(pkCols)
		sb.SetScan(&newScanPrivate)
		// Construct an IndexJoin operator that provides the columns missing from
		// the index.
		sb.AddIndexJoin(sp.Cols)
		input := sb.BuildNewExpr()
		// Use the overlapping indexes and requiredOrdering ordering.
		newPrivate := *tp
		c.e.mem.AddTopKToGroup(&memo.TopKExpr{Input: input, TopKPrivate: newPrivate}, grp)
	})
}

// getPrefixFromOrdering returns an OrderingChoice that holds the prefix
// of Ordering o that satisfies part of the required OrderingChoice intraOrd,
// a bool indicating whether the entire Ordering o was satisfied, and a bool
// indicating whether a prefix of any kind was found.
// isOptional is a function that allows the caller to impose additional
// constraints on columns that are considered optional, and should return true
// if the column is optional.
func getPrefixFromOrdering(
	o opt.Ordering,
	intraOrd props.OrderingChoice,
	input memo.RelExpr,
	isOptional func(id opt.ColumnID) bool,
) (newOrd props.OrderingChoice, isFullPrefix bool, found bool) {
	// We are looking for a prefix of o that satisfies part of the required ordering
	oIdx, intraIdx := 0, 0
	for ; oIdx < len(o); oIdx++ {
		oCol := o[oIdx].ID()
		if intraOrd.Optional.Contains(oCol) || isOptional(oCol) {
			// Optional column.
			continue
		}

		if intraIdx < len(intraOrd.Columns) &&
			intraOrd.Group(intraIdx).Contains(oCol) &&
			intraOrd.Columns[intraIdx].Descending == o[oIdx].Descending() {
			// Column matches the one in the ordering.
			intraIdx++
			continue
		}
		break
	}
	isFullPrefix = intraIdx == len(intraOrd.Columns)
	if oIdx == 0 {
		// No match.
		return newOrd, isFullPrefix, false
	}
	o = o[:oIdx]

	newOrd.FromOrderingWithOptCols(o, opt.ColSet{})

	// Simplify the ordering according to the input's FDs. Note that this is not
	// necessary for correctness because buildChildPhysicalProps would do it
	// anyway, but doing it here once can make things more efficient (and we may
	// generate fewer expressions if some of these orderings turn out to be
	// equivalent).
	newOrd.Simplify(&input.Relational().FuncDeps)
	return newOrd, isFullPrefix, true
}

// GeneratePartialOrderTopK generates TopK expressions with more specific orderings
// based on the interesting orderings property. This enables the optimizer to
// explore TopK with partially ordered input columns.
func (c *CustomFuncs) GeneratePartialOrderTopK(
	grp memo.RelExpr, required *physical.Required, input memo.RelExpr, private *memo.TopKPrivate,
) {
	orders := ordering.DeriveInterestingOrderings(c.e.mem, input)
	intraOrd := private.Ordering
	for _, ord := range orders {
		newOrd, fullPrefix, found := getPrefixFromOrdering(ord.ToOrdering(), intraOrd, input, func(id opt.ColumnID) bool {
			return false
		})
		// We don't need to generate a new expression if no prefix was found or the
		// prefix encompasses the entire ordering, since that would be a full, not
		// partial, order.
		if !found || fullPrefix {
			continue
		}

		newPrivate := *private
		newPrivate.PartialOrdering = newOrd

		c.e.mem.AddTopKToGroup(&memo.TopKExpr{Input: input, TopKPrivate: newPrivate}, grp)
	}
}

// CanPushOffsetIntoIndexJoin returns true if the session setting
// optimizer_push_offset_into_index_join is enabled.
func (c *CustomFuncs) CanPushOffsetIntoIndexJoin() bool {
	return c.e.evalCtx.SessionData().OptimizerPushOffsetIntoIndexJoin
}

// IsFixedWidthVectorCol returns true if the given column is a fixed-width
// vector.
func (c *CustomFuncs) IsFixedWidthVectorCol(col opt.ColumnID) bool {
	typ := c.e.mem.Metadata().ColumnMeta(col).Type
	if typ.Family() != types.PGVectorFamily {
		return false
	}
	return typ.Width() > 0
}

// OrderingBySingleColAsc returns true if the given ordering choice allows an
// ordering that is simply a single column in ascending order. It does not allow
// the empty ordering.
func (c *CustomFuncs) OrderingBySingleColAsc(ordering props.OrderingChoice, col opt.ColumnID) bool {
	cols := ordering.Columns
	return len(cols) == 1 && !cols[0].Descending && cols[0].Group.Contains(col)
}

// TryGenerateVectorSearch attempts to generate a vector search plan for the
// given query vector, which is assumed to be part of a KNN search against the
// given vector column. The plan consists of:
//  1. A vector-search operator which produces candidate PKs.
//  2. A lookup-join to retrieve the vector column and any other needed columns.
//  3. A projection of the distance between the vector column and query vector.
//  4. A top-k operator to perform re-ranking.
//
// Note that TryGenerateVectorSearch does not handle additional filters beyond
// those used to constrain index prefix columns.
func (c *CustomFuncs) TryGenerateVectorSearch(
	grp memo.RelExpr,
	_ *physical.Required,
	scanExpr *memo.ScanExpr,
	originalFilters memo.FiltersExpr,
	passthrough opt.ColSet,
	vectorCol opt.ColumnID,
	queryVector opt.ScalarExpr,
	projections memo.ProjectionsExpr,
	limit tree.Datum,
	limitOrd props.OrderingChoice,
) {
	sp := &scanExpr.ScanPrivate
	tab := c.e.mem.Metadata().TableMeta(sp.Table)

	// Generate implicit filters from constraints and computed columns as
	// optional filters to help constrain an index scan.
	originalOptionalFilters := c.checkConstraintFilters(sp.Table)
	computedColFilters := c.ComputedColFilters(sp, originalFilters, originalOptionalFilters)
	originalOptionalFilters = append(originalOptionalFilters, computedColFilters...)

	var iter scanIndexIter
	iter.Init(c.e.evalCtx, c.e, c.e.mem, &c.im, sp, originalFilters, rejectNonVectorIndexes)
	iter.ForEach(func(index cat.Index, filters memo.FiltersExpr, _ opt.ColSet, _ bool, _ memo.ProjectionsExpr) {
		if sp.Table.ColumnID(index.VectorColumn().Ordinal()) != vectorCol {
			// This index is for a different vector column.
			return
		}
		var ok bool
		var prefixConstraint *constraint.Constraint
		prefixColumns, notNullCols := idxconstraint.IndexPrefixCols(sp.Table, index)
		if len(prefixColumns) > 0 {
			optionalFilters := originalOptionalFilters
			if predScalar, isPartialIdx := tab.PartialIndexPredicate(index.Ordinal()); isPartialIdx {
				// Include the partial index predicate in the set of optional filters.
				pred := *predScalar.(*memo.FiltersExpr)
				if len(optionalFilters) == 0 {
					optionalFilters = pred
				} else {
					optionalFilters = make(memo.FiltersExpr, 0, len(originalOptionalFilters)+len(pred))
					optionalFilters = append(optionalFilters, originalOptionalFilters...)
					optionalFilters = append(optionalFilters, pred...)
				}
			}
			prefixConstraint, filters, ok = idxconstraint.ConstrainIndexPrefixCols(
				c.e.ctx, c.e.evalCtx, c.e.f, prefixColumns, notNullCols, filters,
				optionalFilters, sp.Table, index, c.checkCancellation,
			)
			if !ok {
				return
			}
			// Ensure that the constraint consists of single-key spans that each
			// specify a single value for every prefix column.
			if prefixConstraint.IsUnconstrained() || prefixConstraint.IsContradiction() {
				return
			}
			for i, n := 0, prefixConstraint.Spans.Count(); i < n; i++ {
				span := prefixConstraint.Spans.Get(i)
				if !span.HasSingleKey(c.e.ctx, c.e.evalCtx) {
					return
				}
				if span.StartKey().Length() != len(prefixColumns) {
					return
				}
			}
		}
		if len(filters) > 0 {
			// TODO(drewk): support additional filters once streaming vector search
			// execution is supported.
			return
		}

		// VectorSearch operators return the primary-key columns.
		limitInt := int64(*limit.(*tree.DInt))
		indexCols := c.PrimaryKeyCols(sp.Table)
		vectorSearch := c.e.f.ConstructVectorSearch(
			queryVector,
			&memo.VectorSearchPrivate{
				Table:               sp.Table,
				Index:               index.Ordinal(),
				PrefixConstraint:    prefixConstraint,
				Cols:                indexCols,
				TargetNeighborCount: limitInt,
			},
		)

		// Add a lookup-join against the primary index to get the rest of the
		// columns. The primary-key join is always necessary because the vector
		// column is not projected by the VectorSearch operator.
		primaryIndex := c.e.mem.Metadata().Table(sp.Table).Index(cat.PrimaryIndex)
		lookupCols := make(opt.ColList, primaryIndex.KeyColumnCount())
		for i := 0; i < primaryIndex.KeyColumnCount(); i++ {
			lookupCols[i] = sp.Table.IndexColumnID(primaryIndex, i)
		}
		lookupPrivate := &memo.LookupJoinPrivate{
			JoinType:              opt.InnerJoinOp,
			Table:                 sp.Table,
			Index:                 cat.PrimaryIndex,
			KeyCols:               lookupCols,
			Cols:                  sp.Cols,
			LookupColsAreTableKey: true,
			Locking:               sp.Locking,
		}
		vectorSearch = c.e.f.ConstructLookupJoin(vectorSearch, nil /* on */, lookupPrivate)

		// Add back the projections, including the distance column.
		vectorSearch = c.e.f.ConstructProject(vectorSearch, projections, passthrough)

		// Build a top-k operator ordering by the distance column and limited by the
		// NN count to obtain the final result. We verified when the rule matched
		// that the limit is ordering by the distance column.
		topKPrivate := memo.TopKPrivate{K: limitInt, Ordering: limitOrd}
		c.e.mem.AddTopKToGroup(&memo.TopKExpr{Input: vectorSearch, TopKPrivate: topKPrivate}, grp)
	})
}
