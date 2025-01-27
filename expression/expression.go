// Copyright 2016 PingCAP, Inc.
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

package expression

import (
	goJSON "encoding/json"
	"fmt"
	"sync"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/parser/ast"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/parser/mysql"
	"github.com/pingcap/tidb/parser/terror"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
)

// These are byte flags used for `HashCode()`.
const (
	constantFlag       byte = 0
	columnFlag         byte = 1
	scalarFunctionFlag byte = 3
)

// EvalAstExpr evaluates ast expression directly.
var EvalAstExpr func(sctx sessionctx.Context, expr ast.ExprNode) (types.Datum, error)

// VecExpr contains all vectorized evaluation methods.
type VecExpr interface {
	// Vectorized returns if this expression supports vectorized evaluation.
	Vectorized() bool

	// VecEvalInt evaluates this expression in a vectorized manner.
	VecEvalInt(ctx sessionctx.Context, input *chunk.Chunk, result *chunk.Column) error

	// VecEvalReal evaluates this expression in a vectorized manner.
	VecEvalReal(ctx sessionctx.Context, input *chunk.Chunk, result *chunk.Column) error

	// VecEvalString evaluates this expression in a vectorized manner.
	VecEvalString(ctx sessionctx.Context, input *chunk.Chunk, result *chunk.Column) error
}

// Expression represents all scalar expression in SQL.
type Expression interface {
	fmt.Stringer
	goJSON.Marshaler
	VecExpr

	// Eval evaluates an expression through a row.
	Eval(row chunk.Row) (types.Datum, error)

	// EvalInt returns the int64 representation of expression.
	EvalInt(ctx sessionctx.Context, row chunk.Row) (val int64, isNull bool, err error)

	// EvalReal returns the float64 representation of expression.
	EvalReal(ctx sessionctx.Context, row chunk.Row) (val float64, isNull bool, err error)

	// EvalString returns the string representation of expression.
	EvalString(ctx sessionctx.Context, row chunk.Row) (val string, isNull bool, err error)

	// GetType gets the type that the expression returns.
	GetType() *types.FieldType

	// Clone copies an expression totally.
	Clone() Expression

	// Equal checks whether two expressions are equal.
	Equal(ctx sessionctx.Context, e Expression) bool

	// IsCorrelated checks if this expression has correlated key.
	IsCorrelated() bool

	// ConstItem checks if this expression is constant item, regardless of query evaluation state.
	// A constant item can be eval() when build a plan.
	// An expression is constant item if it:
	// refers no tables.
	// refers no subqueries that refers any tables.
	// refers no non-deterministic functions.
	// refers no statement parameters.
	ConstItem() bool

	// Decorrelate try to decorrelate the expression by schema.
	Decorrelate(schema *Schema) Expression

	// ResolveIndices resolves indices by the given schema. It will copy the original expression and return the copied one.
	ResolveIndices(schema *Schema) (Expression, error)

	// resolveIndices is called inside the `ResolveIndices` It will perform on the expression itself.
	resolveIndices(schema *Schema) error

	// ExplainInfo returns operator information to be explained.
	ExplainInfo() string

	// HashCode creates the hashcode for expression which can be used to identify itself from other expression.
	// It generated as the following:
	// Constant: ConstantFlag+encoded value
	// Column: ColumnFlag+encoded value
	// ScalarFunction: SFFlag+encoded function name + encoded arg_1 + encoded arg_2 + ...
	HashCode(sc *stmtctx.StatementContext) []byte
}

// CNFExprs stands for a CNF expression.
type CNFExprs []Expression

// Clone clones itself.
func (e CNFExprs) Clone() CNFExprs {
	cnf := make(CNFExprs, 0, len(e))
	for _, expr := range e {
		cnf = append(cnf, expr.Clone())
	}
	return cnf
}

// Shallow makes a shallow copy of itself.
func (e CNFExprs) Shallow() CNFExprs {
	cnf := make(CNFExprs, 0, len(e))
	cnf = append(cnf, e...)
	return cnf
}

// EvalBool evaluates expression list to a boolean value. The first returned value
// indicates bool result of the expression list, the second returned value indicates
// whether the result of the expression list is null, it can only be true when the
// first returned values is false.
func EvalBool(ctx sessionctx.Context, exprList CNFExprs, row chunk.Row) (bool, bool, error) {
	hasNull := false
	for _, expr := range exprList {
		data, err := expr.Eval(row)
		if err != nil {
			return false, false, err
		}
		if data.IsNull() {
			return false, false, nil
		}

		i, err := data.ToBool(ctx.GetSessionVars().StmtCtx)
		if err != nil {
			return false, false, err
		}
		if i == 0 {
			return false, false, nil
		}
	}
	if hasNull {
		return false, true, nil
	}
	return true, false, nil
}

