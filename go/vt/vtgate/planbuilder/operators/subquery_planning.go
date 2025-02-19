/*
Copyright 2022 The Vitess Authors.

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
	"io"

	"golang.org/x/exp/slices"

	"vitess.io/vitess/go/slice"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vtgate/engine/opcode"
	"vitess.io/vitess/go/vt/vtgate/planbuilder/operators/ops"
	"vitess.io/vitess/go/vt/vtgate/planbuilder/operators/rewrite"
	"vitess.io/vitess/go/vt/vtgate/planbuilder/plancontext"
	"vitess.io/vitess/go/vt/vtgate/semantics"
)

func isMergeable(ctx *plancontext.PlanningContext, query sqlparser.SelectStatement, op ops.Operator) bool {
	validVindex := func(expr sqlparser.Expr) bool {
		sc := findColumnVindex(ctx, op, expr)
		return sc != nil && sc.IsUnique()
	}

	if query.GetLimit() != nil {
		return false
	}

	sel, ok := query.(*sqlparser.Select)
	if !ok {
		return false
	}

	if len(sel.GroupBy) > 0 {
		// iff we are grouping, we need to check that we can perform the grouping inside a single shard, and we check that
		// by checking that one of the grouping expressions used is a unique single column vindex.
		// TODO: we could also support the case where all the columns of a multi-column vindex are used in the grouping
		for _, gb := range sel.GroupBy {
			if validVindex(gb) {
				return true
			}
		}
		return false
	}

	// if we have grouping, we have already checked that it's safe, and don't need to check for aggregations
	// but if we don't have groupings, we need to check if there are aggregations that will mess with us
	if sqlparser.ContainsAggregation(sel.SelectExprs) {
		return false
	}

	if sqlparser.ContainsAggregation(sel.Having) {
		return false
	}

	return true
}

func settleSubqueries(ctx *plancontext.PlanningContext, op ops.Operator) (ops.Operator, error) {
	visit := func(op ops.Operator, lhsTables semantics.TableSet, isRoot bool) (ops.Operator, *rewrite.ApplyResult, error) {
		switch op := op.(type) {
		case *SubQueryContainer:
			outer := op.Outer
			for _, subq := range op.Inner {
				newOuter, err := subq.settle(ctx, outer)
				if err != nil {
					return nil, nil, err
				}
				subq.Outer = newOuter
				outer = subq
			}
			return outer, rewrite.NewTree("extracted subqueries from subquery container", outer), nil
		case *Projection:
			ap, err := op.GetAliasedProjections()
			if err != nil {
				return nil, nil, err
			}

			for _, pe := range ap {
				mergeSubqueryExpr(ctx, pe)
			}
		case *Update:
			for _, setExpr := range op.Assignments {
				mergeSubqueryExpr(ctx, setExpr.Expr)
			}
		}
		return op, rewrite.SameTree, nil
	}
	return rewrite.BottomUp(op, TableID, visit, nil)
}

func mergeSubqueryExpr(ctx *plancontext.PlanningContext, pe *ProjExpr) {
	se, ok := pe.Info.(SubQueryExpression)
	if !ok {
		return
	}
	newExpr, rewritten := rewriteMergedSubqueryExpr(ctx, se, pe.EvalExpr)
	if rewritten {
		pe.EvalExpr = newExpr
	}
}

func rewriteMergedSubqueryExpr(ctx *plancontext.PlanningContext, se SubQueryExpression, expr sqlparser.Expr) (sqlparser.Expr, bool) {
	rewritten := false
	for _, sq := range se {
		for _, sq2 := range ctx.MergedSubqueries {
			if sq.originalSubquery == sq2 {
				expr = sqlparser.Rewrite(expr, nil, func(cursor *sqlparser.Cursor) bool {
					switch expr := cursor.Node().(type) {
					case *sqlparser.ColName:
						if expr.Name.String() != sq.ArgName { // TODO systay 2023.09.15 - This is not safe enough. We should figure out a better way.
							return true
						}
					case *sqlparser.Argument:
						if expr.Name != sq.ArgName {
							return true
						}
					default:
						return true
					}
					rewritten = true
					if sq.FilterType == opcode.PulloutExists {
						cursor.Replace(&sqlparser.ExistsExpr{Subquery: sq.originalSubquery})
					} else {
						cursor.Replace(sq.originalSubquery)
					}
					return false
				}).(sqlparser.Expr)
			}
		}
	}
	return expr, rewritten
}

// tryPushSubQueryInJoin attempts to push down a SubQuery into an ApplyJoin
/*
For this query:

    select 1 from user u1, user u2 where exists (
        select 1 from user_extra ue where ue.col = u1.col and ue.col = u2.col
    )

We can use a very simplified tree where the subquery starts at the top, like this:
┌──────────────────────────────────────────────────────────────────────┐
│SQ WHERE ue.col = u1.col and ue.col = u2.col, JoinVars: u1.col. u2.col│
└──┬────────────────────────────────────────────────────┬──────────────┘
 inner                                                outer
┌──▼──┐                                 ┌───────────────▼──────────────┐
│R(ue)│                                 │JOIN WHERE true JoinVars <nil>│
└─────┘                                 └──┬───────────────────────┬───┘
                                        ┌──▼──┐                  ┌─▼───┐
                                        │R(u1)│                  │R(u2)│
                                        └─────┘                  └─────┘

We transform it to:
    ┌────────────────────────────────┐
    │JOIN WHERE true JoinVars: u1.col│
    ├─────────────────────────────┬──┘
┌───▼─┐ ┌─────────────────────────▼────────────────────────────────────┐
│R(u1)│ │SQ WHERE ue.col = :u1_col and ue.col = u2.col JoinVars: u2.col│
└─────┘ └──┬───────────────────────────────────────────────────────┬───┘
         inner                                                   outer
        ┌──▼──┐                                                 ┌──▼──┐
        │R(ue)│                                                 │R(u2)│
        └─────┘                                                 └─────┘
We are rewriting all expressions in the subquery to use arguments any columns
coming from the LHS. The join predicate is not affected, but we are adding
any new columns needed by the inner subquery to the JoinVars that the join
will handle.
*/
func tryPushSubQueryInJoin(
	ctx *plancontext.PlanningContext,
	inner *SubQuery,
	outer *ApplyJoin,
) (ops.Operator, *rewrite.ApplyResult, error) {
	lhs := TableID(outer.LHS)
	rhs := TableID(outer.RHS)
	joinID := TableID(outer)
	innerID := TableID(inner.Subquery)

	// Deps are the dependencies of the merge predicates -
	// we want to push the subquery as close to its needs
	// as possible, so that we can potentially merge them together
	// TODO: we need to check dependencies and break apart all expressions in the subquery, not just the merge predicates
	deps := semantics.EmptyTableSet()
	for _, predicate := range inner.GetMergePredicates() {
		deps = deps.Merge(ctx.SemTable.RecursiveDeps(predicate))
	}
	deps = deps.Remove(innerID)

	// in general, we don't want to push down uncorrelated subqueries into the RHS of a join,
	// since this side is executed once per row from the LHS, so we would unnecessarily execute
	// the subquery multiple times. The exception is if we can merge the subquery with the RHS of the join.
	merged, result, err := tryMergeWithRHS(ctx, inner, outer)
	if err != nil {
		return nil, nil, err
	}
	if merged != nil {
		return merged, result, nil
	}

	_, ok := inner.Subquery.(*Projection)
	if ok {
		// This is a little hacky, but I could not find a better solution for it.
		// Projections are easy to push down, so if this is still at the top,
		// it means we have not tried pushing it yet.
		// Let's give it a chance to push down before we push it on the left
		return nil, rewrite.SameTree, nil
	}

	if deps.IsSolvedBy(lhs) {
		// we can safely push down the subquery on the LHS
		outer.LHS = addSubQuery(outer.LHS, inner)
		return outer, rewrite.NewTree("push subquery into LHS of join", inner), nil
	}

	if outer.LeftJoin || len(inner.Predicates) == 0 {
		// we can't push any filters on the RHS of an outer join, and
		// we don't want to push uncorrelated subqueries to the RHS of a join
		return nil, rewrite.SameTree, nil
	}

	if deps.IsSolvedBy(rhs) {
		// we can push down the subquery filter on RHS of the join
		outer.RHS = addSubQuery(outer.RHS, inner)
		return outer, rewrite.NewTree("push subquery into RHS of join", inner), nil
	}

	if deps.IsSolvedBy(joinID) {
		// we can rewrite the predicate to not use the values from the lhs,
		// and instead use arguments for these dependencies.
		// this way we can push the subquery into the RHS of this join
		err := inner.mapExpr(extractLHSExpr(ctx, outer, lhs))
		if err != nil {
			return nil, nil, err
		}

		outer.RHS = addSubQuery(outer.RHS, inner)
		return outer, rewrite.NewTree("push subquery into RHS of join rewriting predicates", inner), nil
	}

	return nil, rewrite.SameTree, nil
}

