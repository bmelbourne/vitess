/*
Copyright 2021 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package operators

import (
	"slices"
	"strings"

	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vtgate/evalengine"
	"vitess.io/vitess/go/vt/vtgate/planbuilder/plancontext"
	"vitess.io/vitess/go/vt/vtgate/semantics"
)

type Filter struct {
	unaryOperator
	Predicates []sqlparser.Expr

	// PredicateWithOffsets is the evalengine expression that will finally be used.
	// It contains the ANDed predicates in Predicates, with ColName:s replaced by Offset:s
	PredicateWithOffsets evalengine.Expr

	ResultColumns int
}

func newFilterSinglePredicate(op Operator, expr sqlparser.Expr) Operator {
	return newFilter(op, expr)
}

func newFilter(op Operator, expr ...sqlparser.Expr) Operator {
	return &Filter{
		unaryOperator: newUnaryOp(op),
		Predicates:    expr,
	}
}

// Clone implements the Operator interface
func (f *Filter) Clone(inputs []Operator) Operator {
	klon := *f
	klon.Source = inputs[0]
	klon.Predicates = slices.Clone(f.Predicates)
	return &klon
}

// UnsolvedPredicates implements the unresolved interface
func (f *Filter) UnsolvedPredicates(st *semantics.SemTable) []sqlparser.Expr {
	var result []sqlparser.Expr
	id := TableID(f)
	for _, p := range f.Predicates {
		deps := st.RecursiveDeps(p)
		if !deps.IsSolvedBy(id) {
			result = append(result, p)
		}
	}
	return result
}

func (f *Filter) AddPredicate(ctx *plancontext.PlanningContext, expr sqlparser.Expr) Operator {
	f.Source = f.Source.AddPredicate(ctx, expr)
	return f
}

func (f *Filter) AddColumn(ctx *plancontext.PlanningContext, reuse bool, gb bool, expr *sqlparser.AliasedExpr) int {
	return f.Source.AddColumn(ctx, reuse, gb, expr)
}

func (f *Filter) FindCol(ctx *plancontext.PlanningContext, expr sqlparser.Expr, underRoute bool) int {
	return f.Source.FindCol(ctx, expr, underRoute)
}

func (f *Filter) AddWSColumn(ctx *plancontext.PlanningContext, offset int, underRoute bool) int {
	return f.Source.AddWSColumn(ctx, offset, underRoute)
}

func (f *Filter) GetColumns(ctx *plancontext.PlanningContext) []*sqlparser.AliasedExpr {
	return truncate(f, f.Source.GetColumns(ctx))
}

func (f *Filter) GetSelectExprs(ctx *plancontext.PlanningContext) []sqlparser.SelectExpr {
	return truncate(f, f.Source.GetSelectExprs(ctx))
}

func (f *Filter) GetOrdering(ctx *plancontext.PlanningContext) []OrderBy {
	return f.Source.GetOrdering(ctx)
}

func (f *Filter) planOffsets(ctx *plancontext.PlanningContext) Operator {
	cfg := &evalengine.Config{
		ResolveType: ctx.TypeForExpr,
		Collation:   ctx.SemTable.Collation,
		Environment: ctx.VSchema.Environment(),
	}

	predicate := sqlparser.AndExpressions(f.Predicates...)
	rewritten := useOffsets(ctx, predicate, f)
	eexpr, err := evalengine.Translate(rewritten, cfg)
	if err != nil {
		if strings.HasPrefix(err.Error(), evalengine.ErrTranslateExprNotSupported) {
			panic(vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "%s: %s", evalengine.ErrTranslateExprNotSupported, sqlparser.String(predicate)))
		}
		panic(err)
	}

	f.PredicateWithOffsets = eexpr
	return nil
}

func (f *Filter) ShortDescription() string {
	return sqlparser.String(sqlparser.AndExpressions(f.Predicates...))
}

func (f *Filter) setTruncateColumnCount(offset int) {
	f.ResultColumns = offset
}

func (f *Filter) getTruncateColumnCount() int {
	return f.ResultColumns
}
