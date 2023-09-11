// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
// // Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package core

import (
	"context"

	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/parser/ast"
	"github.com/pingcap/tidb/parser/mysql"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/types"
)

type ppdSolver struct{}

func (s *ppdSolver) optimize(ctx context.Context, lp LogicalPlan) (LogicalPlan, error) {
	_, p := lp.PredicatePushDown(nil)
	return p, nil
}

func addSelection(p LogicalPlan, child LogicalPlan, conditions []expression.Expression, chIdx int) {
	if len(conditions) == 0 {
		p.Children()[chIdx] = child
		return
	}
	conditions = expression.PropagateConstant(p.SCtx(), conditions)
	// Return table dual when filter is constant false or null.
	dual := Conds2TableDual(child, conditions)
	if dual != nil {
		p.Children()[chIdx] = dual
		return
	}
	selection := LogicalSelection{Conditions: conditions}.Init(p.SCtx())
	selection.SetChildren(child)
	p.Children()[chIdx] = selection
}

// PredicatePushDown implements LogicalPlan interface.
func (p *baseLogicalPlan) PredicatePushDown(predicates []expression.Expression) ([]expression.Expression, LogicalPlan) {
	if len(p.children) == 0 {
		return predicates, p.self
	}
	child := p.children[0]
	rest, newChild := child.PredicatePushDown(predicates)
	addSelection(p.self, newChild, rest, 0)
	return nil, p.self
}

func splitSetGetVarFunc(filters []expression.Expression) ([]expression.Expression, []expression.Expression) {
	canBePushDown := make([]expression.Expression, 0, len(filters))
	canNotBePushDown := make([]expression.Expression, 0, len(filters))
	for _, expr := range filters {
		if expression.HasGetSetVarFunc(expr) {
			canNotBePushDown = append(canNotBePushDown, expr)
		} else {
			canBePushDown = append(canBePushDown, expr)
		}
	}
	return canBePushDown, canNotBePushDown
}

// PredicatePushDown implements LogicalPlan PredicatePushDown interface.
func (p *LogicalSelection) PredicatePushDown(predicates []expression.Expression) ([]expression.Expression, LogicalPlan) {
	canBePushDown, canNotBePushDown := splitSetGetVarFunc(p.Conditions)
	retConditions, child := p.children[0].PredicatePushDown(append(canBePushDown, predicates...))
	retConditions = append(retConditions, canNotBePushDown...)
	if len(retConditions) > 0 {
		p.Conditions = expression.PropagateConstant(p.ctx, retConditions)
		// Return table dual when filter is constant false or null.
		dual := Conds2TableDual(p, p.Conditions)
		if dual != nil {
			return nil, dual
		}
		return nil, p
	}
	return nil, child
}

// PredicatePushDown implements LogicalPlan PredicatePushDown interface.
func (p *LogicalUnionScan) PredicatePushDown(predicates []expression.Expression) ([]expression.Expression, LogicalPlan) {
	retainedPredicates, _ := p.children[0].PredicatePushDown(predicates)
	p.conditions = make([]expression.Expression, 0, len(predicates))
	p.conditions = append(p.conditions, predicates...)
	// The conditions in UnionScan is only used for added rows, so parent Selection should not be removed.
	return retainedPredicates, p
}

// PredicatePushDown implements LogicalPlan PredicatePushDown interface.
func (ds *DataSource) PredicatePushDown(predicates []expression.Expression) ([]expression.Expression, LogicalPlan) {
	ds.allConds = predicates
	_, ds.pushedDownConds, predicates = expression.ExpressionsToPB(ds.ctx.GetSessionVars().StmtCtx, predicates, ds.ctx.GetClient())
	return predicates, ds
}

// PredicatePushDown implements LogicalPlan PredicatePushDown interface.
func (p *LogicalTableDual) PredicatePushDown(predicates []expression.Expression) ([]expression.Expression, LogicalPlan) {
	return predicates, p
}