// extractLHSExpr will return a function that extracts any ColName coming from the LHS table,
// adding them to the ExtraLHSVars on the join if they are not already known
func extractLHSExpr(
	ctx *plancontext.PlanningContext,
	outer *ApplyJoin,
	lhs semantics.TableSet,
) func(expr sqlparser.Expr) (sqlparser.Expr, error) {
	return func(expr sqlparser.Expr) (sqlparser.Expr, error) {
		col, err := BreakExpressionInLHSandRHS(ctx, expr, lhs)
		if err != nil {
			return nil, err
		}
		if col.IsPureLeft() {
			return nil, vterrors.VT13001("did not expect to find any predicates that do not need data from the inner here")
		}
		for _, bve := range col.LHSExprs {
			if !outer.isColNameMovedFromL2R(bve.Name) {
				outer.ExtraLHSVars = append(outer.ExtraLHSVars, bve)
			}
		}
		return col.RHSExpr, nil
	}
}

// tryMergeWithRHS attempts to merge a subquery with the RHS of a join
func tryMergeWithRHS(ctx *plancontext.PlanningContext, inner *SubQuery, outer *ApplyJoin) (ops.Operator, *rewrite.ApplyResult, error) {
	if outer.LeftJoin {
		return nil, nil, nil
	}
	// both sides need to be routes
	outerRoute, ok := outer.RHS.(*Route)
	if !ok {
		return nil, nil, nil
	}
	innerRoute, ok := inner.Subquery.(*Route)
	if !ok {
		return nil, nil, nil
	}

	newExpr, err := rewriteOriginalPushedToRHS(ctx, inner.Original, outer)
	if err != nil {
		return nil, nil, err
	}
	sqm := &subqueryRouteMerger{
		outer:    outerRoute,
		original: newExpr,
		subq:     inner,
	}
	newOp, err := mergeSubqueryInputs(ctx, innerRoute, outerRoute, inner.GetMergePredicates(), sqm)
	if err != nil || newOp == nil {
		return nil, nil, err
	}

	outer.RHS = newOp
	ctx.MergedSubqueries = append(ctx.MergedSubqueries, inner.originalSubquery)
	return outer, rewrite.NewTree("merged subquery with rhs of join", inner), nil
}

