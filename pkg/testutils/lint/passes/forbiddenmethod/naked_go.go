// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package forbiddenmethod

import (
	"fmt"
	"go/ast"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/testutils/lint/passes/passesutil"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// NakedGoAnalyzer prevents use of the `go` keyword.
var NakedGoAnalyzer = NewNakedGoAnalyzer(
	"nakedgo",
	"Use of go keyword not allowed, use a Stopper instead",
	"Prevents direct use of the 'go' keyword. Goroutines should be launched through Stopper.",
	nil,
)

type FilterFunc func(pass *analysis.Pass) bool

func NewNakedGoAnalyzer(name, message, doc string, filter FilterFunc) *analysis.Analyzer {
	return &analysis.Analyzer{
		Name:     name,
		Doc:      doc,
		Requires: []*analysis.Analyzer{inspect.Analyzer},
		Run: func(pass *analysis.Pass) (interface{}, error) {
			if filter != nil && !filter(pass) {
				return nil, nil
			}
			inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
			inspect.Preorder([]ast.Node{
				(*ast.GoStmt)(nil),
			}, func(n ast.Node) {
				node := n.(*ast.GoStmt)

				const debug = false

				// NB: we're not using passesutil.HasNoLintComment because it
				// has false negatives (i.e. comments apply to infractions that
				// they were clearly not intended for).
				//
				// The approach here is inspired by `analysistest.check` - the
				// nolint comment has to be on the same line as the *end* of the
				// `*GoStmt`.
				f := passesutil.FindContainingFile(pass, n)
				cm := ast.NewCommentMap(pass.Fset, node, f.Comments)
				var cc *ast.Comment
				for _, cg := range cm[n] {
					for _, c := range cg.List {
						if c.Pos() < node.Go {
							// The current comment is "before" the `go` invocation, so it's
							// not relevant for silencing the lint.
							continue
						}
						if cc == nil || cc.Pos() > node.Go {
							// This comment is after, but closer to the `go` invocation than
							// previous candidate.
							cc = c
							if debug {
								fmt.Printf("closest comment now %d-%d: %s\n", cc.Pos(), cc.End(), cc.Text)
							}
						}
					}
				}
				if cc != nil && strings.Contains(cc.Text, "nolint:"+name) {
					if debug {
						fmt.Printf("GoStmt at: %d-%d\n", n.Pos(), n.End())
						fmt.Printf("GoStmt.Go at: %d\n", node.Go)
						fmt.Printf("GoStmt.Call at: %d-%d\n", node.Call.Pos(), node.Call.End())
					}

					goPos := pass.Fset.Position(node.End())
					cmPos := pass.Fset.Position(cc.Pos())

					if goPos.Line == cmPos.Line {
						if debug {
							fmt.Printf("suppressing lint because of %d-%d: %s\n", cc.Pos(), cc.End(), cc.Text)
						}
						return
					}
				}

				pass.Report(analysis.Diagnostic{
					Pos:     n.Pos(),
					Message: message,
				})
			})
			return nil, nil
		},
	}
}
