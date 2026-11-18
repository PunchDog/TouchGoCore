package postgres

import "strings"

// Query 是类型安全的链式查询构建器
type Query struct {
	whereExpr  string
	whereArgs  []any
	orders     []string
	limit      int
	offset     int
	selects    []string
	groups     []string
	having     string
	havingArgs []any
}

// NewQuery 构造空查询
func NewQuery() *Query {
	return &Query{}
}

// Where 追加 WHERE 条件（多次调用以 AND 连接，与 GORM 的 Where 语义一致）。
//
// 修复记录：旧实现直接覆盖 whereExpr，链式 .Where(a).Where(b) 会静默丢掉
// 前一个条件——租户隔离等前置条件被覆盖即成跨租户漏洞。现改为 AND 追加。
// 含 OR 的表达式请自行加括号：q.Where("(a = ? OR b = ?)", x, y)。
func (q *Query) Where(expr string, args ...any) *Query {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return q
	}
	if q.whereExpr == "" {
		q.whereExpr = expr
	} else {
		q.whereExpr = "(" + q.whereExpr + ") AND (" + expr + ")"
	}
	q.whereArgs = append(q.whereArgs, args...)
	return q
}

// Eq 等值
func (q *Query) Eq(col string, v any) *Query { return q.Where(col+" = ?", v) }

// Ne 不等
func (q *Query) Ne(col string, v any) *Query { return q.Where(col+" <> ?", v) }

// Lt <
func (q *Query) Lt(col string, v any) *Query { return q.Where(col+" < ?", v) }

// Lte <=
func (q *Query) Lte(col string, v any) *Query { return q.Where(col+" <= ?", v) }

// Gt >
func (q *Query) Gt(col string, v any) *Query { return q.Where(col+" > ?", v) }

// Gte >=
func (q *Query) Gte(col string, v any) *Query { return q.Where(col+" >= ?", v) }

// Like 模糊匹配
func (q *Query) Like(col string, v any) *Query { return q.Where(col+" LIKE ?", v) }

// In IN 子句
func (q *Query) In(col string, vs ...any) *Query {
	if len(vs) == 0 {
		return q.Where("1 = 0")
	}
	placeholders := strings.Repeat("?,", len(vs))
	placeholders = placeholders[:len(placeholders)-1]
	return q.Where(col+" IN ("+placeholders+")", vs...)
}

// NotIn NOT IN
func (q *Query) NotIn(col string, vs ...any) *Query {
	if len(vs) == 0 {
		return q
	}
	placeholders := strings.Repeat("?,", len(vs))
	placeholders = placeholders[:len(placeholders)-1]
	return q.Where(col+" NOT IN ("+placeholders+")", vs...)
}

// Between BETWEEN
func (q *Query) Between(col string, lo, hi any) *Query {
	return q.Where(col+" BETWEEN ? AND ?", lo, hi)
}

// IsNull IS NULL
func (q *Query) IsNull(col string) *Query { return q.Where(col + " IS NULL") }

// IsNotNull IS NOT NULL
func (q *Query) IsNotNull(col string) *Query { return q.Where(col + " IS NOT NULL") }

// OrderAsc 升序
func (q *Query) OrderAsc(col string) *Query {
	q.orders = append(q.orders, col+" ASC")
	return q
}

// OrderDesc 降序
func (q *Query) OrderDesc(col string) *Query {
	q.orders = append(q.orders, col+" DESC")
	return q
}

// OrderBy 追加自定义排序
func (q *Query) OrderBy(raw string) *Query {
	q.orders = append(q.orders, raw)
	return q
}

// Limit
func (q *Query) Limit(n int) *Query { q.limit = n; return q }

// Offset
func (q *Query) Offset(n int) *Query { q.offset = n; return q }

// Select 选择返回列
func (q *Query) Select(cols ...string) *Query {
	q.selects = append([]string(nil), cols...)
	return q
}

// GroupBy
func (q *Query) GroupBy(cols ...string) *Query {
	q.groups = append([]string(nil), cols...)
	return q
}

// Having
func (q *Query) Having(expr string, args ...any) *Query {
	q.having = expr
	q.havingArgs = append([]any(nil), args...)
	return q
}

// orderString 拼接 ORDER BY 子句
func (q *Query) orderString() string { return strings.Join(q.orders, ", ") }

// groupString 拼接 GROUP BY 子句
func (q *Query) groupString() string { return strings.Join(q.groups, ", ") }

// selectString 拼接 SELECT 列表
func (q *Query) selectString() string { return strings.Join(q.selects, ", ") }