// addSubQuery adds a SubQuery to the given operator. If the operator is a SubQueryContainer,
// it will add the SubQuery to the SubQueryContainer. If the operator is something else,	it will
// create a new SubQueryContainer with the given operator as the outer and the SubQuery as the inner.
func addSubQuery(in ops.Operator, inner *SubQuery) ops.Operator {
	sql, ok := in.(*SubQueryContainer)
	if !ok {
		return &SubQueryContainer{
			Outer: in,
			Inner: []*SubQuery{inner},
		}
	}

	sql.Inner = append(sql.Inner, inner)
	return sql
}

// rewriteOriginalPushedToRHS rewrites the original expression to use the argument names instead of the column names
// this is necessary because we are pushing the subquery into the RHS of the join, and we need to use the argument names
// instead of the column names
func rewriteOriginalPushedToRHS(ctx *plancontext.PlanningContext, expression sqlparser.Expr, outer *ApplyJoin) (sqlparser.Expr, error) {
	var err error
	outerID := TableID(outer.LHS)
	result := sqlparser.CopyOnRewrite(expression, nil, func(cursor *sqlparser.CopyOnWriteCursor) {
		col, ok := cursor.Node().(*sqlparser.ColName)
		if !ok || ctx.SemTable.RecursiveDeps(col) != outerID {
			// we are only interested in columns that are coming from the LHS of the join
			return
		}
		// this is a dependency we are being fed from the LHS of the join, so we
		// need to find the argument name for it and use that instead
		// we can't use the column name directly, because we're in the RHS of the join
		name, innerErr := outer.findOrAddColNameBindVarName(ctx, col)
		if err != nil {
			err = innerErr
			cursor.StopTreeWalk()
			return
		}
		cursor.Replace(sqlparser.NewArgument(name))
	}, nil)
	if err != nil {
		return nil, err
	}
	return result.(sqlparser.Expr), nil
}