// PredicatePushDown implements LogicalPlan PredicatePushDown interface.
func (p *LogicalJoin) PredicatePushDown(predicates []expression.Expression) (ret []expression.Expression, retPlan LogicalPlan) {
	simplifyOuterJoin(p, predicates)
	var equalCond []*expression.ScalarFunction
	var leftPushCond, rightPushCond, otherCond, leftCond, rightCond []expression.Expression
	switch p.JoinType {
	case LeftOuterJoin:
		predicates = p.outerJoinPropConst(predicates)
		dual := Conds2TableDual(p, predicates)
		if dual != nil {
			return ret, dual
		}
		// Handle where conditions
		predicates = expression.ExtractFiltersFromDNFs(p.ctx, predicates)
		// Only derive left where condition, because right where condition cannot be pushed down
		equalCond, leftPushCond, rightPushCond, otherCond = p.extractOnCondition(predicates, true, false)
		leftCond = leftPushCond
		// Handle join conditions, only derive right join condition, because left join condition cannot be pushed down
		_, derivedRightJoinCond := deriveOtherConditions(p, false, true)
		rightCond = append(p.RightConditions, derivedRightJoinCond...)
		p.RightConditions = nil
		ret = append(expression.ScalarFuncs2Exprs(equalCond), otherCond...)
		ret = append(ret, rightPushCond...)
	case RightOuterJoin:
		predicates = p.outerJoinPropConst(predicates)
		dual := Conds2TableDual(p, predicates)
		if dual != nil {
			return ret, dual
		}
		// Handle where conditions
		predicates = expression.ExtractFiltersFromDNFs(p.ctx, predicates)
		// Only derive right where condition, because left where condition cannot be pushed down
		equalCond, leftPushCond, rightPushCond, otherCond = p.extractOnCondition(predicates, false, true)
		rightCond = rightPushCond
		// Handle join conditions, only derive left join condition, because right join condition cannot be pushed down
		derivedLeftJoinCond, _ := deriveOtherConditions(p, true, false)
		leftCond = append(p.LeftConditions, derivedLeftJoinCond...)
		p.LeftConditions = nil
		ret = append(expression.ScalarFuncs2Exprs(equalCond), otherCond...)
		ret = append(ret, leftPushCond...)
	case InnerJoin:
		tempCond := make([]expression.Expression, 0, len(p.LeftConditions)+len(p.RightConditions)+len(p.EqualConditions)+len(p.OtherConditions)+len(predicates))
		tempCond = append(tempCond, p.LeftConditions...)
		tempCond = append(tempCond, p.RightConditions...)
		tempCond = append(tempCond, expression.ScalarFuncs2Exprs(p.EqualConditions)...)
		tempCond = append(tempCond, p.OtherConditions...)
		tempCond = append(tempCond, predicates...)
		tempCond = expression.ExtractFiltersFromDNFs(p.ctx, tempCond)
		tempCond = expression.PropagateConstant(p.ctx, tempCond)
		// Return table dual when filter is constant false or null.
		dual := Conds2TableDual(p, tempCond)
		if dual != nil {
			return ret, dual
		}
		equalCond, leftPushCond, rightPushCond, otherCond = p.extractOnCondition(tempCond, true, true)
		p.LeftConditions = nil
		p.RightConditions = nil
		p.EqualConditions = equalCond
		p.OtherConditions = otherCond
		leftCond = leftPushCond
		rightCond = rightPushCond
	}
	leftCond = expression.RemoveDupExprs(p.ctx, leftCond)
	rightCond = expression.RemoveDupExprs(p.ctx, rightCond)
	leftRet, lCh := p.children[0].PredicatePushDown(leftCond)
	rightRet, rCh := p.children[1].PredicatePushDown(rightCond)
	addSelection(p, lCh, leftRet, 0)
	addSelection(p, rCh, rightRet, 1)
	p.updateEQCond()
	for _, eqCond := range p.EqualConditions {
		p.LeftJoinKeys = append(p.LeftJoinKeys, eqCond.GetArgs()[0].(*expression.Column))
		p.RightJoinKeys = append(p.RightJoinKeys, eqCond.GetArgs()[1].(*expression.Column))
	}
	p.mergeSchema()
	buildKeyInfo(p)
	return ret, p.self
}

