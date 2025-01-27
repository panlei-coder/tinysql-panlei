// Copyright 2018 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package cascades

import (
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/expression/aggregation"
	plannercore "github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/planner/memo"
	"github.com/pingcap/tidb/util/ranger"
)

// Transformation defines the interface for the transformation rules.
type Transformation interface {
	// GetPattern gets the cached pattern of the rule.
	GetPattern() *memo.Pattern
	// Match is used to check whether the GroupExpr satisfies all the requirements of the transformation rule.
	//
	// The pattern only identifies the operator type, some transformation rules also need
	// detailed information for certain plan operators to decide whether it is applicable.
	Match(expr *memo.ExprIter) bool
	// OnTransform does the real work of the optimization rule.
	//
	// newExprs indicates the new GroupExprs generated by the transformationrule. Multiple GroupExprs may be
	// returned, e.g, EnumeratePath would convert DataSource to several possible assess paths.
	//
	// eraseOld indicates that the returned GroupExpr must be better than the old one, so we can remove it from Group.
	//
	// eraseAll indicates that the returned GroupExpr must be better than all other candidates in the Group, e.g, we can
	// prune all other access paths if we found the filter is constantly false.
	OnTransform(old *memo.ExprIter) (newExprs []*memo.GroupExpr, eraseOld bool, eraseAll bool, err error)
}

var defaultTransformationMap = map[memo.Operand][]Transformation{
	memo.OperandSelection: {
		NewRulePushSelDownTableScan(),
		NewRulePushSelDownTiKVSingleGather(),
		NewRulePushSelDownSort(),
		NewRulePushSelDownProjection(),
		NewRulePushSelDownAggregation(),
		NewRulePushSelDownJoin(),
		NewRulePushSelDownIndexScan(),
	},
	memo.OperandDataSource: {
		NewRuleEnumeratePaths(),
	},
	memo.OperandAggregation: {
		NewRulePushAggDownGather(),
		NewRuleMergeAggregationProjection(),
	},
	memo.OperandLimit: {
		NewRuleTransformLimitToTopN(),
	},
	memo.OperandProjection: {
		NewRuleEliminateProjection(),
		NewRuleMergeAdjacentProjection(),
	},
	memo.OperandTopN: {
		NewRulePushTopNDownProjection(),
	},
}

type baseRule struct {
	pattern *memo.Pattern
}

// Match implements Transformation Interface.
func (r *baseRule) Match(expr *memo.ExprIter) bool {
	return true
}

// GetPattern implements Transformation Interface.
func (r *baseRule) GetPattern() *memo.Pattern {
	return r.pattern
}

// PushSelDownTableScan pushes the selection down to TableScan.
type PushSelDownTableScan struct {
	baseRule
}

// NewRulePushSelDownTableScan creates a new Transformation PushSelDownTableScan.
// The pattern of this rule is: `Selection -> TableScan`
func NewRulePushSelDownTableScan() Transformation {
	rule := &PushSelDownTableScan{}
	ts := memo.NewPattern(memo.OperandTableScan, memo.EngineTiKVOnly)
	p := memo.BuildPattern(memo.OperandSelection, memo.EngineTiKVOnly, ts)
	rule.pattern = p
	return rule
}

// OnTransform implements Transformation interface.
//
// It transforms `sel -> ts` to one of the following new exprs:
// 1. `newSel -> newTS`
// 2. `newTS`
//
// Filters of the old `sel` operator are removed if they are used to calculate
// the key ranges of the `ts` operator.
func (r *PushSelDownTableScan) OnTransform(old *memo.ExprIter) (newExprs []*memo.GroupExpr, eraseOld bool, eraseAll bool, err error) {
	sel := old.GetExpr().ExprNode.(*plannercore.LogicalSelection)
	ts := old.Children[0].GetExpr().ExprNode.(*plannercore.LogicalTableScan)
	if ts.Handle == nil {
		return nil, false, false, nil
	}
	accesses, remained := ranger.DetachCondsForColumn(ts.SCtx(), sel.Conditions, ts.Handle)
	if accesses == nil {
		return nil, false, false, nil
	}
	newTblScan := plannercore.LogicalTableScan{
		Source:      ts.Source,
		Handle:      ts.Handle,
		AccessConds: ts.AccessConds.Shallow(),
	}.Init(ts.SCtx())
	newTblScan.AccessConds = append(newTblScan.AccessConds, accesses...)
	tblScanExpr := memo.NewGroupExpr(newTblScan)
	if len(remained) == 0 {
		// `sel -> ts` is transformed to `newTS`.
		return []*memo.GroupExpr{tblScanExpr}, true, false, nil
	}
	schema := old.GetExpr().Group.Prop.Schema
	tblScanGroup := memo.NewGroupWithSchema(tblScanExpr, schema)
	newSel := plannercore.LogicalSelection{Conditions: remained}.Init(sel.SCtx())
	selExpr := memo.NewGroupExpr(newSel)
	selExpr.Children = append(selExpr.Children, tblScanGroup)
	// `sel -> ts` is transformed to `newSel ->newTS`.
	return []*memo.GroupExpr{selExpr}, true, false, nil
}

// PushSelDownIndexScan pushes a Selection down to IndexScan.
type PushSelDownIndexScan struct {
	baseRule
}