func pushProjectionToOuterContainer(ctx *plancontext.PlanningContext, p *Projection, src *SubQueryContainer) (ops.Operator, *rewrite.ApplyResult, error) {
	ap, err := p.GetAliasedProjections()
	if err != nil {
		return p, rewrite.SameTree, nil
	}

	outer := TableID(src.Outer)
	for _, pe := range ap {
		_, isOffset := pe.Info.(*Offset)
		if isOffset {
			continue
		}

		if !ctx.SemTable.RecursiveDeps(pe.EvalExpr).IsSolvedBy(outer) {
			return p, rewrite.SameTree, nil
		}

		if se, ok := pe.Info.(SubQueryExpression); ok {
			pe.EvalExpr = rewriteColNameToArgument(ctx, pe.EvalExpr, se, src.Inner...)
		}
	}
	// all projections can be pushed to the outer
	src.Outer, p.Source = p, src.Outer
	return src, rewrite.NewTree("push projection into outer side of subquery container", p), nil
}

func rewriteColNameToArgument(ctx *plancontext.PlanningContext, in sqlparser.Expr, se SubQueryExpression, subqueries ...*SubQuery) sqlparser.Expr {
	rewriteIt := func(s string) sqlparser.SQLNode {
		for _, sq1 := range se {
			if sq1.ArgName != s && sq1.HasValuesName != s {
				continue
			}

			for _, sq2 := range subqueries {
				if s == sq2.ArgName {
					switch {
					case sq1.FilterType.NeedsListArg():
						return sqlparser.NewListArg(s)
					case sq1.FilterType == opcode.PulloutExists:
						if sq1.HasValuesName == "" {
							sq1.HasValuesName = ctx.ReservedVars.ReserveHasValuesSubQuery()
							sq2.HasValuesName = sq1.HasValuesName
						}
						return sqlparser.NewArgument(sq1.HasValuesName)
					default:
						return sqlparser.NewArgument(s)
					}
				}
			}
		}
		return nil
	}

	// replace the ColNames with Argument inside the subquery
	result := sqlparser.Rewrite(in, nil, func(cursor *sqlparser.Cursor) bool {
		col, ok := cursor.Node().(*sqlparser.ColName)
		if !ok || !col.Qualifier.IsEmpty() {
			return true
		}
		arg := rewriteIt(col.Name.String())
		if arg == nil {
			return true
		}
		cursor.Replace(arg)
		return true
	})
	return result.(sqlparser.Expr)
}

func pushOrMergeSubQueryContainer(ctx *plancontext.PlanningContext, in *SubQueryContainer) (ops.Operator, *rewrite.ApplyResult, error) {
	if !reachedPhase(ctx, initialPlanning) {
		return in, rewrite.SameTree, nil
	}

	var remaining []*SubQuery
	var result *rewrite.ApplyResult
	for _, inner := range in.Inner {
		newOuter, _result, err := pushOrMerge(ctx, in.Outer, inner)
		if err != nil {
			return nil, nil, err
		}
		if _result == rewrite.SameTree {
			remaining = append(remaining, inner)
			continue
		}

		in.Outer = newOuter
		result = result.Merge(_result)
	}

	if len(remaining) == 0 {
		return in.Outer, result, nil
	}

	in.Inner = remaining

	return in, result, nil
}