// updateEQCond will extract the arguments of a equal condition that connect two expressions.
func (p *LogicalJoin) updateEQCond() {
	lChild, rChild := p.children[0], p.children[1]
	var lKeys, rKeys []expression.Expression
	for i := len(p.OtherConditions) - 1; i >= 0; i-- {
		need2Remove := false
		if eqCond, ok := p.OtherConditions[i].(*expression.ScalarFunction); ok && eqCond.FuncName.L == ast.EQ {
			lExpr, rExpr := eqCond.GetArgs()[0], eqCond.GetArgs()[1]
			if expression.ExprFromSchema(lExpr, lChild.Schema()) && expression.ExprFromSchema(rExpr, rChild.Schema()) {
				lKeys = append(lKeys, lExpr)
				rKeys = append(rKeys, rExpr)
				need2Remove = true
			} else if expression.ExprFromSchema(lExpr, rChild.Schema()) && expression.ExprFromSchema(rExpr, lChild.Schema()) {
				lKeys = append(lKeys, rExpr)
				rKeys = append(rKeys, lExpr)
				need2Remove = true
			}
		}
		if need2Remove {
			p.OtherConditions = append(p.OtherConditions[:i], p.OtherConditions[i+1:]...)
		}
	}
	if len(lKeys) > 0 {
		needLProj, needRProj := false, false
		for i := range lKeys {
			_, lOk := lKeys[i].(*expression.Column)
			_, rOk := rKeys[i].(*expression.Column)
			needLProj = needLProj || !lOk
			needRProj = needRProj || !rOk
		}

		var lProj, rProj *LogicalProjection
		if needLProj {
			lProj = p.getProj(0)
		}
		if needRProj {
			rProj = p.getProj(1)
		}
		for i := range lKeys {
			lKey, rKey := lKeys[i], rKeys[i]
			if lProj != nil {
				lKey = lProj.appendExpr(lKey)
			}
			if rProj != nil {
				rKey = rProj.appendExpr(rKey)
			}
			eqCond := expression.NewFunctionInternal(p.ctx, ast.EQ, types.NewFieldType(mysql.TypeTiny), lKey, rKey)
			p.EqualConditions = append(p.EqualConditions, eqCond.(*expression.ScalarFunction))
		}
	}
}

func (p *LogicalProjection) appendExpr(expr expression.Expression) *expression.Column {
	if col, ok := expr.(*expression.Column); ok {
		return col
	}
	expr = expression.ColumnSubstitute(expr, p.schema, p.Exprs)
	p.Exprs = append(p.Exprs, expr)

	col := &expression.Column{
		UniqueID: p.ctx.GetSessionVars().AllocPlanColumnID(),
		RetType:  expr.GetType(),
	}
	p.schema.Append(col)
	return col
}

func (p *LogicalJoin) getProj(idx int) *LogicalProjection {
	child := p.children[idx]
	proj, ok := child.(*LogicalProjection)
	if ok {
		return proj
	}
	proj = LogicalProjection{Exprs: make([]expression.Expression, 0, child.Schema().Len())}.Init(p.ctx)
	for _, col := range child.Schema().Columns {
		proj.Exprs = append(proj.Exprs, col)
	}
	proj.SetSchema(child.Schema().Clone())
	proj.SetChildren(child)
	p.children[idx] = proj
	return proj
}

// simplifyOuterJoin transforms "LeftOuterJoin/RightOuterJoin" to "InnerJoin" if possible.
func simplifyOuterJoin(p *LogicalJoin, predicates []expression.Expression) {
	if p.JoinType != LeftOuterJoin && p.JoinType != RightOuterJoin && p.JoinType != InnerJoin {
		return
	}

	innerTable := p.children[0]
	outerTable := p.children[1]
	if p.JoinType == LeftOuterJoin {
		innerTable, outerTable = outerTable, innerTable
	}

	// first simplify embedded outer join.
	if innerPlan, ok := innerTable.(*LogicalJoin); ok {
		simplifyOuterJoin(innerPlan, predicates)
	}
	if outerPlan, ok := outerTable.(*LogicalJoin); ok {
		simplifyOuterJoin(outerPlan, predicates)
	}

	if p.JoinType == InnerJoin {
		return
	}
	// then simplify embedding outer join.
	canBeSimplified := false
	for _, expr := range predicates {
		isOk := isNullRejected(p.ctx, innerTable.Schema(), expr)
		if isOk {
			canBeSimplified = true
			break
		}
	}
	if canBeSimplified {
		p.JoinType = InnerJoin
	}
}