var (
	defaultChunkSize = 1024
	selPool          = sync.Pool{
		New: func() interface{} {
			return make([]int, defaultChunkSize)
		},
	}
	zeroPool = sync.Pool{
		New: func() interface{} {
			return make([]int8, defaultChunkSize)
		},
	}
)

func allocSelSlice(n int) []int {
	if n > defaultChunkSize {
		return make([]int, n)
	}
	return selPool.Get().([]int)
}

func deallocateSelSlice(sel []int) {
	if cap(sel) <= defaultChunkSize {
		selPool.Put(sel)
	}
}

func allocZeroSlice(n int) []int8 {
	if n > defaultChunkSize {
		return make([]int8, n)
	}
	return zeroPool.Get().([]int8)
}

func deallocateZeroSlice(isZero []int8) {
	if cap(isZero) <= defaultChunkSize {
		zeroPool.Put(isZero)
	}
}

// VecEvalBool does the same thing as EvalBool but it works in a vectorized manner.
func VecEvalBool(ctx sessionctx.Context, exprList CNFExprs, input *chunk.Chunk, selected, nulls []bool) ([]bool, []bool, error) {
	// If input.Sel() != nil, we will call input.SetSel(nil) to clear the sel slice in input chunk.
	// After the function finished, then we reset the input.Sel().
	// The caller will handle the input.Sel() and selected slices.
	defer input.SetSel(input.Sel())
	input.SetSel(nil)

	n := input.NumRows()
	selected = selected[:0]
	nulls = nulls[:0]
	for i := 0; i < n; i++ {
		selected = append(selected, false)
		nulls = append(nulls, false)
	}

	sel := allocSelSlice(n)
	defer deallocateSelSlice(sel)
	sel = sel[:0]
	for i := 0; i < n; i++ {
		sel = append(sel, i)
	}
	input.SetSel(sel)

	// In isZero slice, -1 means Null, 0 means zero, 1 means not zero
	isZero := allocZeroSlice(n)
	defer deallocateZeroSlice(isZero)
	for _, expr := range exprList {
		eType := expr.GetType().EvalType()
		buf, err := globalColumnAllocator.get(eType, n)
		if err != nil {
			return nil, nil, err
		}

		if err := VecEval(ctx, expr, input, buf); err != nil {
			return nil, nil, err
		}

		err = toBool(ctx.GetSessionVars().StmtCtx, eType, buf, sel, isZero)
		if err != nil {
			return nil, nil, err
		}

		j := 0
		for i := range sel {
			if isZero[i] == -1 {
				if eType != types.ETInt {
					continue
				}
				// In this case, we set this row to null and let it pass this filter.
				// The null flag may be set to false later by other expressions in some cases.
				nulls[sel[i]] = true
				sel[j] = sel[i]
				j++
				continue
			}

			if isZero[i] == 0 {
				continue
			}
			sel[j] = sel[i] // this row passes this filter
			j++
		}
		sel = sel[:j]
		input.SetSel(sel)
		globalColumnAllocator.put(buf)
	}

	for _, i := range sel {
		if !nulls[i] {
			selected[i] = true
		}
	}

	return selected, nulls, nil
}

func toBool(sc *stmtctx.StatementContext, eType types.EvalType, buf *chunk.Column, sel []int, isZero []int8) error {
	var err error
	switch eType {
	case types.ETInt:
		i64s := buf.Int64s()
		for i := range sel {
			if buf.IsNull(i) {
				isZero[i] = -1
			} else {
				if i64s[i] == 0 {
					isZero[i] = 0
				} else {
					isZero[i] = 1
				}
			}
		}
	case types.ETReal:
		f64s := buf.Float64s()
		for i := range sel {
			if buf.IsNull(i) {
				isZero[i] = -1
			} else {
				if types.RoundFloat(f64s[i]) == 0 {
					isZero[i] = 0
				} else {
					isZero[i] = 1
				}
			}
		}
	case types.ETString:
		for i := range sel {
			if buf.IsNull(i) {
				isZero[i] = -1
			} else {
				iVal, err1 := types.StrToInt(sc, buf.GetString(i))
				err = err1
				if iVal == 0 {
					isZero[i] = 0
				} else {
					isZero[i] = 1
				}
			}
		}
	}
	return errors.Trace(err)
}

