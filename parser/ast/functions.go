// Copyright 2015 PingCAP, Inc.
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

package ast

import (
	"fmt"
	"io"

	"github.com/pingcap/tidb/parser/model"
)

var (
	_ FuncNode = &AggregateFuncExpr{}
	_ FuncNode = &FuncCallExpr{}
)

// List scalar function names.
const (
	IsNull      = "isnull"       // 是否为空值
	Length      = "length"       // 获取字符串的长度
	Strcmp      = "strcmp"       // 比较两个字符串的长度
	OctetLength = "octet_length" // 获取字节长度
	If          = "if"           // 条件语句
	Ifnull      = "ifnull"       // 是否为空，如果为空则返回指定的值
	LogicAnd    = "and"          // 逻辑与
	LogicOr     = "or"           // 逻辑或
	GE          = "ge"           // 大于等于比较
	LE          = "le"           // 小于等于比较
	EQ          = "eq"           // 等于比较
	NE          = "ne"           // 不等于比较
	LT          = "lt"           // 小于比较
	GT          = "gt"           // 大于比较
	Plus        = "plus"         // 加法
	Minus       = "minus"        // 减法
	Div         = "div"          // 除法
	Mul         = "mul"          // 乘法
	UnaryNot    = "not"          // 逻辑非操作
	UnaryMinus  = "unaryminus"   // 一元负号
	In          = "in"           // 判断某个值是否在一组值中
	RowFunc     = "row"          // 行函数
	SetVar      = "setvar"       // 设置变量的值
	GetVar      = "getvar"       // 获取变量的值
	Values      = "values"       // 一组值
)

// FuncCallExpr is for function expression.
type FuncCallExpr struct {
	funcNode
	// FnName is the function name.
	FnName model.CIStr
	// Args is the function args.
	Args []ExprNode
}

// Format the ExprNode into a Writer.
func (n *FuncCallExpr) Format(w io.Writer) {
	fmt.Fprintf(w, "%s(", n.FnName.L)
	for i, arg := range n.Args {
		arg.Format(w)
		if i != len(n.Args)-1 {
			fmt.Fprint(w, ", ")
		}
	}
	fmt.Fprint(w, ")")
}

// Accept implements Node interface.
func (n *FuncCallExpr) Accept(v Visitor) (Node, bool) {
	newNode, skipChildren := v.Enter(n)
	if skipChildren {
		return v.Leave(newNode)
	}
	n = newNode.(*FuncCallExpr)
	for i, val := range n.Args {
		node, ok := val.Accept(v)
		if !ok {
			return n, false
		}
		n.Args[i] = node.(ExprNode)
	}
	return v.Leave(n)
}

const (
	// AggFuncCount is the name of Count function.
	AggFuncCount = "count"
	// AggFuncSum is the name of Sum function.
	AggFuncSum = "sum"
	// AggFuncAvg is the name of Avg function.
	AggFuncAvg = "avg"
	// AggFuncFirstRow is the name of FirstRowColumn function.
	AggFuncFirstRow = "firstrow"
	// AggFuncMax is the name of max function.
	AggFuncMax = "max"
	// AggFuncMin is the name of min function.
	AggFuncMin = "min"
)

// AggregateFuncExpr represents aggregate function expression.
type AggregateFuncExpr struct {
	funcNode
	// F is the function name.
	F string
	// Args is the function args.
	Args []ExprNode
}

// Format the ExprNode into a Writer.
func (n *AggregateFuncExpr) Format(w io.Writer) {
	panic("Not implemented")
}

// Accept implements Node Accept interface.
func (n *AggregateFuncExpr) Accept(v Visitor) (Node, bool) {
	newNode, skipChildren := v.Enter(n)
	if skipChildren {
		return v.Leave(newNode)
	}
	n = newNode.(*AggregateFuncExpr)
	for i, val := range n.Args {
		node, ok := val.Accept(v)
		if !ok {
			return n, false
		}
		n.Args[i] = node.(ExprNode)
	}
	return v.Leave(n)
}
