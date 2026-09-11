package mysql

import "testing"

func TestQuery_NewQuery(t *testing.T) {
	q := NewQuery()
	if q == nil {
		t.Fatal("NewQuery 不应返回 nil")
	}
	if q.whereExpr != "" {
		t.Fatal("新查询 whereExpr 应为空")
	}
}

func TestQuery_Eq(t *testing.T) {
	q := NewQuery().Eq("id", 1)
	if q.whereExpr != "id = ?" {
		t.Fatalf("Eq: whereExpr=%q", q.whereExpr)
	}
	if len(q.whereArgs) != 1 || q.whereArgs[0] != 1 {
		t.Fatalf("Eq: args=%v", q.whereArgs)
	}
}

func TestQuery_Chain(t *testing.T) {
	q := NewQuery().
		Where("id > ?", 10).
		Gt("age", 18).
		OrderDesc("created_at").
		Limit(20).
		Offset(40)
	if q.whereExpr != "age > ?" {
		t.Fatalf("Gt 应覆盖 whereExpr: %q", q.whereExpr)
	}
	if len(q.orders) != 1 {
		t.Fatalf("OrderDesc 数量异常: %d", len(q.orders))
	}
	if q.limit != 20 || q.offset != 40 {
		t.Fatalf("Limit/Offset: %d/%d", q.limit, q.offset)
	}
}

func TestQuery_In(t *testing.T) {
	q := NewQuery().In("id", 1, 2, 3)
	if q.whereExpr != "id IN (?,?,?)" {
		t.Fatalf("In: whereExpr=%q", q.whereExpr)
	}
	if len(q.whereArgs) != 3 {
		t.Fatalf("In: args=%v", q.whereArgs)
	}
}

func TestQuery_NotIn_Empty(t *testing.T) {
	q := NewQuery().NotIn("id")
	if q.whereExpr != "" {
		t.Fatalf("空 NotIn 应不设置 where: %q", q.whereExpr)
	}
}

func TestQuery_Between(t *testing.T) {
	q := NewQuery().Between("age", 18, 30)
	if q.whereExpr != "age BETWEEN ? AND ?" {
		t.Fatalf("Between: %q", q.whereExpr)
	}
}

func TestQuery_IsNull(t *testing.T) {
	q := NewQuery().IsNull("deleted_at")
	if q.whereExpr != "deleted_at IS NULL" {
		t.Fatalf("IsNull: %q", q.whereExpr)
	}
}

func TestQuery_SelectGroupHaving(t *testing.T) {
	q := NewQuery().
		Select("id, name").
		GroupBy("status").
		Having("COUNT(*) > ?", 5)
	if q.selectString() != "id, name" {
		t.Fatalf("Select: %q", q.selectString())
	}
	if q.groupString() != "status" {
		t.Fatalf("GroupBy: %q", q.groupString())
	}
	if q.having != "COUNT(*) > ?" {
		t.Fatalf("Having: %q", q.having)
	}
}

func TestQuery_OrderByRaw(t *testing.T) {
	q := NewQuery().OrderBy("FIELD(id, 1, 2, 3)")
	if q.orderString() != "FIELD(id, 1, 2, 3)" {
		t.Fatalf("OrderBy: %q", q.orderString())
	}
}