// NewRulePushSelDownIndexScan creates a new Transformation PushSelDownIndexScan.
// The pattern of this rule is `Selection -> IndexScan`.
func NewRulePushSelDownIndexScan() Transformation {
	rule := &PushSelDownIndexScan{}
	rule.pattern = memo.BuildPattern(
		memo.OperandSelection,
		memo.EngineTiKVOnly,
		memo.NewPattern(memo.OperandIndexScan, memo.EngineTiKVOnly),
	)
	return rule
}

// OnTransform implements Transformation interface.
// It will transform `Selection -> IndexScan` to:
//   `IndexScan(with a new access range)` or
//   `Selection -> IndexScan(with a new access range)`
//	 or just keep the two GroupExprs unchanged.
func (r *PushSelDownIndexScan) OnTransform(old *memo.ExprIter) (newExprs []*memo.GroupExpr, eraseOld bool, eraseAll bool, err error) {
	sel := old.GetExpr().ExprNode.(*plannercore.LogicalSelection)
	is := old.Children[0].GetExpr().ExprNode.(*plannercore.LogicalIndexScan)
	if len(is.IdxCols) == 0 {
		return nil, false, false, nil
	}
	conditions := sel.Conditions
	if is.AccessConds != nil {
		// If we have already pushed some conditions down here,
		// we merge old AccessConds with new conditions,
		// to make sure this rule can be applied more than once.
		conditions = make([]expression.Expression, len(sel.Conditions)+len(is.AccessConds))
		copy(conditions, sel.Conditions)
		copy(conditions[len(sel.Conditions):], is.AccessConds)
	}
	res, err := ranger.DetachCondAndBuildRangeForIndex(is.SCtx(), conditions, is.IdxCols, is.IdxColLens)
	if err != nil {
		return nil, false, false, err
	}
	if len(res.AccessConds) == len(is.AccessConds) {
		// There is no condition can be pushed down as range,
		// or the pushed down conditions are the same with before.
		sameConds := true
		for i := range res.AccessConds {
			if !res.AccessConds[i].Equal(is.SCtx(), is.AccessConds[i]) {
				sameConds = false
				break
			}
		}
		if sameConds {
			return nil, false, false, nil
		}
	}
	// TODO: `res` still has some unused fields: EqOrInCount, IsDNFCond.
	newIs := plannercore.LogicalIndexScan{
		Source:         is.Source,
		IsDoubleRead:   is.IsDoubleRead,
		EqCondCount:    res.EqCondCount,
		AccessConds:    res.AccessConds,
		Ranges:         res.Ranges,
		Index:          is.Index,
		Columns:        is.Columns,
		FullIdxCols:    is.FullIdxCols,
		FullIdxColLens: is.FullIdxColLens,
		IdxCols:        is.IdxCols,
		IdxColLens:     is.IdxColLens,
	}.Init(is.SCtx())
	isExpr := memo.NewGroupExpr(newIs)

	if len(res.RemainedConds) == 0 {
		return []*memo.GroupExpr{isExpr}, true, false, nil
	}
	isGroup := memo.NewGroupWithSchema(isExpr, old.Children[0].GetExpr().Group.Prop.Schema)
	newSel := plannercore.LogicalSelection{Conditions: res.RemainedConds}.Init(sel.SCtx())
	selExpr := memo.NewGroupExpr(newSel)
	selExpr.SetChildren(isGroup)
	return []*memo.GroupExpr{selExpr}, true, false, nil
}

// PushSelDownTiKVSingleGather pushes the selection down to child of TiKVSingleGather.
type PushSelDownTiKVSingleGather struct {
	baseRule
}

// NewRulePushSelDownTiKVSingleGather creates a new Transformation PushSelDownTiKVSingleGather.
// The pattern of this rule is `Selection -> TiKVSingleGather -> Any`.
func NewRulePushSelDownTiKVSingleGather() Transformation {
	any := memo.NewPattern(memo.OperandAny, memo.EngineTiKVOnly)
	tg := memo.BuildPattern(memo.OperandTiKVSingleGather, memo.EngineTiDBOnly, any)
	p := memo.BuildPattern(memo.OperandSelection, memo.EngineTiDBOnly, tg)

	rule := &PushSelDownTiKVSingleGather{}
	rule.pattern = p
	return rule
}