// VecEval evaluates this expr according to its type.
func VecEval(ctx sessionctx.Context, expr Expression, input *chunk.Chunk, result *chunk.Column) (err error) {
	switch expr.GetType().EvalType() {
	case types.ETInt:
		err = expr.VecEvalInt(ctx, input, result)
	case types.ETReal:
		err = expr.VecEvalReal(ctx, input, result)
	case types.ETString:
		err = expr.VecEvalString(ctx, input, result)
	default:
		err = errors.New(fmt.Sprintf("invalid eval type %v", expr.GetType().EvalType()))
	}
	return
}

// composeConditionWithBinaryOp composes condition with binary operator into a balance deep tree, which benefits a lot for pb decoder/encoder.
func composeConditionWithBinaryOp(ctx sessionctx.Context, conditions []Expression, funcName string) Expression {
	length := len(conditions)
	if length == 0 {
		return nil
	}
	if length == 1 {
		return conditions[0]
	}
	expr := NewFunctionInternal(ctx, funcName,
		types.NewFieldType(mysql.TypeTiny),
		composeConditionWithBinaryOp(ctx, conditions[:length/2], funcName),
		composeConditionWithBinaryOp(ctx, conditions[length/2:], funcName))
	return expr
}

// ComposeCNFCondition composes CNF items into a balance deep CNF tree, which benefits a lot for pb decoder/encoder.
func ComposeCNFCondition(ctx sessionctx.Context, conditions ...Expression) Expression {
	return composeConditionWithBinaryOp(ctx, conditions, ast.LogicAnd)
}

// ComposeDNFCondition composes DNF items into a balance deep DNF tree.
func ComposeDNFCondition(ctx sessionctx.Context, conditions ...Expression) Expression {
	return composeConditionWithBinaryOp(ctx, conditions, ast.LogicOr)
}

func extractBinaryOpItems(conditions *ScalarFunction, funcName string) []Expression {
	var ret []Expression
	for _, arg := range conditions.GetArgs() {
		if sf, ok := arg.(*ScalarFunction); ok && sf.FuncName.L == funcName {
			ret = append(ret, extractBinaryOpItems(sf, funcName)...)
		} else {
			ret = append(ret, arg)
		}
	}
	return ret
}

// FlattenDNFConditions extracts DNF expression's leaf item.
// e.g. or(or(a=1, a=2), or(a=3, a=4)), we'll get [a=1, a=2, a=3, a=4].
func FlattenDNFConditions(DNFCondition *ScalarFunction) []Expression {
	return extractBinaryOpItems(DNFCondition, ast.LogicOr)
}

// FlattenCNFConditions extracts CNF expression's leaf item.
// e.g. and(and(a>1, a>2), and(a>3, a>4)), we'll get [a>1, a>2, a>3, a>4].
func FlattenCNFConditions(CNFCondition *ScalarFunction) []Expression {
	return extractBinaryOpItems(CNFCondition, ast.LogicAnd)
}

// Assignment represents a set assignment in Update, such as
// Update t set c1 = hex(12), c2 = c3 where c2 = 1
type Assignment struct {
	Col *Column
	// ColName indicates its original column name in table schema. It's used for outputting helping message when executing meets some errors.
	ColName model.CIStr
	Expr    Expression
}

// VarAssignment represents a variable assignment in Set, such as set global a = 1.
type VarAssignment struct {
	Name      string
	Expr      Expression
	IsDefault bool
	IsGlobal  bool
	IsSystem  bool
}

// splitNormalFormItems split CNF(conjunctive normal form) like "a and b and c", or DNF(disjunctive normal form) like "a or b or c"
// splitNormalFormItems拆分CNF(合取范式)，如“a and b and c”，或DNF(析取范式)，如“a or b or c”
func splitNormalFormItems(onExpr Expression, funcName string) []Expression {
	switch v := onExpr.(type) {
	case *ScalarFunction:
		if v.FuncName.L == funcName {
			var ret []Expression
			for _, arg := range v.GetArgs() {
				ret = append(ret, splitNormalFormItems(arg, funcName)...)
			}
			return ret
		}
	}
	return []Expression{onExpr}
}

// SplitCNFItems splits CNF items.
// CNF means conjunctive normal form, e.g. "a and b and c".
func SplitCNFItems(onExpr Expression) []Expression {
	return splitNormalFormItems(onExpr, ast.LogicAnd)
}