func tryMergeSubQuery(
	ctx *plancontext.PlanningContext,
	subQuery *SubQuery,
	outer *Route,
) (newOuter ops.Operator, result *rewrite.ApplyResult, err error) {
	switch inner := subQuery.Subquery.(type) {
	case *Route:
		return tryMergeSubqueryWithOuter(ctx, subQuery, outer, inner)
	case *SubQueryContainer:
		return tryMergeSubqueriesRecursively(ctx, subQuery, outer, inner)
	}
	return outer, rewrite.SameTree, nil
}

// tryMergeSubqueriesRecursively attempts to merge a SubQueryContainer with the outer Route.
func tryMergeSubqueriesRecursively(
	ctx *plancontext.PlanningContext,
	subQuery *SubQuery,
	outer *Route,
	inner *SubQueryContainer,
) (ops.Operator, *rewrite.ApplyResult, error) {
	exprs := subQuery.GetMergePredicates()
	merger := &subqueryRouteMerger{
		outer:    outer,
		original: subQuery.Original,
		subq:     subQuery,
	}
	op, err := mergeSubqueryInputs(ctx, inner.Outer, outer, exprs, merger)
	if err != nil {
		return nil, nil, err
	}
	if op == nil {
		return outer, rewrite.SameTree, nil
	}

	op = Clone(op).(*Route)
	op.Source = outer.Source
	var finalResult *rewrite.ApplyResult
	for _, subq := range inner.Inner {
		newOuter, res, err := tryMergeSubQuery(ctx, subq, op)
		if err != nil {
			return nil, nil, err
		}
		if res == rewrite.SameTree {
			// we failed to merge one of the inners - we need to abort
			return nil, rewrite.SameTree, nil
		}
		op = newOuter.(*Route)
		finalResult = finalResult.Merge(res)
	}

	op.Source = &Filter{Source: outer.Source, Predicates: []sqlparser.Expr{subQuery.Original}}
	return op, finalResult.Merge(rewrite.NewTree("merge outer of two subqueries", subQuery)), nil
}

func tryMergeSubqueryWithOuter(ctx *plancontext.PlanningContext, subQuery *SubQuery, outer *Route, inner ops.Operator) (ops.Operator, *rewrite.ApplyResult, error) {
	if updOp, ok := outer.Source.(*Update); ok && mergingIsBlocked(subQuery, updOp) {
		return outer, rewrite.SameTree, nil
	}
	exprs := subQuery.GetMergePredicates()
	merger := &subqueryRouteMerger{
		outer:    outer,
		original: subQuery.Original,
		subq:     subQuery,
	}
	op, err := mergeSubqueryInputs(ctx, inner, outer, exprs, merger)
	if err != nil {
		return nil, nil, err
	}
	if op == nil {
		return outer, rewrite.SameTree, nil
	}
	if !subQuery.IsProjection {
		op.Source = &Filter{Source: outer.Source, Predicates: []sqlparser.Expr{subQuery.Original}}
	}
	ctx.MergedSubqueries = append(ctx.MergedSubqueries, subQuery.originalSubquery)
	return op, rewrite.NewTree("merged subquery with outer", subQuery), nil
}

// This checked if subquery is part of the changed vindex values. Subquery cannot be merged with the outer route.
func mergingIsBlocked(subQuery *SubQuery, updOp *Update) bool {
	for _, sqArg := range updOp.SubQueriesArgOnChangedVindex {
		if sqArg == subQuery.ArgName {
			return true
		}
	}
	return false
}

func pushOrMerge(ctx *plancontext.PlanningContext, outer ops.Operator, inner *SubQuery) (ops.Operator, *rewrite.ApplyResult, error) {
	switch o := outer.(type) {
	case *Route:
		return tryMergeSubQuery(ctx, inner, o)
	case *ApplyJoin:
		join, applyResult, err := tryPushSubQueryInJoin(ctx, inner, o)
		if err != nil {
			return nil, nil, err
		}
		if join == nil {
			return outer, rewrite.SameTree, nil
		}
		return join, applyResult, nil
	default:
		return outer, rewrite.SameTree, nil
	}
}

