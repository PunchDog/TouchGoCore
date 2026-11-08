package dbcache

// ============================================================================
// 结构化聚合表达式（不暴露 SQL 文本）
//
// 看板指标需要 COALESCE(SUM(x),0) / SUM(GREATEST(a - COALESCE(b,0),0)) /
// COUNT(DISTINCT x) / DATE(col + INTERVAL ? SECOND) 这类形状。调用方用
// Col/Lit/Sub/Greatest/Coalesce/SumExpr/CountExpr/DateBucket 组合，
// 由本层渲染成 SQL；列名一律校验绑定元数据，字面量一律走占位符。
// ============================================================================

import (
	"fmt"
	"strings"
)

// Expr 聚合表达式节点。
type Expr interface{ exprNode() }

type colExpr struct {
	table string
	name  string
}

func (colExpr) exprNode() {}

type litExpr struct{ v any }

func (litExpr) exprNode() {}

type binExpr struct {
	op   string
	l, r Expr
}

func (binExpr) exprNode() {}

type callExpr struct {
	fn   string
	args []Expr
}

func (callExpr) exprNode() {}

type starExpr struct{}

func (starExpr) exprNode() {}

type sumExpr struct{ arg Expr }

func (sumExpr) exprNode() {}

type countExpr struct {
	arg      Expr
	distinct bool
}

func (countExpr) exprNode() {}

type dateBucketExpr struct {
	col       string
	offsetSec int
}

func (dateBucketExpr) exprNode() {}

// Col 引用一个绑定表列。
func Col(name string) Expr { return colExpr{name: name} }

// TCol 引用一个带表别名限定的列（JOIN 查询）。
func TCol(table, name string) Expr { return colExpr{table: table, name: name} }

// Lit 一个字面量（渲染成占位符）。
func Lit(v any) Expr { return litExpr{v: v} }

// Sub 减法表达式 a - b。
func Sub(a, b Expr) Expr { return binExpr{op: "-", l: a, r: b} }

// Greatest GREATEST(args...)。
func Greatest(args ...Expr) Expr { return callExpr{fn: "GREATEST", args: args} }

// Coalesce COALESCE(args...)。
func Coalesce(args ...Expr) Expr { return callExpr{fn: "COALESCE", args: args} }

// SumExpr SUM(arg)。
func SumExpr(arg Expr) Expr { return sumExpr{arg: arg} }

// CountExpr COUNT/COUNT(DISTINCT arg)。
func CountExpr(arg Expr, distinct bool) Expr { return countExpr{arg: arg, distinct: distinct} }

// Star 聚合里的 *（仅 COUNT 使用）。
func Star() Expr { return starExpr{} }

// DateBucket 把列按「加 offsetSec 秒后的日期」分桶（DATE(col + INTERVAL ? SECOND)）。
func DateBucket(column string, offsetSec int) Expr {
	return dateBucketExpr{col: column, offsetSec: offsetSec}
}

// RenderExpr 渲染表达式（列名按绑定校验，字面量走占位符）。
// resolver 以表别名/表名为键；base 是基表绑定。
func RenderExpr(base *binding, resolver map[string]*binding, e Expr) (string, []any, error) {
	switch x := e.(type) {
	case colExpr:
		b := base
		prefix := ""
		if x.table != "" {
			var ok bool
			b, ok = resolver[x.table]
			if !ok || b == nil {
				return "", nil, ErrBadColumn
			}
			prefix = "`" + x.table + "`."
		}
		if err := validateColumn(b, x.name); err != nil {
			return "", nil, err
		}
		return prefix + "`" + x.name + "`", nil, nil
	case litExpr:
		return "?", []any{x.v}, nil
	case binExpr:
		l, la, err := RenderExpr(base, resolver, x.l)
		if err != nil {
			return "", nil, err
		}
		r, ra, err := RenderExpr(base, resolver, x.r)
		if err != nil {
			return "", nil, err
		}
		return "(" + l + " " + x.op + " " + r + ")", append(la, ra...), nil
	case callExpr:
		switch x.fn {
		case "GREATEST", "COALESCE":
		default:
			return "", nil, ErrBadColumn
		}
		parts := make([]string, 0, len(x.args))
		var args []any
		for _, a := range x.args {
			s, sa, err := RenderExpr(base, resolver, a)
			if err != nil {
				return "", nil, err
			}
			parts = append(parts, s)
			args = append(args, sa...)
		}
		return x.fn + "(" + strings.Join(parts, ", ") + ")", args, nil
	case starExpr:
		return "*", nil, nil
	case sumExpr:
		s, args, err := RenderExpr(base, resolver, x.arg)
		if err != nil {
			return "", nil, err
		}
		// SUM 在 MySQL 里返回 DECIMAL；显式 CAST 成 SIGNED，扫描进 int64 才稳定。
		return "CAST(SUM(" + s + ") AS SIGNED)", args, nil
	case countExpr:
		s, args, err := RenderExpr(base, resolver, x.arg)
		if err != nil {
			return "", nil, err
		}
		if x.distinct {
			return "COUNT(DISTINCT " + s + ")", args, nil
		}
		return "COUNT(" + s + ")", args, nil
	case dateBucketExpr:
		if err := validateColumn(base, x.col); err != nil {
			return "", nil, err
		}
		return "DATE(`" + x.col + "` + INTERVAL ? SECOND)", []any{x.offsetSec}, nil
	default:
		return "", nil, fmt.Errorf("%w: 未知表达式 %T", ErrBadColumn, e)
	}
}
