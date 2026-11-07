package dbcache

import (
	"context"
	"errors"
	"testing"
)

// TestDeleteRemovesCacheAndRow 删除后行 hash 与脏账均清空，再查必须回源。
func TestDeleteRemovesCacheAndRow(t *testing.T) {
	e, fs, src, _, b := newTestEngine(t)
	ctx := context.Background()
	if err := e.save[testUser](ctx, "test_user", "", &testUser{ID: 7, Name: "cache"}, true); err != nil {
		t.Fatalf("save: %v", err)
	}
	rowKey := b.keys.rowPrefix + "7"
	if len(fs.hash(rowKey)) == 0 {
		t.Fatal("保存后行 hash 应为非空")
	}

	src.one = nil
	if err := e.remove(ctx, b, &idCond{ID: ptrInt(7)}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if src.delCalls != 1 {
		t.Fatalf("数据库删除调用次数=%d", src.delCalls)
	}
	if h := fs.hash(rowKey); len(h) != 0 {
		t.Fatalf("删除后行 hash 应为空，实际 %v", h)
	}
	if n := fs.dirtyCount(b.keys.dirty); n != 0 {
		t.Fatalf("删除后脏账应为空，实际 %d", n)
	}

	var out testUser
	err := e.query[testUser](ctx, "test_user", &idCond{ID: ptrInt(7)}, &out)
	if !errors.Is(err, ErrNoRows) {
		t.Fatalf("删除后查询应回源并返回 ErrNoRows，实际 %v", err)
	}
}

// TestDeleteDropsPendingDirty 删除前的待落库脏账不得在之后的 flush 里写回。
func TestDeleteDropsPendingDirty(t *testing.T) {
	e, fs, _, sink, b := newTestEngine(t)
	ctx := context.Background()
	if err := e.save[testUser](ctx, "test_user", "", &testUser{ID: 7, Name: "pending"}, false); err != nil {
		t.Fatalf("save: %v", err)
	}
	if n := fs.dirtyCount(b.keys.dirty); n != 1 {
		t.Fatalf("应有一条待落库脏账，实际 %d", n)
	}
	if err := e.remove(ctx, b, &idCond{ID: ptrInt(7)}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := e.flushAll(ctx, b); err != nil {
		t.Fatalf("flushAll: %v", err)
	}
	if rows := sink.snapshot(); len(rows) != 0 {
		t.Fatalf("删除后 flush 不应写回任何行，实际 %d", len(rows))
	}
}

// TestDeleteTombBlocksUnclaimedDirty 墓碑生效期间，残留脏账也不得写回。
func TestDeleteTombBlocksUnclaimedDirty(t *testing.T) {
	e, fs, _, sink, b := newTestEngine(t)
	ctx := context.Background()
	if err := e.save[testUser](ctx, "test_user", "", &testUser{ID: 7, Name: "pending"}, false); err != nil {
		t.Fatalf("save: %v", err)
	}
	// 模拟删除流程已立墓碑但尚未走完清理：脏账与行 hash 都还在。
	if err := fs.SetTomb(ctx, b.keys.tombPrefix+"7", 0); err != nil {
		t.Fatalf("SetTomb: %v", err)
	}
	if err := e.flushAll(ctx, b); err != nil {
		t.Fatalf("flushAll: %v", err)
	}
	if rows := sink.snapshot(); len(rows) != 0 {
		t.Fatalf("墓碑生效时不应回写，实际 %d", len(rows))
	}
	if n := fs.dirtyCount(b.keys.dirty); n != 0 {
		t.Fatalf("墓碑命中后脏账应销掉，实际 %d", n)
	}
}

// TestDeleteThenSaveRestoresRow 删除后重建同一主键必须能正常落库。
func TestDeleteThenSaveRestoresRow(t *testing.T) {
	e, fs, _, sink, b := newTestEngine(t)
	ctx := context.Background()
	if err := e.remove(ctx, b, &idCond{ID: ptrInt(7)}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !fs.hasTomb(b.keys.tombPrefix + "7") {
		t.Fatal("删除后应留下墓碑")
	}
	if err := e.save[testUser](ctx, "test_user", "", &testUser{ID: 7, Name: "again"}, true); err != nil {
		t.Fatalf("save: %v", err)
	}
	if fs.hasTomb(b.keys.tombPrefix + "7") {
		t.Fatal("重建行后墓碑应被清除")
	}
	rows := sink.snapshot()
	if len(rows) != 1 || rows[0].Name != "again" {
		t.Fatalf("重建行应落库，实际 %v", rows)
	}
}

// TestDeletePartialKey 非键条件必须被拒绝，避免误删多行。
func TestDeletePartialKey(t *testing.T) {
	e, _, _, _, b := newTestEngine(t)
	err := e.remove(context.Background(), b, &nameCond{Name: ptrStr("db")})
	if !errors.Is(err, ErrPartialKey) {
		t.Fatalf("非键条件应返回 ErrPartialKey，实际 %v", err)
	}
}

// TestDeleteDBFailureKeepsCache 数据库删除失败时不得清缓存，墓碑需回滚。
func TestDeleteDBFailureKeepsCache(t *testing.T) {
	e, fs, src, _, b := newTestEngine(t)
	ctx := context.Background()
	if err := e.save[testUser](ctx, "test_user", "", &testUser{ID: 7, Name: "keep"}, true); err != nil {
		t.Fatalf("save: %v", err)
	}
	src.delErr = errSinkDown

	err := e.remove(ctx, b, &idCond{ID: ptrInt(7)})
	if !errors.Is(err, errSinkDown) {
		t.Fatalf("应返回数据库删除错误，实际 %v", err)
	}
	if h := fs.hash(b.keys.rowPrefix + "7"); len(h) == 0 {
		t.Fatal("数据库删除失败时行 hash 应保留")
	}
	if fs.hasTomb(b.keys.tombPrefix + "7") {
		t.Fatal("数据库删除失败时墓碑应回滚")
	}
}