// isNullRejected check whether a condition is null-rejected
// A condition would be null-rejected in one of following cases:
// If it is a predicate containing a reference to an inner table that evaluates to UNKNOWN or FALSE when one of its arguments is NULL.
// If it is a conjunction containing a null-rejected condition as a conjunct.
// If it is a disjunction of null-rejected conditions.
func isNullRejected(ctx sessionctx.Context, schema *expression.Schema, expr expression.Expression) bool {
	expr = expression.PushDownNot(nil, expr)
	sc := ctx.GetSessionVars().StmtCtx
	sc.InNullRejectCheck = true
	result := expression.EvaluateExprWithNull(ctx, schema, expr)
	sc.InNullRejectCheck = false
	x, ok := result.(*expression.Constant)
	if !ok {
		return false
	}
	if x.Value.IsNull() {
		return true
	} else if isTrue, err := x.Value.ToBool(sc); err == nil && isTrue == 0 {
		return true
	}
	return false
}

// PredicatePushDown implements LogicalPlan PredicatePushDown interface.
func (p *LogicalProjection) PredicatePushDown(predicates []expression.Expression) (ret []expression.Expression, retPlan LogicalPlan) {
	canBePushed := make([]expression.Expression, 0, len(predicates))    // 可以被下推的谓词
	canNotBePushed := make([]expression.Expression, 0, len(predicates)) // 不可以被下推的谓词
	for _, expr := range p.Exprs {                                      // 遍历表达式
		if expression.HasAssignSetVarFunc(expr) {
			_, child := p.baseLogicalPlan.PredicatePushDown(nil)
			return predicates, child
		}
	}
	for _, cond := range predicates {
		newFilter := expression.ColumnSubstitute(cond, p.Schema(), p.Exprs)
		if !expression.HasGetSetVarFunc(newFilter) {
			canBePushed = append(canBePushed, expression.ColumnSubstitute(cond, p.Schema(), p.Exprs))
		} else {
			canNotBePushed = append(canNotBePushed, cond)
		}
	}
	remained, child := p.baseLogicalPlan.PredicatePushDown(canBePushed)
	return append(remained, canNotBePushed...), child
}

// pushDownPredicatesForAggregation split a condition to two parts, can be pushed-down or can not be pushed-down below aggregation.
func (la *LogicalAggregation) pushDownPredicatesForAggregation(cond expression.Expression,
	groupByColumns *expression.Schema, exprsOriginal []expression.Expression) ([]expression.Expression, []expression.Expression) {
	var condsToPush []expression.Expression
	var ret []expression.Expression
	switch cond.(type) {
	case *expression.Constant:
		condsToPush = append(condsToPush, cond)
		// Consider SQL list "select sum(b) from t group by a having 1=0". "1=0" is a constant predicate which should be
		// retained and pushed down at the same time. Because we will get a wrong query result that contains one column
		// with value 0 rather than an empty query result.
		ret = append(ret, cond)
	case *expression.ScalarFunction:
		extractedCols := expression.ExtractColumns(cond)
		ok := true
		for _, col := range extractedCols {
			if !groupByColumns.Contains(col) {
				ok = false
				break
			}
		}

		if ok {
			newFunc := expression.ColumnSubstitute(cond, la.Schema(), exprsOriginal)
			condsToPush = append(condsToPush, newFunc)
		} else {
			ret = append(ret, cond)
		}
	}

	return condsToPush, ret
}

