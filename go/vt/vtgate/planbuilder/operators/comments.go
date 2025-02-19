/*
Copyright 2023 The Vitess Authors.

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

	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vtgate/planbuilder/operators/ops"
	"vitess.io/vitess/go/vt/vtgate/planbuilder/plancontext"
)

// LockAndComment contains any comments or locking directives we want on all queries down from this operator
type LockAndComment struct {
	Source   ops.Operator
	Comments *sqlparser.ParsedComments
	Lock     sqlparser.Lock
}

func (l *LockAndComment) Clone(inputs []ops.Operator) ops.Operator {
	klon := *l
	klon.Source = inputs[0]
	return &klon
}

func (l *LockAndComment) Inputs() []ops.Operator {
	return []ops.Operator{l.Source}
}

func (l *LockAndComment) SetInputs(operators []ops.Operator) {
	l.Source = operators[0]
}

func (l *LockAndComment) AddPredicate(ctx *plancontext.PlanningContext, expr sqlparser.Expr) (ops.Operator, error) {
	newSrc, err := l.Source.AddPredicate(ctx, expr)
	if err != nil {
		return nil, err
	}
	l.Source = newSrc
	return l, nil
}

func (l *LockAndComment) AddColumn(ctx *plancontext.PlanningContext, reuseExisting bool, addToGroupBy bool, expr *sqlparser.AliasedExpr) (int, error) {
	return l.Source.AddColumn(ctx, reuseExisting, addToGroupBy, expr)
}

func (l *LockAndComment) FindCol(ctx *plancontext.PlanningContext, expr sqlparser.Expr, underRoute bool) (int, error) {
	return l.Source.FindCol(ctx, expr, underRoute)
}

func (l *LockAndComment) GetColumns(ctx *plancontext.PlanningContext) ([]*sqlparser.AliasedExpr, error) {
	return l.Source.GetColumns(ctx)
}

func (l *LockAndComment) GetSelectExprs(ctx *plancontext.PlanningContext) (sqlparser.SelectExprs, error) {
	return l.Source.GetSelectExprs(ctx)
}

func (l *LockAndComment) ShortDescription() string {
	s := slices.Clone(l.Comments.GetComments())
	if l.Lock != sqlparser.NoLock {
		s = append(s, l.Lock.ToString())
	}

	return strings.Join(s, " ")
}

func (l *LockAndComment) GetOrdering() ([]ops.OrderBy, error) {
	return l.Source.GetOrdering()
}
