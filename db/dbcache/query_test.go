package dbcache

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestQueryRedisHitSkipsDB(t *testing.T) {
	e, fs, src, _, b := newTestEngine(t)
	cached := &testUser{ID: 7, Name: "cached"}
	fields, err := encodeRow(b.meta, reflect.ValueOf(cached).Elem())
	if err != nil {
		t.Fatalf("encodeRow: %v", err)
	}
	if err := fs.HSetAll(context.Background(), b.keys.rowPrefix+"7", fields, 0); err != nil {
		t.Fatalf("HSetAll: %v", err)
	}

	var out testUser
	if err := e.query[testUser](context.Background(), "test_user", &idCond{ID: ptrInt(7)}, &out); err != nil {
		t.Fatalf("query: %v", err)
	}
	if src.oneCalls != 0 {
		t.Fatalf("Redis 命中不应回源，实际调用 %d 次", src.oneCalls)
	}
	if out.Name != "cached" {
		t.Fatalf("out.Name=%q", out.Name)
	}
}

func TestQueryMissBackfills(t *testing.T) {
	e, fs, src, _, b := newTestEngine(t)
	src.one = &testUser{ID: 7, Name: "db"}

	var out testUser
	if err := e.query[testUser](context.Background(), "test_user", &idCond{ID: ptrInt(7)}, &out); err != nil {
		t.Fatalf("query: %v", err)
	}
	if src.oneCalls != 1 {
		t.Fatalf("未命中应回源一次，实际 %d 次", src.oneCalls)
	}
	if out.Name != "db" {
		t.Fatalf("out.Name=%q", out.Name)
	}
	if h := fs.hash(b.keys.rowPrefix + "7"); h["name"] != "db" {
		t.Fatalf("回填内容=%v", h)
	}
}

func TestQueryKeyConditionWithExtraFilterGoesDB(t *testing.T) {
	e, _, src, _, _ := newTestEngine(t)
	src.one = &testUser{ID: 7, Name: "db"}

	cond := struct {
		ID   *int64  `cache:"id"`
		Name *string `cache:"name"`
	}{ID: ptrInt(7), Name: ptrStr("db")}
	var out testUser
	if err := e.query[testUser](context.Background(), "test_user", &cond, &out); err != nil {
		t.Fatalf("query: %v", err)
	}
	if src.oneCalls != 1 {
		t.Fatalf("含非键列应回源，实际 %d 次", src.oneCalls)
	}
}

func TestQueryNonKeyCondition(t *testing.T) {
	e, fs, src, _, b := newTestEngine(t)
	src.one = &testUser{ID: 7, Name: "db"}

	var out testUser
	if err := e.query[testUser](context.Background(), "test_user", &nameCond{Name: ptrStr("db")}, &out); err != nil {
		t.Fatalf("query: %v", err)
	}
	if src.oneCalls != 1 {
		t.Fatalf("非键条件应回源一次，实际 %d 次", src.oneCalls)
	}
	if _, ok := fs.hash(b.keys.rowPrefix + "7")["name"]; !ok {
		t.Fatal("命中的行应回填 Redis")
	}
}

func TestQueryListBackfillsAll(t *testing.T) {
	e, fs, src, _, b := newTestEngine(t)
	src.list = []*testUser{{ID: 1, Name: "a"}, {ID: 2, Name: "b"}}

	var out []testUser
	if err := e.query[testUser](context.Background(), "test_user", &nameCond{Name: ptrStr("x")}, &out); err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(out) != 2 || src.listCalls != 1 {
		t.Fatalf("列表结果=%v calls=%d", out, src.listCalls)
	}
	if fs.hash(b.keys.rowPrefix + "1")["name"] != "a" || fs.hash(b.keys.rowPrefix + "2")["name"] != "b" {
		t.Fatalf("列表回填不完整")
	}
}

func TestQueryEmptyCondition(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	var out testUser
	err := e.query[testUser](context.Background(), "test_user", &idCond{}, &out)
	if !errors.Is(err, ErrEmptyCondition) {
		t.Fatalf("期望 ErrEmptyCondition，实际 %v", err)
	}
}

func TestQueryBadCondition(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	var out testUser
	err := e.query[testUser](context.Background(), "test_user", idCond{ID: ptrInt(1)}, &out)
	if !errors.Is(err, ErrBadCond) {
		t.Fatalf("期望 ErrBadCond，实际 %v", err)
	}
}

func TestQueryBadOut(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	var out int
	err := e.query[testUser](context.Background(), "test_user", &idCond{ID: ptrInt(1)}, &out)
	if !errors.Is(err, ErrBadOut) {
		t.Fatalf("期望 ErrBadOut，实际 %v", err)
	}
}

func TestQueryUnboundAndTypeMismatch(t *testing.T) {
	e, _, _, _, _ := newTestEngine(t)
	var out testUser
	if err := e.query[testUser](context.Background(), "nope", &idCond{ID: ptrInt(1)}, &out); !errors.Is(err, ErrTableUnbound) {
		t.Fatalf("期望 ErrTableUnbound，实际 %v", err)
	}
	var other testOther
	if err := e.query[testOther](context.Background(), "test_user", &idCond{ID: ptrInt(1)}, &other); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("期望 ErrTypeMismatch，实际 %v", err)
	}
}

func TestQueryNoRows(t *testing.T) {
	e, _, src, _, _ := newTestEngine(t)
	src.one = nil
	var out testUser
	err := e.query[testUser](context.Background(), "test_user", &idCond{ID: ptrInt(99)}, &out)
	if !errors.Is(err, ErrNoRows) {
		t.Fatalf("期望 ErrNoRows，实际 %v", err)
	}
}