// OnTransform implements Transformation interface.
//
// It transforms `oldSel -> oldTg -> any` to one of the following new exprs:
// 1. `newTg -> pushedSel -> any`
// 2. `remainedSel -> newTg -> pushedSel -> any`
func (r *PushSelDownTiKVSingleGather) OnTransform(old *memo.ExprIter) (newExprs []*memo.GroupExpr, eraseOld bool, eraseAll bool, err error) {
	sel := old.GetExpr().ExprNode.(*plannercore.LogicalSelection)
	sg := old.Children[0].GetExpr().ExprNode.(*plannercore.TiKVSingleGather)
	childGroup := old.Children[0].Children[0].Group
	var pushed, remained []expression.Expression
	sctx := sg.SCtx()
	_, pushed, remained = expression.ExpressionsToPB(sctx.GetSessionVars().StmtCtx, sel.Conditions, sctx.GetClient())
	if len(pushed) == 0 {
		return nil, false, false, nil
	}
	pushedSel := plannercore.LogicalSelection{Conditions: pushed}.Init(sctx)
	pushedSelExpr := memo.NewGroupExpr(pushedSel)
	pushedSelExpr.Children = append(pushedSelExpr.Children, childGroup)
	pushedSelGroup := memo.NewGroupWithSchema(pushedSelExpr, childGroup.Prop.Schema).SetEngineType(childGroup.EngineType)
	// The field content of TiKVSingleGather would not be modified currently, so we
	// just reference the same tg instead of making a copy of it.
	//
	// TODO: if we save pushed filters later in TiKVSingleGather, in order to do partition
	//       pruning or skyline pruning, we need to make a copy of the TiKVSingleGather here.
	tblGatherExpr := memo.NewGroupExpr(sg)
	tblGatherExpr.Children = append(tblGatherExpr.Children, pushedSelGroup)
	if len(remained) == 0 {
		// `oldSel -> oldTg -> any` is transformed to `newTg -> pushedSel -> any`.
		return []*memo.GroupExpr{tblGatherExpr}, true, false, nil
	}
	tblGatherGroup := memo.NewGroupWithSchema(tblGatherExpr, pushedSelGroup.Prop.Schema)
	remainedSel := plannercore.LogicalSelection{Conditions: remained}.Init(sel.SCtx())
	remainedSelExpr := memo.NewGroupExpr(remainedSel)
	remainedSelExpr.Children = append(remainedSelExpr.Children, tblGatherGroup)
	// `oldSel -> oldTg -> any` is transformed to `remainedSel -> newTg -> pushedSel -> any`.
	return []*memo.GroupExpr{remainedSelExpr}, true, false, nil
}

// EnumeratePaths converts DataSource to table scan and index scans.
type EnumeratePaths struct {
	baseRule
}

// NewRuleEnumeratePaths creates a new Transformation EnumeratePaths.
// The pattern of this rule is: `DataSource`.
func NewRuleEnumeratePaths() Transformation {
	rule := &EnumeratePaths{}
	rule.pattern = memo.NewPattern(memo.OperandDataSource, memo.EngineTiDBOnly)
	return rule
}

// OnTransform implements Transformation interface.
func (r *EnumeratePaths) OnTransform(old *memo.ExprIter) (newExprs []*memo.GroupExpr, eraseOld bool, eraseAll bool, err error) {
	ds := old.GetExpr().ExprNode.(*plannercore.DataSource)
	gathers := ds.Convert2Gathers()
	for _, gather := range gathers {
		expr := memo.Convert2GroupExpr(gather)
		expr.Children[0].SetEngineType(memo.EngineTiKV)
		newExprs = append(newExprs, expr)
	}
	return newExprs, true, false, nil
}

// PushAggDownGather splits Aggregation to two stages, final and partial1,
// and pushed the partial Aggregation down to the child of TiKVSingleGather.
type PushAggDownGather struct {
	baseRule
}

// NewRulePushAggDownGather creates a new Transformation PushAggDownGather.
// The pattern of this rule is: `Aggregation -> TiKVSingleGather`.
func NewRulePushAggDownGather() Transformation {
	rule := &PushAggDownGather{}
	rule.pattern = memo.BuildPattern(
		memo.OperandAggregation,
		memo.EngineTiDBOnly,
		memo.NewPattern(memo.OperandTiKVSingleGather, memo.EngineTiDBOnly),
	)
	return rule
}

// Match implements Transformation interface.
func (r *PushAggDownGather) Match(expr *memo.ExprIter) bool {
	agg := expr.GetExpr().ExprNode.(*plannercore.LogicalAggregation)
	for _, aggFunc := range agg.AggFuncs {
		if aggFunc.Mode != aggregation.CompleteMode {
			return false
		}
	}
	return plannercore.CheckAggCanPushCop(agg.SCtx(), agg.AggFuncs, agg.GroupByItems)
}

// OnTransform implements Transformation interface.
// It will transform `Agg->Gather` to `Agg(Final) -> Gather -> Agg(Partial1)`.
func (r *PushAggDownGather) OnTransform(old *memo.ExprIter) (newExprs []*memo.GroupExpr, eraseOld bool, eraseAll bool, err error) {
	agg := old.GetExpr().ExprNode.(*plannercore.LogicalAggregation)
	aggSchema := old.GetExpr().Group.Prop.Schema
	gather := old.Children[0].GetExpr().ExprNode.(*plannercore.TiKVSingleGather)
	childGroup := old.Children[0].GetExpr().Children[0]
	// The old Aggregation should stay unchanged for other transformation.
	// So we build a new LogicalAggregation for the partialAgg.
	partialAggFuncs := make([]*aggregation.AggFuncDesc, len(agg.AggFuncs))
	for i, aggFunc := range agg.AggFuncs {
		newAggFunc := &aggregation.AggFuncDesc{
			Mode: aggregation.Partial1Mode,
		}
		newAggFunc.Name = aggFunc.Name
		newAggFunc.RetTp = aggFunc.RetTp
		// The args will be changed below, so that we have to build a new slice for it.
		newArgs := make([]expression.Expression, len(aggFunc.Args))
		copy(newArgs, aggFunc.Args)
		newAggFunc.Args = newArgs
		partialAggFuncs[i] = newAggFunc
	}
	partialGbyItems := make([]expression.Expression, len(agg.GroupByItems))
	copy(partialGbyItems, agg.GroupByItems)
	partialAgg := plannercore.LogicalAggregation{
		AggFuncs:     partialAggFuncs,
		GroupByItems: partialGbyItems,
	}.Init(agg.SCtx())

	finalAggFuncs, finalGbyItems, partialSchema :=
		plannercore.BuildFinalModeAggregation(partialAgg.SCtx(), partialAgg.AggFuncs, partialAgg.GroupByItems, aggSchema)
	// Remove unnecessary FirstRow.
	partialAgg.AggFuncs =
		plannercore.RemoveUnnecessaryFirstRow(partialAgg.SCtx(), finalAggFuncs, finalGbyItems, partialAgg.AggFuncs, partialAgg.GroupByItems, partialSchema)
	finalAgg := plannercore.LogicalAggregation{
		AggFuncs:     finalAggFuncs,
		GroupByItems: finalGbyItems,
	}.Init(agg.SCtx())

	partialAggExpr := memo.NewGroupExpr(partialAgg)
	partialAggExpr.SetChildren(childGroup)
	partialAggGroup := memo.NewGroupWithSchema(partialAggExpr, partialSchema).SetEngineType(childGroup.EngineType)
	gatherExpr := memo.NewGroupExpr(gather)
	gatherExpr.SetChildren(partialAggGroup)
	gatherGroup := memo.NewGroupWithSchema(gatherExpr, partialSchema)
	finalAggExpr := memo.NewGroupExpr(finalAgg)
	finalAggExpr.SetChildren(gatherGroup)
	// We don't erase the old complete mode Aggregation because
	// this transformation would not always be better.
	return []*memo.GroupExpr{finalAggExpr}, false, false, nil
}