// pushDownPredicatesForAggregation split a CNF condition to two parts, can be pushed-down or can not be pushed-down below aggregation.
// It would consider the CNF.
// For example,
// (a > 1 or avg(b) > 1) and (a < 3), and `avg(b) > 1` can't be pushed-down.
// Then condsToPush: a < 3, ret: a > 1 or avg(b) > 1
func (la *LogicalAggregation) pushDownCNFPredicatesForAggregation(cond expression.Expression,
	groupByColumns *expression.Schema, exprsOriginal []expression.Expression) ([]expression.Expression, []expression.Expression) {
	var condsToPush []expression.Expression
	var ret []expression.Expression
	subCNFItem := expression.SplitCNFItems(cond) // 合取范式拆分
	if len(subCNFItem) == 1 {
		return la.pushDownPredicatesForAggregation(subCNFItem[0], groupByColumns, exprsOriginal)
	}

	for _, item := range subCNFItem {
		condsToPushForItem, retForItem := la.pushDownDNFPredicatesForAggregation(item, groupByColumns, exprsOriginal)
		if len(condsToPushForItem) > 0 {
			condsToPush = append(condsToPush, expression.ComposeDNFCondition(la.SCtx(), condsToPushForItem...))
		}
		if len(retForItem) > 0 {
			ret = append(ret, expression.ComposeDNFCondition(la.SCtx(), retForItem...))
		}
	}

	return condsToPush, ret
}

// pushDownDNFPredicatesForAggregation split a DNF condition to two parts, can be pushed-down or can not be pushed-down below aggregation.
// It would consider the DNF.
// For example,
// (a > 1 and avg(b) > 1) or (a < 3), and `avg(b) > 1` can't be pushed-down.
// Then condsToPush: (a < 3) and (a > 1), ret: (a > 1 and avg(b) > 1) or (a < 3)
func (la *LogicalAggregation) pushDownDNFPredicatesForAggregation(cond expression.Expression,
	groupByColumns *expression.Schema, exprsOriginal []expression.Expression) ([]expression.Expression, []expression.Expression) {
	var condsToPush []expression.Expression
	var ret []expression.Expression
	subDNFItem := expression.SplitDNFItems(cond) // 析取范式拆分
	if len(subDNFItem) == 1 {
		return la.pushDownPredicatesForAggregation(subDNFItem[0], groupByColumns, exprsOriginal)
	}
	for _, item := range subDNFItem {
		condsToPushForItem, retForItem := la.pushDownCNFPredicatesForAggregation(item, groupByColumns, exprsOriginal)
		if len(condsToPushForItem) <= 0 {
			return nil, []expression.Expression{cond}
		}
		condsToPush = append(condsToPush, expression.ComposeCNFCondition(la.SCtx(), condsToPushForItem...))
		if len(retForItem) > 0 {
			ret = append(ret, expression.ComposeCNFCondition(la.SCtx(), retForItem...))
		}
	}

	if len(ret) == 0 {
		return []expression.Expression{cond}, nil
	}

	dnfPushDownCond := expression.ComposeDNFCondition(la.SCtx(), condsToPush...)
	return []expression.Expression{dnfPushDownCond}, []expression.Expression{cond}
}