type subqueryRouteMerger struct {
	outer    *Route
	original sqlparser.Expr
	subq     *SubQuery
}

func (s *subqueryRouteMerger) mergeShardedRouting(ctx *plancontext.PlanningContext, r1, r2 *ShardedRouting, old1, old2 *Route) (*Route, error) {
	tr := &ShardedRouting{
		VindexPreds: append(r1.VindexPreds, r2.VindexPreds...),
		keyspace:    r1.keyspace,
		RouteOpCode: r1.RouteOpCode,
	}

	if !s.subq.TopLevel {
		// if the subquery is not at the root level, we can't use it for routing, only for merging
		tr.SeenPredicates = r2.SeenPredicates
	} else {
		tr.SeenPredicates = slice.Filter(append(r1.SeenPredicates, r2.SeenPredicates...), func(expr sqlparser.Expr) bool {
			// There are two cases we can have - we can have predicates in the outer
			// that are no longer valid, and predicates in the inner that are no longer valid
			// For the case WHERE exists(select 1 from user where user.id = ue.user_id)
			// Outer: ::has_values
			// Inner: user.id = :ue_user_id
			//
			// And for the case WHERE id IN (select id FROM user WHERE id = 5)
			// Outer: id IN ::__sq1
			// Inner: id = 5
			//
			// We only keep SeenPredicates that are not bind variables in the join columns.
			// We have to remove the outer predicate since we merge both routes, and no one
			// is producing the bind variable anymore.
			if exprFromSubQ := ctx.SemTable.RecursiveDeps(expr).IsOverlapping(TableID(s.subq.Subquery)); !exprFromSubQ {
				return true
			}
			var argFound bool
			_ = sqlparser.Walk(func(node sqlparser.SQLNode) (kontinue bool, err error) {
				arg, ok := node.(*sqlparser.Argument)
				if !ok {
					return true, nil
				}
				f := func(bve BindVarExpr) bool { return bve.Name == arg.Name }
				for _, jc := range s.subq.JoinColumns {
					if slices.ContainsFunc(jc.LHSExprs, f) {
						argFound = true
						return false, io.EOF
					}
				}
				return true, nil
			}, expr)

			return !argFound
		})
	}

	routing, err := tr.resetRoutingLogic(ctx)
	if err != nil {
		return nil, err
	}
	return s.merge(ctx, old1, old2, routing)
}

func (s *subqueryRouteMerger) merge(ctx *plancontext.PlanningContext, inner, outer *Route, r Routing) (*Route, error) {
	if !s.subq.TopLevel {
		// if the subquery we are merging isn't a top level predicate, we can't use it for routing
		return &Route{
			Source:        outer.Source,
			MergedWith:    mergedWith(inner, outer),
			Routing:       outer.Routing,
			Ordering:      outer.Ordering,
			ResultColumns: outer.ResultColumns,
		}, nil

	}
	_, isSharded := r.(*ShardedRouting)
	var src ops.Operator
	var err error
	if isSharded {
		src = s.outer.Source
		if !s.subq.IsProjection {
			src = &Filter{
				Source:     s.outer.Source,
				Predicates: []sqlparser.Expr{s.original},
			}
		}
	} else {
		src, err = s.rewriteASTExpression(ctx, inner)
		if err != nil {
			return nil, err
		}
	}
	return &Route{
		Source:        src,
		MergedWith:    mergedWith(inner, outer),
		Routing:       r,
		Ordering:      s.outer.Ordering,
		ResultColumns: s.outer.ResultColumns,
	}, nil
}