// PushSelDownSort pushes the Selection down to the child of Sort.
type PushSelDownSort struct {
	baseRule
}

// NewRulePushSelDownSort creates a new Transformation PushSelDownSort.
// The pattern of this rule is: `Selection -> Sort`.
func NewRulePushSelDownSort() Transformation {
	rule := &PushSelDownSort{}
	rule.pattern = memo.BuildPattern(
		memo.OperandSelection,
		memo.EngineTiDBOnly,
		memo.NewPattern(memo.OperandSort, memo.EngineTiDBOnly),
	)
	return rule
}

// OnTransform implements Transformation interface.
// It will transform `sel->sort->x` to `sort->sel->x`.
func (r *PushSelDownSort) OnTransform(old *memo.ExprIter) (newExprs []*memo.GroupExpr, eraseOld bool, eraseAll bool, err error) {
	sel := old.GetExpr().ExprNode.(*plannercore.LogicalSelection)
	sort := old.Children[0].GetExpr().ExprNode.(*plannercore.LogicalSort)
	childGroup := old.Children[0].GetExpr().Children[0]

	newSelExpr := memo.NewGroupExpr(sel)
	newSelExpr.Children = append(newSelExpr.Children, childGroup)
	newSelGroup := memo.NewGroupWithSchema(newSelExpr, childGroup.Prop.Schema)

	newSortExpr := memo.NewGroupExpr(sort)
	newSortExpr.Children = append(newSortExpr.Children, newSelGroup)
	return []*memo.GroupExpr{newSortExpr}, true, false, nil
}

// PushSelDownProjection pushes the Selection down to the child of Projection.
type PushSelDownProjection struct {
	baseRule
}

// NewRulePushSelDownProjection creates a new Transformation PushSelDownProjection.
// The pattern of this rule is: `Selection -> Projection`.
func NewRulePushSelDownProjection() Transformation {
	rule := &PushSelDownProjection{}
	rule.pattern = memo.BuildPattern(
		memo.OperandSelection,
		memo.EngineTiDBOnly,
		memo.NewPattern(memo.OperandProjection, memo.EngineTiDBOnly),
	)
	return rule
}

// OnTransform implements Transformation interface.
// It will transform `selection -> projection -> x` to
// 1. `projection -> selection -> x` or
// 2. `selection -> projection -> selection -> x` or
// 3. just keep unchanged.
func (r *PushSelDownProjection) OnTransform(old *memo.ExprIter) (newExprs []*memo.GroupExpr, eraseOld bool, eraseAll bool, err error) {
	sel := old.GetExpr().ExprNode.(*plannercore.LogicalSelection)
	proj := old.Children[0].GetExpr().ExprNode.(*plannercore.LogicalProjection)
	projSchema := old.Children[0].Prop.Schema
	childGroup := old.Children[0].GetExpr().Children[0]
	for _, expr := range proj.Exprs {
		if expression.HasAssignSetVarFunc(expr) {
			return nil, false, false, nil
		}
	}
	canBePushed := make([]expression.Expression, 0, len(sel.Conditions))
	canNotBePushed := make([]expression.Expression, 0, len(sel.Conditions))
	for _, cond := range sel.Conditions {
		if !expression.HasGetSetVarFunc(cond) {
			canBePushed = append(canBePushed, expression.ColumnSubstitute(cond, projSchema, proj.Exprs))
		} else {
			canNotBePushed = append(canNotBePushed, cond)
		}
	}
	if len(canBePushed) == 0 {
		return nil, false, false, nil
	}
	newBottomSel := plannercore.LogicalSelection{Conditions: canBePushed}.Init(sel.SCtx())
	newBottomSelExpr := memo.NewGroupExpr(newBottomSel)
	newBottomSelExpr.SetChildren(childGroup)
	newBottomSelGroup := memo.NewGroupWithSchema(newBottomSelExpr, childGroup.Prop.Schema)
	newProjExpr := memo.NewGroupExpr(proj)
	newProjExpr.SetChildren(newBottomSelGroup)
	if len(canNotBePushed) == 0 {
		return []*memo.GroupExpr{newProjExpr}, true, false, nil
	}
	newProjGroup := memo.NewGroupWithSchema(newProjExpr, projSchema)
	newTopSel := plannercore.LogicalSelection{Conditions: canNotBePushed}.Init(sel.SCtx())
	newTopSelExpr := memo.NewGroupExpr(newTopSel)
	newTopSelExpr.SetChildren(newProjGroup)
	return []*memo.GroupExpr{newTopSelExpr}, true, false, nil
}

