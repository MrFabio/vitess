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

package planbuilder

import (
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vtgate/engine"
	"vitess.io/vitess/go/vt/vtgate/planbuilder/plancontext"
	"vitess.io/vitess/go/vt/vtgate/semantics"
)

var _ logicalPlan = (*fkCascade)(nil)

// fkCascade is the logicalPlan for engine.FkCascade.
type fkCascade struct {
	parent    logicalPlan
	selection logicalPlan
	children  []*engine.FkChild
}

// newFkCascade builds a new fkCascade.
func newFkCascade(parent, selection logicalPlan, children []*engine.FkChild) *fkCascade {
	return &fkCascade{
		parent:    parent,
		selection: selection,
		children:  children,
	}
}

// Primitive implements the logicalPlan interface
func (fkc *fkCascade) Primitive() engine.Primitive {
	return &engine.FkCascade{
		Parent:    fkc.parent.Primitive(),
		Selection: fkc.selection.Primitive(),
		Children:  fkc.children,
	}
}

// Wireup implements the logicalPlan interface
func (fkc *fkCascade) Wireup(ctx *plancontext.PlanningContext) error {
	if err := fkc.parent.Wireup(ctx); err != nil {
		return err
	}
	return fkc.selection.Wireup(ctx)
}

// Rewrite implements the logicalPlan interface
func (fkc *fkCascade) Rewrite(inputs ...logicalPlan) error {
	if len(inputs) != 2 {
		return vterrors.VT13001("fkCascade: wrong number of inputs")
	}
	fkc.parent = inputs[0]
	fkc.selection = inputs[1]
	return nil
}

// ContainsTables implements the logicalPlan interface
func (fkc *fkCascade) ContainsTables() semantics.TableSet {
	return fkc.parent.ContainsTables()
}

// Inputs implements the logicalPlan interface
func (fkc *fkCascade) Inputs() []logicalPlan {
	return []logicalPlan{fkc.parent, fkc.selection}
}

// OutputColumns implements the logicalPlan interface
func (fkc *fkCascade) OutputColumns() []sqlparser.SelectExpr {
	return nil
}