// rewriteASTExpression rewrites the subquery expression that is used in the merged output
// Any changes that have been done to the operator tree since it was extracted from the
// query need make it to the expression
// TODO: systay 2023-09-26
// we should be able to use this method for all plan types,
// but using this method for sharded queries introduces bugs
// We really need to figure out why this is not working as expected
func (s *subqueryRouteMerger) rewriteASTExpression(ctx *plancontext.PlanningContext, inner *Route) (ops.Operator, error) {
	src := s.outer.Source
	stmt, _, err := ToSQL(ctx, inner.Source)
	if err != nil {
		return nil, err
	}
	subqStmt, ok := stmt.(sqlparser.SelectStatement)
	if !ok {
		return nil, vterrors.VT13001("subqueries should only be select statement")
	}
	subqID := TableID(s.subq.Subquery)
	subqStmt = sqlparser.CopyOnRewrite(subqStmt, nil, func(cursor *sqlparser.CopyOnWriteCursor) {
		arg, ok := cursor.Node().(*sqlparser.Argument)
		if !ok {
			return
		}
		var exprFound sqlparser.Expr
		for expr, argName := range ctx.ReservedArguments {
			if arg.Name == argName {
				exprFound = expr
			}
		}
		if exprFound == nil {
			return
		}
		deps := ctx.SemTable.RecursiveDeps(exprFound)
		if deps.IsEmpty() {
			err = vterrors.VT13001("found colname that we dont have deps for")
			cursor.StopTreeWalk()
			return
		}
		if !deps.IsSolvedBy(subqID) {
			cursor.Replace(exprFound)
		}
	}, nil).(sqlparser.SelectStatement)
	if err != nil {
		return nil, err
	}

	if s.subq.IsProjection {
		ctx.SemTable.CopySemanticInfo(s.subq.originalSubquery.Select, subqStmt)
		s.subq.originalSubquery.Select = subqStmt
	} else {
		sQuery := sqlparser.CopyOnRewrite(s.original, dontEnterSubqueries, func(cursor *sqlparser.CopyOnWriteCursor) {
			if subq, ok := cursor.Node().(*sqlparser.Subquery); ok {
				subq.Select = subqStmt
				cursor.Replace(subq)
			}
		}, ctx.SemTable.CopySemanticInfo).(sqlparser.Expr)
		src = &Filter{
			Source:     s.outer.Source,
			Predicates: []sqlparser.Expr{sQuery},
		}
	}
	return src, nil
}

// mergeSubqueryInputs checks whether two operators can be merged into a single one.
// If they can be merged, a new operator with the merged routing is returned
// If they cannot be merged, nil is returned.
// These rules are similar but different from join merging
func mergeSubqueryInputs(ctx *plancontext.PlanningContext, in, out ops.Operator, joinPredicates []sqlparser.Expr, m *subqueryRouteMerger) (*Route, error) {
	inRoute, outRoute := operatorsToRoutes(in, out)
	if inRoute == nil || outRoute == nil {
		return nil, nil
	}

	inRoute, outRoute, inRouting, outRouting, sameKeyspace := getRoutesOrAlternates(inRoute, outRoute)
	inner, outer := getRoutingType(inRouting), getRoutingType(outRouting)

	switch {
	// We have to let the outer control how many rows are returned,
	// which means that we have to be careful with merging when the outer side
	case inner == dual ||
		(inner == anyShard && sameKeyspace):
		return m.merge(ctx, inRoute, outRoute, outRouting)

	case inner == none && sameKeyspace:
		return m.merge(ctx, inRoute, outRoute, inRouting)

	// we can merge dual-outer subqueries only if the
	// inner is guaranteed to hit a single shard
	case inRoute.IsSingleShard() &&
		(outer == dual || (outer == anyShard && sameKeyspace)):
		return m.merge(ctx, inRoute, outRoute, inRouting)

	case outer == none && sameKeyspace:
		return m.merge(ctx, inRoute, outRoute, outRouting)

	// infoSchema routing is complex, so we handle it in a separate method
	case inner == infoSchema && outer == infoSchema:
		return tryMergeInfoSchemaRoutings(ctx, inRouting, outRouting, m, inRoute, outRoute)

	// sharded routing is complex, so we handle it in a separate method
	case inner == sharded && outer == sharded:
		return tryMergeJoinShardedRouting(ctx, inRoute, outRoute, m, joinPredicates)

	default:
		return nil, nil
	}
}

func mergedWith(inner *Route, outer *Route) []*Route {
	mergedWith := append(inner.MergedWith, inner, outer)
	mergedWith = append(mergedWith, outer.MergedWith...)
	return mergedWith
}

var _ merger = (*subqueryRouteMerger)(nil)