// PushSelDownAggregation pushes Selection down to the child of Aggregation.
type PushSelDownAggregation struct {
	baseRule
}

// NewRulePushSelDownAggregation creates a new Transformation PushSelDownAggregation.
// The pattern of this rule is `Selection -> Aggregation`.
func NewRulePushSelDownAggregation() Transformation {
	rule := &PushSelDownAggregation{}
	rule.pattern = memo.BuildPattern(
		memo.OperandSelection,
		memo.EngineAll,
		memo.NewPattern(memo.OperandAggregation, memo.EngineAll),
	)
	return rule
}

// OnTransform implements Transformation interface.
// It will transform `sel->agg->x` to `agg->sel->x` or `sel->agg->sel->x`
// or just keep the selection unchanged.
/*
主要实现从sel->agg->x到agg->sel->x或者sel->agg->sel->x的变换。
当sel中的条件都存在于agg中就可以完全下推到agg之后（前者），否则就是后者。
具体体现是判断sel中每一个condition的列是否存在于agg的groupby的列当中，
并做下记录。存在的列可以下推到agg之下，剩余的保留在原位置。
*/
func (r *PushSelDownAggregation) OnTransform(old *memo.ExprIter) (newExprs []*memo.GroupExpr, eraseOld bool, eraseAll bool, err error) {
	// TODO: implement the algo according to the header comment.
	sel := old.GetExpr().ExprNode.(*plannercore.LogicalSelection)               // sel的节点
	agg := old.Children[0].GetExpr().ExprNode.(*plannercore.LogicalAggregation) // agg的节点
	aggSchema := old.Children[0].Prop.Schema

	var pushedExprs []expression.Expression                         // 被下推的列
	var remainedExprs []expression.Expression                       // 被保留的列
	groupByColumns := expression.NewSchema(agg.GetGroupByCols()...) // agg函数的列
	for _, cond := range sel.Conditions {                           // 依次遍历sel的列，如果是Constant则既保留也下推
		switch cond.(type) {
		case *expression.Constant:
			// Consider SQL list "select sum(b) from t group by a having 1=0". "1=0" is a constant predicate which should be
			// retained and pushed down at the same time. Because we will get a wrong query result that contains one column
			// with value 0 rather than an empty query result.
			pushedExprs = append(pushedExprs, cond)
			remainedExprs = append(remainedExprs, cond)
		case *expression.ScalarFunction: // 如果是ScalarFunction，则需要判断当前的列是否是包含在groupby中，如果包含的话，就下推，不包含就保留
			extractedCols := expression.ExtractColumns(cond)
			canPush := true
			for _, col := range extractedCols {
				if !groupByColumns.Contains(col) {
					canPush = false
					break
				}
			}
			if canPush {
				pushedExprs = append(pushedExprs, cond)
			} else {
				remainedExprs = append(remainedExprs, cond)
			}
		default:
			remainedExprs = append(remainedExprs, cond)
		}
	}
	// If no condition can be pushed, keep the selection unchanged.
	// 如果没有需要被下推的，则不做任何改变
	if len(pushedExprs) == 0 {
		return nil, false, false, nil
	}

	// 如果有需要被下推的，则创建一个sel添加到agg之后
	// 从sel->agg->x到agg->sel->x
	sctx := sel.SCtx()
	childGroup := old.Children[0].GetExpr().Children[0] // 获取sel的孩子节点的孩子节点，即agg的孩子节点，后续将新生成的sel（下推的）添加到agg之后
	pushedSel := plannercore.LogicalSelection{Conditions: pushedExprs}.Init(sctx)
	pushedGroupExpr := memo.NewGroupExpr(pushedSel)
	// 更新新生成的sel（下推的）的孩子
	pushedGroupExpr.SetChildren(childGroup)
	pushedGroup := memo.NewGroupWithSchema(pushedGroupExpr, childGroup.Prop.Schema)
	// 更新agg的孩子为新生成的sel（下推的）
	aggGroupExpr := memo.NewGroupExpr(agg)
	aggGroupExpr.SetChildren(pushedGroup)

	// 如果没有需要保留的列，则将直接返回以agg为根的memo
	if len(remainedExprs) == 0 {
		return []*memo.GroupExpr{aggGroupExpr}, true, false, nil
	}

	// 如果有保留的话，则需要将保留的列重新生成sel，并将agg设置成新的sel（保留的）的孩子
	// 从sel->agg->x到sel->agg->sel->x
	aggGroup := memo.NewGroupWithSchema(aggGroupExpr, aggSchema)
	remainedSel := plannercore.LogicalSelection{Conditions: remainedExprs}.Init(sctx)
	remainedGroupExpr := memo.NewGroupExpr(remainedSel)
	remainedGroupExpr.SetChildren(aggGroup)
	return []*memo.GroupExpr{remainedGroupExpr}, true, false, nil
}