// SplitDNFItems splits DNF items.
// DNF means disjunctive normal form, e.g. "a or b or c".
// 析取范式的拆解
func SplitDNFItems(onExpr Expression) []Expression {
	return splitNormalFormItems(onExpr, ast.LogicOr)
}

// EvaluateExprWithNull sets columns in schema as null and calculate the final result of the scalar function.
// If the Expression is a non-constant value, it means the result is unknown.
func EvaluateExprWithNull(ctx sessionctx.Context, schema *Schema, expr Expression) Expression {
	switch x := expr.(type) {
	case *ScalarFunction:
		args := make([]Expression, len(x.GetArgs()))
		for i, arg := range x.GetArgs() {
			args[i] = EvaluateExprWithNull(ctx, schema, arg)
		}
		return NewFunctionInternal(ctx, x.FuncName.L, x.RetType, args...)
	case *Column:
		if !schema.Contains(x) {
			return x
		}
		return &Constant{Value: types.Datum{}, RetType: types.NewFieldType(mysql.TypeNull)}
	}
	return expr
}

// TableInfo2SchemaAndNames converts the TableInfo to the schema and name slice.
func TableInfo2SchemaAndNames(ctx sessionctx.Context, dbName model.CIStr, tbl *model.TableInfo) (*Schema, []*types.FieldName) {
	cols, names := ColumnInfos2ColumnsAndNames(ctx, dbName, tbl.Name, tbl.Columns)
	keys := make([]KeyInfo, 0, len(tbl.Indices)+1)
	for _, idx := range tbl.Indices {
		if !idx.Unique || idx.State != model.StatePublic {
			continue
		}
		ok := true
		newKey := make([]*Column, 0, len(idx.Columns))
		for _, idxCol := range idx.Columns {
			find := false
			for i, col := range tbl.Columns {
				if idxCol.Name.L == col.Name.L {
					if !mysql.HasNotNullFlag(col.Flag) {
						break
					}
					newKey = append(newKey, cols[i])
					find = true
					break
				}
			}
			if !find {
				ok = false
				break
			}
		}
		if ok {
			keys = append(keys, newKey)
		}
	}
	if tbl.PKIsHandle {
		for i, col := range tbl.Columns {
			if mysql.HasPriKeyFlag(col.Flag) {
				keys = append(keys, KeyInfo{cols[i]})
				break
			}
		}
	}
	schema := NewSchema(cols...)
	schema.SetUniqueKeys(keys)
	return schema, names
}

// ColumnInfos2ColumnsAndNames converts the ColumnInfo to the *Column and NameSlice.
func ColumnInfos2ColumnsAndNames(ctx sessionctx.Context, dbName, tblName model.CIStr, colInfos []*model.ColumnInfo) ([]*Column, types.NameSlice) {
	columns := make([]*Column, 0, len(colInfos))
	names := make([]*types.FieldName, 0, len(colInfos))
	for i, col := range colInfos {
		if col.State != model.StatePublic {
			continue
		}
		names = append(names, &types.FieldName{
			OrigTblName: tblName,
			OrigColName: col.Name,
			DBName:      dbName,
			TblName:     tblName,
			ColName:     col.Name,
		})
		newCol := &Column{
			RetType:  &col.FieldType,
			ID:       col.ID,
			UniqueID: ctx.GetSessionVars().AllocPlanColumnID(),
			Index:    col.Offset,
			OrigName: names[i].String(),
		}
		columns = append(columns, newCol)
	}
	return columns, names
}

// NewValuesFunc creates a new values function.
func NewValuesFunc(ctx sessionctx.Context, offset int, retTp *types.FieldType) *ScalarFunction {
	fc := &valuesFunctionClass{baseFunctionClass{ast.Values, 0, 0}, offset, retTp}
	bt, err := fc.getFunction(ctx, nil)
	terror.Log(err)
	return &ScalarFunction{
		FuncName: model.NewCIStr(ast.Values),
		RetType:  retTp,
		Function: bt,
	}
}

// FindFieldName finds the column name from NameSlice.
func FindFieldName(names types.NameSlice, astCol *ast.ColumnName) (int, error) {
	dbName, tblName, colName := astCol.Schema, astCol.Table, astCol.Name
	idx := -1
	for i, name := range names {
		if (dbName.L == "" || dbName.L == name.DBName.L) &&
			(tblName.L == "" || tblName.L == name.TblName.L) &&
			(colName.L == name.ColName.L) {
			if idx == -1 {
				idx = i
			} else {
				return -1, errNonUniq.GenWithStackByArgs(name.String(), "field list")
			}
		}
	}
	return idx, nil
}