// PredicatePushDown implements LogicalPlan PredicatePushDown interface.
// TODO: project 4-1 your code here.
// Here you need to push the predicates across the aggregation.
// A simple example is that `select * from (select count(*) from t group by b) tmp_t where b > 1` is the same with
// `select * from (select count(*) from t where b > 1 group by b) tmp_t.
// parameters:
//   predicates: an expression slice which needs to be pushed down as deeply as possible.
// return values:
//   ret:     the expressions that can't be pushed.
//   retPlan: a plan that represents a new root, because it might change the root if the having clause exists
// In this function, you need to iterate through list `predicates`, and consider whether each function in it can be pushed down
// below the current aggregation.
// Hints:
//   1. predicates need to be discussed in two types: expression.Constant and expression.ScalarFunction
// PredicatePushDown实现LogicalPlan PredicatePushDown接口。
// TODO:项目4-1你的代码在这里。
// 这里需要跨聚合推送谓词。
// 一个简单的例子是，' select * from (select count(*) from t group by b) tmp_t where b > 1 '与
// ' select * from (select count(*) from t where b > 1 group by b) tmp_t。
// 参数:
// 谓词:一个表达式切片，需要尽可能地向下推。
// 返回值:
// ret:不能被压入的表达式。
// retPlan:一个表示新根的计划，因为如果having子句存在，它可能会改变根
// 在这个函数中，你需要遍历列表'谓词'，并考虑其中的每个函数是否可以下推到低于当前聚合中。
// 提示:
// 1、需要以两种类型讨论谓词:expression.Constant和expression.ScalarFunction
func (la *LogicalAggregation) PredicatePushDown(predicates []expression.Expression) (ret []expression.Expression, retPlan LogicalPlan) {
	var condsToPush []expression.Expression // 下推的条件
	// 1.将聚集函数的参数append到exprsOriginal，并获取groupby的列
	exprsOriginal := make([]expression.Expression, 0, len(la.AggFuncs)) // 存放每一个聚集函数的参数表达式
	for _, fun := range la.AggFuncs {
		exprsOriginal = append(exprsOriginal, fun.Args[0])
	}
	groupByColumns := expression.NewSchema(la.GetGroupByCols()...) // 获取groupby的列

	// 2.对谓词进行遍历
	/*
		对于常数，比如1=0，这种应该在保留的同时下推；
		对于ScalarFunction，它是一个返回一个值的函数，这种需要从当前谓词中提取出涉及的columns，遍历columns，如果函数中的列是groupby中需要的，就下推计算，否则保留
	*/
	for _, cond := range predicates {
		/*subConsToPush, subRet := la.pushDownDNFPredicatesForAggregation(cond, groupByColumns, exprsOriginal)
		if len(subConsToPush) > 0 {
			condsToPush = append(condsToPush, subConsToPush...)
		}
		if len(subRet) > 0 {
			ret = append(ret, subRet...)
		}*/
		switch cond.(type) {
		case *expression.Constant:
			// 对于常数，比如1=0，这种应该既保留也下推
			condsToPush = append(condsToPush, cond)
			// Consider SQL list "select sum(b) from t group by a having 1=0". "1=0" is a constant predicate which should be
			// retained and pushed down at the same time. Because we will get a wrong query result that contains one column
			// with value 0 rather than an empty query result.
			ret = append(ret, cond)
		case *expression.ScalarFunction:
			// 对于ScalarFunction(标量函数LEN/UPPER/LOWER/LEFT/RIGHT/SUBSTRING/CONVERT/GETDATE)，
			// 它是一个返回一个值的函数，这种需要从当前谓词中提取出涉及的columns，
			// 遍历columns，如果函数中的列是groupby中需要的，就下推计算，否则保留
			extractedCols := expression.ExtractColumns(cond) // 提取cond中的所有列
			ok := true
			for _, col := range extractedCols {
				if !groupByColumns.Contains(col) { // 查看当前cond中包含的列是否存在于groupByColumns中
					ok = false
					break
				}
			}

			// 如果存在的话，需要将列进行替换
			if ok {
				// 在聚合过程中使用聚合函数的列来替换cond的列，生成新的谓词条件，以确保谓词中的列与实际进行聚合的列保持一致
				newFunc := expression.ColumnSubstitute(cond, la.Schema(), exprsOriginal)
				condsToPush = append(condsToPush, newFunc)
			} else {
				ret = append(ret, cond)
			}
		}
	}

	// 3.调用baseLogicalPlan进行谓词下推
	la.baseLogicalPlan.PredicatePushDown(condsToPush)
	//if condsToPush != nil {
	//
	//}

	return ret, la
}

// PredicatePushDown implements LogicalPlan PredicatePushDown interface.
func (p *LogicalLimit) PredicatePushDown(predicates []expression.Expression) ([]expression.Expression, LogicalPlan) {
	// Limit forbids any condition to push down.
	p.baseLogicalPlan.PredicatePushDown(nil)
	return predicates, p
}

