package dbcache

import (
	"reflect"
	"testing"
)

func TestBuildMetaColumns(t *testing.T) {
	m, err := buildMeta(typeOf[testUser](), nil, backendMySQL)
	if err != nil {
		t.Fatalf("buildMeta: %v", err)
	}
	want := []string{"id", "name", "age", "active", "score", "created"}
	if len(m.Fields) != len(want) {
		t.Fatalf("字段数=%d，期望 %d（%v）", len(m.Fields), len(want), refColumns(m))
	}
	for i, col := range want {
		if m.Fields[i].Column != col {
			t.Fatalf("第 %d 列=%q，期望 %q", i, m.Fields[i].Column, col)
		}
	}
	if !m.HasPK || m.PKCol != "id" {
		t.Fatalf("主键=%q hasPK=%v，期望 id", m.PKCol, m.HasPK)
	}
	if !reflect.DeepEqual(m.KeyCols, []string{"id"}) {
		t.Fatalf("KeyCols=%v，期望 [id]", m.KeyCols)
	}
	for _, f := range m.Fields {
		if f.Column == "Skip" || f.Column == "Private" || f.Column == "private" {
			t.Fatalf("非列字段未跳过: %s", f.Column)
		}
	}
}

func TestBuildMetaCompositeKey(t *testing.T) {
	m, err := buildMeta(typeOf[testUser](), []string{"name", "id"}, backendMySQL)
	if err != nil {
		t.Fatalf("buildMeta: %v", err)
	}
	if !reflect.DeepEqual(m.KeyCols, []string{"name", "id"}) {
		t.Fatalf("KeyCols=%v", m.KeyCols)
	}
}

func TestBuildMetaBadKeyColumn(t *testing.T) {
	if _, err := buildMeta(typeOf[testUser](), []string{"nope"}, backendMySQL); err == nil {
		t.Fatal("未知 key 列应报错")
	}
}

func TestCacheTagSkip(t *testing.T) {
	type model struct {
		ID   int64  `cache:"id"`
		Keep string `cache:"keep"`
		Drop string `cache:"-"`
	}
	m, err := buildMeta(typeOf[model](), nil, backendMySQL)
	if err != nil {
		t.Fatalf("buildMeta: %v", err)
	}
	if len(m.Fields) != 2 {
		t.Fatalf("字段数=%d，期望 2（%v）", len(m.Fields), refColumns(m))
	}
}

func TestKeyFromRowZeroValue(t *testing.T) {
	m, err := buildMeta(typeOf[testUser](), nil, backendMySQL)
	if err != nil {
		t.Fatalf("buildMeta: %v", err)
	}
	if _, err := keyFromRow(m, reflect.ValueOf(&testUser{}).Elem()); err == nil {
		t.Fatal("主键为 0 应返回 ErrKeyMissing")
	}
	key, err := keyFromRow(m, reflect.ValueOf(&testUser{ID: 42}).Elem())
	if err != nil {
		t.Fatalf("keyFromRow: %v", err)
	}
	if key != "42" {
		t.Fatalf("key=%q，期望 42", key)
	}
}

func TestSnakeCase(t *testing.T) {
	cases := map[string]string{
		"ID":       "id",
		"MemberID": "member_id",
		"Name":     "name",
		"URLPath":  "url_path",
	}
	for in, want := range cases {
		if got := snakeCase(in); got != want {
			t.Fatalf("snakeCase(%q)=%q，期望 %q", in, got, want)
		}
	}
}

func refColumns(m *tableMeta) []string {
	out := make([]string, len(m.Fields))
	for i, f := range m.Fields {
		out[i] = f.Column
	}
	return out
}