// TransformLimitToTopN transforms Limit+Sort to TopN.
type TransformLimitToTopN struct {
	baseRule
}

// NewRuleTransformLimitToTopN creates a new Transformation TransformLimitToTopN.
// The pattern of this rule is `Limit -> Sort`.
func NewRuleTransformLimitToTopN() Transformation {
	rule := &TransformLimitToTopN{}
	rule.pattern = memo.BuildPattern(
		memo.OperandLimit,
		memo.EngineTiDBOnly,
		memo.NewPattern(memo.OperandSort, memo.EngineTiDBOnly),
	)
	return rule
}

// OnTransform implements Transformation interface.
// This rule will transform `Limit -> Sort -> x` to `TopN -> x`.
func (r *TransformLimitToTopN) OnTransform(old *memo.ExprIter) (newExprs []*memo.GroupExpr, eraseOld bool, eraseAll bool, err error) {
	limit := old.GetExpr().ExprNode.(*plannercore.LogicalLimit)
	sort := old.Children[0].GetExpr().ExprNode.(*plannercore.LogicalSort)
	childGroup := old.Children[0].GetExpr().Children[0]
	topN := plannercore.LogicalTopN{
		ByItems: sort.ByItems,
		Offset:  limit.Offset,
		Count:   limit.Count,
	}.Init(limit.SCtx())
	topNExpr := memo.NewGroupExpr(topN)
	topNExpr.SetChildren(childGroup)
	return []*memo.GroupExpr{topNExpr}, true, false, nil
}

// PushSelDownJoin pushes Selection through Join.
type PushSelDownJoin struct {
	baseRule
}

// NewRulePushSelDownJoin creates a new Transformation PushSelDownJoin.
// The pattern of this rule is `Selection -> Join`.
func NewRulePushSelDownJoin() Transformation {
	rule := &PushSelDownJoin{}
	rule.pattern = memo.BuildPattern(
		memo.OperandSelection,
		memo.EngineTiDBOnly,
		memo.NewPattern(memo.OperandJoin, memo.EngineTiDBOnly),
	)
	return rule
}

// buildChildSelectionGroup builds a new childGroup if the pushed down condition is not empty.
func buildChildSelectionGroup(
	oldSel *plannercore.LogicalSelection,
	conditions []expression.Expression,
	childGroup *memo.Group) *memo.Group {
	if len(conditions) == 0 {
		return childGroup
	}
	newSel := plannercore.LogicalSelection{Conditions: conditions}.Init(oldSel.SCtx())
	groupExpr := memo.NewGroupExpr(newSel)
	groupExpr.SetChildren(childGroup)
	newChild := memo.NewGroupWithSchema(groupExpr, childGroup.Prop.Schema)
	return newChild
}

// OnTransform implements Transformation interface.
// This rule tries to pushes the Selection through Join. Besides, this rule fulfills the `XXXConditions` field of Join.
func (r *PushSelDownJoin) OnTransform(old *memo.ExprIter) (newExprs []*memo.GroupExpr, eraseOld bool, eraseAll bool, err error) {
	sel := old.GetExpr().ExprNode.(*plannercore.LogicalSelection)
	joinExpr := old.Children[0].GetExpr()
	// TODO: we need to create a new LogicalJoin here.
	join := joinExpr.ExprNode.(*plannercore.LogicalJoin)
	sctx := sel.SCtx()
	leftGroup := old.Children[0].GetExpr().Children[0]
	rightGroup := old.Children[0].GetExpr().Children[1]
	var equalCond []*expression.ScalarFunction
	var leftPushCond, rightPushCond, otherCond, leftCond, rightCond []expression.Expression
	switch join.JoinType {
	case plannercore.InnerJoin:
		tempCond := make([]expression.Expression, 0,
			len(join.LeftConditions)+len(join.RightConditions)+len(join.EqualConditions)+len(join.OtherConditions)+len(sel.Conditions))
		tempCond = append(tempCond, join.LeftConditions...)
		tempCond = append(tempCond, join.RightConditions...)
		tempCond = append(tempCond, expression.ScalarFuncs2Exprs(join.EqualConditions)...)
		tempCond = append(tempCond, join.OtherConditions...)
		tempCond = append(tempCond, sel.Conditions...)
		tempCond = expression.ExtractFiltersFromDNFs(sctx, tempCond)
		tempCond = expression.PropagateConstant(sctx, tempCond)
		// Return table dual when filter is constant false or null.
		dual := plannercore.Conds2TableDual(join, tempCond)
		if dual != nil {
			return []*memo.GroupExpr{memo.NewGroupExpr(dual)}, false, true, nil
		}
		equalCond, leftPushCond, rightPushCond, otherCond = join.ExtractOnCondition(tempCond, leftGroup.Prop.Schema, rightGroup.Prop.Schema, true, true)
		join.LeftConditions = nil
		join.RightConditions = nil
		join.EqualConditions = equalCond
		join.OtherConditions = otherCond
		leftCond = leftPushCond
		rightCond = rightPushCond
	default:
		// TODO: Enhance this rule to deal with LeftOuter/RightOuter/Semi/SmiAnti/LeftOuterSemi/LeftOuterSemiAnti Joins.
	}
	leftCond = expression.RemoveDupExprs(sctx, leftCond)
	rightCond = expression.RemoveDupExprs(sctx, rightCond)
	for _, eqCond := range join.EqualConditions {
		join.LeftJoinKeys = append(join.LeftJoinKeys, eqCond.GetArgs()[0].(*expression.Column))
		join.RightJoinKeys = append(join.RightJoinKeys, eqCond.GetArgs()[1].(*expression.Column))
	}
	// TODO: Update EqualConditions like what we have done in the method join.updateEQCond() before.
	leftGroup = buildChildSelectionGroup(sel, leftCond, joinExpr.Children[0])
	rightGroup = buildChildSelectionGroup(sel, rightCond, joinExpr.Children[1])
	newJoinExpr := memo.NewGroupExpr(join)
	newJoinExpr.SetChildren(leftGroup, rightGroup)
	return []*memo.GroupExpr{newJoinExpr}, true, false, nil
}