// deriveOtherConditions given a LogicalJoin, check the OtherConditions to see if we can derive more
// conditions for left/right child pushdown.
func deriveOtherConditions(p *LogicalJoin, deriveLeft bool, deriveRight bool) (leftCond []expression.Expression,
	rightCond []expression.Expression) {
	leftPlan, rightPlan := p.children[0], p.children[1]
	for _, expr := range p.OtherConditions {
		if deriveLeft {
			leftRelaxedCond := expression.DeriveRelaxedFiltersFromDNF(expr, leftPlan.Schema())
			if leftRelaxedCond != nil {
				leftCond = append(leftCond, leftRelaxedCond)
			}
			notNullExpr := deriveNotNullExpr(expr, leftPlan.Schema())
			if notNullExpr != nil {
				leftCond = append(leftCond, notNullExpr)
			}
		}
		if deriveRight {
			rightRelaxedCond := expression.DeriveRelaxedFiltersFromDNF(expr, rightPlan.Schema())
			if rightRelaxedCond != nil {
				rightCond = append(rightCond, rightRelaxedCond)
			}
			notNullExpr := deriveNotNullExpr(expr, rightPlan.Schema())
			if notNullExpr != nil {
				rightCond = append(rightCond, notNullExpr)
			}
		}
	}
	return
}

// deriveNotNullExpr generates a new expression `not(isnull(col))` given `col1 op col2`,
// in which `col` is in specified schema. Caller guarantees that only one of `col1` or
// `col2` is in schema.
func deriveNotNullExpr(expr expression.Expression, schema *expression.Schema) expression.Expression {
	binop, ok := expr.(*expression.ScalarFunction)
	if !ok || len(binop.GetArgs()) != 2 {
		return nil
	}
	ctx := binop.GetCtx()
	arg0, lOK := binop.GetArgs()[0].(*expression.Column)
	arg1, rOK := binop.GetArgs()[1].(*expression.Column)
	if !lOK || !rOK {
		return nil
	}
	childCol := schema.RetrieveColumn(arg0)
	if childCol == nil {
		childCol = schema.RetrieveColumn(arg1)
	}
	if isNullRejected(ctx, schema, expr) && !mysql.HasNotNullFlag(childCol.RetType.Flag) {
		return expression.BuildNotNullExpr(ctx, childCol)
	}
	return nil
}

// Conds2TableDual builds a LogicalTableDual if cond is constant false or null.
func Conds2TableDual(p LogicalPlan, conds []expression.Expression) LogicalPlan {
	if len(conds) != 1 {
		return nil
	}
	con, ok := conds[0].(*expression.Constant)
	if !ok {
		return nil
	}
	sc := p.SCtx().GetSessionVars().StmtCtx
	if isTrue, err := con.Value.ToBool(sc); (err == nil && isTrue == 0) || con.Value.IsNull() {
		dual := LogicalTableDual{}.Init(p.SCtx())
		dual.SetSchema(p.Schema())
		return dual
	}
	return nil
}

// outerJoinPropConst propagates constant equal and column equal conditions over outer join.
func (p *LogicalJoin) outerJoinPropConst(predicates []expression.Expression) []expression.Expression {
	outerTable := p.children[0]
	innerTable := p.children[1]
	if p.JoinType == RightOuterJoin {
		innerTable, outerTable = outerTable, innerTable
	}
	lenJoinConds := len(p.EqualConditions) + len(p.LeftConditions) + len(p.RightConditions) + len(p.OtherConditions)
	joinConds := make([]expression.Expression, 0, lenJoinConds)
	for _, equalCond := range p.EqualConditions {
		joinConds = append(joinConds, equalCond)
	}
	joinConds = append(joinConds, p.LeftConditions...)
	joinConds = append(joinConds, p.RightConditions...)
	joinConds = append(joinConds, p.OtherConditions...)
	p.EqualConditions = nil
	p.LeftConditions = nil
	p.RightConditions = nil
	p.OtherConditions = nil
	joinConds, predicates = expression.PropConstOverOuterJoin(p.ctx, joinConds, predicates, outerTable.Schema(), innerTable.Schema())
	p.attachOnConds(joinConds)
	return predicates
}

// PredicatePushDown implements LogicalPlan PredicatePushDown interface.
func (p *LogicalMemTable) PredicatePushDown(predicates []expression.Expression) ([]expression.Expression, LogicalPlan) {
	return predicates, p.self
}

func (*ppdSolver) name() string {
	return "predicate_push_down"
}