// EliminateProjection eliminates the projection.
type EliminateProjection struct {
	baseRule
}

// NewRuleEliminateProjection creates a new Transformation EliminateProjection.
// The pattern of this rule is `Projection -> Any`.
func NewRuleEliminateProjection() Transformation {
	rule := &EliminateProjection{}
	rule.pattern = memo.BuildPattern(
		memo.OperandProjection,
		memo.EngineTiDBOnly,
		memo.NewPattern(memo.OperandAny, memo.EngineTiDBOnly),
	)
	return rule
}

// OnTransform implements Transformation interface.
// This rule tries to eliminate the projection whose output columns are the same with its child.
func (r *EliminateProjection) OnTransform(old *memo.ExprIter) (newExprs []*memo.GroupExpr, eraseOld bool, eraseAll bool, err error) {
	child := old.Children[0]
	if child.Group.Prop.Schema.Len() != old.GetExpr().Group.Prop.Schema.Len() {
		return nil, false, false, nil
	}

	oldCols := old.GetExpr().Group.Prop.Schema.Columns
	for i, col := range child.Group.Prop.Schema.Columns {
		if !col.Equal(nil, oldCols[i]) {
			return nil, false, false, nil
		}
	}

	// Promote the children group's expression.
	finalGroupExprs := make([]*memo.GroupExpr, 0, child.Group.Equivalents.Len())
	for elem := child.Group.Equivalents.Front(); elem != nil; elem = elem.Next() {
		finalGroupExprs = append(finalGroupExprs, elem.Value.(*memo.GroupExpr))
	}
	return finalGroupExprs, true, false, nil
}

// MergeAdjacentProjection merge the adjacent projection.
type MergeAdjacentProjection struct {
	baseRule
}

// NewRuleMergeAdjacentProjection creates a new Transformation MergeAdjacentProjection.
// The pattern of this rule is `Projection -> Projection`.
func NewRuleMergeAdjacentProjection() Transformation {
	rule := &MergeAdjacentProjection{}
	rule.pattern = memo.BuildPattern(
		memo.OperandProjection,
		memo.EngineTiDBOnly,
		memo.NewPattern(memo.OperandProjection, memo.EngineTiDBOnly),
	)
	return rule
}

// OnTransform implements Transformation interface.
// It will transform `proj->proj->x` to `proj->x`
// or just keep the adjacent projections unchanged.
func (r *MergeAdjacentProjection) OnTransform(old *memo.ExprIter) (newExprs []*memo.GroupExpr, eraseOld bool, eraseAll bool, err error) {
	proj := old.GetExpr().ExprNode.(*plannercore.LogicalProjection)
	childGroup := old.Children[0].Group
	child := old.Children[0].GetExpr().ExprNode.(*plannercore.LogicalProjection)
	if plannercore.ExprsHasSideEffects(child.Exprs) {
		return nil, false, false, nil
	}

	replace := make(map[string]*expression.Column)
	for i, col := range childGroup.Prop.Schema.Columns {
		if colOrigin, ok := child.Exprs[i].(*expression.Column); ok {
			replace[string(col.HashCode(nil))] = colOrigin
		}
	}

	newProj := plannercore.LogicalProjection{Exprs: make([]expression.Expression, len(proj.Exprs))}.Init(proj.SCtx())
	newProj.SetSchema(old.GetExpr().Group.Prop.Schema)
	for i, expr := range proj.Exprs {
		newExpr := expr.Clone()
		plannercore.ResolveExprAndReplace(newExpr, replace)
		newProj.Exprs[i] = plannercore.ReplaceColumnOfExpr(newExpr, child, childGroup.Prop.Schema)
	}

	newProjExpr := memo.NewGroupExpr(newProj)
	newProjExpr.SetChildren(old.Children[0].GetExpr().Children[0])
	return []*memo.GroupExpr{newProjExpr}, true, false, nil
}

// PushTopNDownProjection pushes TopN to Projection.
type PushTopNDownProjection struct {
	baseRule
}

// NewRulePushTopNDownProjection creates a new Transformation PushTopNDownProjection.
// The pattern of this rule is `TopN->Projection->X` to `Projection->TopN->X`.
func NewRulePushTopNDownProjection() Transformation {
	rule := &PushTopNDownProjection{}
	rule.pattern = memo.BuildPattern(
		memo.OperandTopN,
		memo.EngineTiDBOnly,
		memo.NewPattern(memo.OperandProjection, memo.EngineTiDBOnly),
	)
	return rule
}

// Match implements Transformation interface.
func (r *PushTopNDownProjection) Match(expr *memo.ExprIter) bool {
	proj := expr.Children[0].GetExpr().ExprNode.(*plannercore.LogicalProjection)
	for _, expr := range proj.Exprs {
		if expression.HasAssignSetVarFunc(expr) {
			return false
		}
	}
	return true
}

// OnTransform implements Transformation interface.
// This rule tries to pushes the TopN through Projection.
func (r *PushTopNDownProjection) OnTransform(old *memo.ExprIter) (newExprs []*memo.GroupExpr, eraseOld bool, eraseAll bool, err error) {
	topN := old.GetExpr().ExprNode.(*plannercore.LogicalTopN)
	proj := old.Children[0].GetExpr().ExprNode.(*plannercore.LogicalProjection)
	childGroup := old.Children[0].GetExpr().Children[0]

	newTopN := plannercore.LogicalTopN{
		Offset: topN.Offset,
		Count:  topN.Count,
	}.Init(topN.SCtx())

	newTopN.ByItems = make([]*plannercore.ByItems, 0, len(topN.ByItems))
	for _, by := range topN.ByItems {
		newTopN.ByItems = append(newTopN.ByItems, &plannercore.ByItems{
			Expr: expression.ColumnSubstitute(by.Expr, old.Children[0].Group.Prop.Schema, proj.Exprs),
			Desc: by.Desc,
		})
	}

	// remove meaningless constant sort items.
	for i := len(newTopN.ByItems) - 1; i >= 0; i-- {
		switch newTopN.ByItems[i].Expr.(type) {
		case *expression.Constant:
			topN.ByItems = append(newTopN.ByItems[:i], newTopN.ByItems[i+1:]...)
		}
	}
	projExpr := memo.NewGroupExpr(proj)
	topNExpr := memo.NewGroupExpr(newTopN)
	topNExpr.SetChildren(childGroup)
	topNGroup := memo.NewGroupWithSchema(topNExpr, childGroup.Prop.Schema)
	projExpr.SetChildren(topNGroup)
	return []*memo.GroupExpr{projExpr}, true, false, nil
}

// MergeAggregationProjection merges the Projection below an Aggregation as a new Aggregation.
// The Projection may be regenerated in the ImplementationPhase. But this rule allows the
// Aggregation to match other rules, such as MergeAdjacentAggregation.
type MergeAggregationProjection struct {
	baseRule
}

// NewRuleMergeAggregationProjection creates a new Transformation MergeAggregationProjection.
// The pattern of this rule is: `Aggregation -> Projection`.
func NewRuleMergeAggregationProjection() Transformation {
	rule := &MergeAggregationProjection{}
	rule.pattern = memo.BuildPattern(
		memo.OperandAggregation,
		memo.EngineTiDBOnly,
		memo.NewPattern(memo.OperandProjection, memo.EngineTiDBOnly),
	)
	return rule
}

// Match implements Transformation interface.
func (r *MergeAggregationProjection) Match(old *memo.ExprIter) bool {
	proj := old.Children[0].GetExpr().ExprNode.(*plannercore.LogicalProjection)
	if plannercore.ExprsHasSideEffects(proj.Exprs) {
		return false
	}
	return true
}

// OnTransform implements Transformation interface.
// It will transform `Aggregation->Projection->X` to `Aggregation->X`.
/*
主要是实现从agg->projection->x到agg->x的变换。也就是把映射合入agg中，只保留需要的列。
我们只需要将agg中的groupby的列与aggregation function的参数进行映射变换即可，然后消除projection这一层group。
*/
func (r *MergeAggregationProjection) OnTransform(old *memo.ExprIter) (newExprs []*memo.GroupExpr, eraseOld bool, eraseAll bool, err error) {
	// TODO: implement the body according to the header comment.
	oldAgg := old.GetExpr().ExprNode.(*plannercore.LogicalAggregation)
	proj := old.Children[0].GetExpr().ExprNode.(*plannercore.LogicalProjection)
	projSchema := old.Children[0].GetExpr().Schema()

	groupByItems := make([]expression.Expression, len(oldAgg.GroupByItems))
	for i, item := range oldAgg.GroupByItems {
		groupByItems[i] = expression.ColumnSubstitute(item, projSchema, proj.Exprs)
	}

	aggFuncs := make([]*aggregation.AggFuncDesc, len(oldAgg.AggFuncs))
	for i, aggFunc := range oldAgg.AggFuncs {
		aggFuncs[i] = aggFunc.Clone()
		newArgs := make([]expression.Expression, len(aggFunc.Args))
		for j, arg := range aggFunc.Args {
			newArgs[j] = expression.ColumnSubstitute(arg, projSchema, proj.Exprs)
		}
		aggFuncs[i].Args = newArgs
	}

	newAgg := plannercore.LogicalAggregation{
		GroupByItems: groupByItems,
		AggFuncs:     aggFuncs,
	}.Init(oldAgg.SCtx())

	newAggExpr := memo.NewGroupExpr(newAgg)
	newAggExpr.SetChildren(old.Children[0].GetExpr().Children...)

	return []*memo.GroupExpr{newAggExpr}, true, false, nil
}
