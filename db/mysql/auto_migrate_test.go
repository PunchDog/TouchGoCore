package mysql

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// fakeRepo 用最小 gorm.DB 替代品替代复杂 mock，验证 runWithAutoMigrate 行为
func TestRunWithAutoMigrate_Disabled(t *testing.T) {
	// autoMigrate=false 时，runWithAutoMigrate 只透传 fn，不尝试迁移
	r := &Repository[struct{ ID int }]{
		hooks: &repoHooks[struct{ ID int }]{},
	}
	calls := 0
	err := r.runWithAutoMigrate(t.Context(), "Test", func() error {
		calls++
		return errors.New("ignored")
	})
	if err == nil || err.Error() != "ignored" {
		t.Fatalf("err=%v 应透传", err)
	}
	if calls != 1 {
		t.Fatalf("应只调用一次 fn，got %d", calls)
	}
	if r.hooks.migrated.Load() {
		t.Fatal("disabled 时不应修改 migrated 标志")
	}
}

func TestRunWithAutoMigrate_AlreadyMigrated_SkipsDetection(t *testing.T) {
	r := &Repository[struct{ ID int }]{
		hooks: &repoHooks[struct{ ID int }]{autoMigrate: true},
	}
	r.hooks.migrated.Store(true)
	calls := 0
	err := r.runWithAutoMigrate(t.Context(), "Test", func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if calls != 1 {
		t.Fatalf("应只调用一次 fn，got %d", calls)
	}
}

func TestIsTableNotExist_VariousSources(t *testing.T) {
	if IsTableNotExist(nil) {
		t.Fatal("nil 应为 false")
	}

	// 直接 MySQL 错误
	if !IsTableNotExist(&mysql.MySQLError{Number: 1146}) {
		t.Fatal("MySQL 1146 应识别")
	}

	// fmt.Errorf 包装
	wrapped := wrap("SomeOp", &mysql.MySQLError{Number: 1146}, KindSyntax)
	if !IsTableNotExist(wrapped) {
		t.Fatal("被 *Error 包装的 1146 应识别")
	}

	// 双重包装
	doubleWrapped := wrap("Outer", wrapped, KindSyntax)
	if !IsTableNotExist(doubleWrapped) {
		t.Fatal("双重包装的 1146 应识别")
	}

	// 其他错误
	if IsTableNotExist(&mysql.MySQLError{Number: 1062}) {
		t.Fatal("1062 不应识别为表不存在")
	}
	if IsTableNotExist(errors.New("random")) {
		t.Fatal("随机错误不应识别")
	}
}

func TestHooks_MigratedConcurrent(t *testing.T) {
	// 验证 atomic.Bool 在并发场景下的可见性
	h := &repoHooks[struct{ ID int }]{autoMigrate: true}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.migrated.Store(true)
			_ = h.migrated.Load()
		}()
	}
	wg.Wait()
	if !h.migrated.Load() {
		t.Fatal("atomic.Bool 并发赋值后应可见")
	}
}

func TestMakeHooks_InheritsClientAutoMigrate(t *testing.T) {
	// 通过 mock Client 验证 makeHooks 读取 Client.AutoMigrateEnabled()
	c := &Client{opts: options{autoMigrate: true}}
	h := makeHooks[struct{ ID int }](c)
	if !h.autoMigrate {
		t.Fatal("应继承 Client.autoMigrate=true")
	}
	c2 := &Client{opts: options{autoMigrate: false}}
	h2 := makeHooks[struct{ ID int }](c2)
	if h2.autoMigrate {
		t.Fatal("应继承 Client.autoMigrate=false")
	}
}

func TestWithAutoMigrate_OptionApplies(t *testing.T) {
	// 验证 Option 函数修改了 options.autoMigrate
	o := defaultOptions()
	WithAutoMigrate(true)(&o)
	if !o.autoMigrate {
		t.Fatal("WithAutoMigrate(true) 未生效")
	}
	WithAutoMigrate(false)(&o)
	if o.autoMigrate {
		t.Fatal("WithAutoMigrate(false) 未生效")
	}
}

func TestRepoHooks_AutoMigrateDefaultFalse(t *testing.T) {
	// 默认 newRepoHooks 不应开启 autoMigrate
	h := newRepoHooks[struct{ ID int }]()
	if h.autoMigrate {
		t.Fatal("默认 autoMigrate 应为 false")
	}
	if h.migrated.Load() {
		t.Fatal("默认 migrated 应为 false")
	}
}

// counterAtomic 用于验证仅触发一次迁移
type counterAtomic struct {
	count atomic.Int32
}

func TestRunWithAutoMigrate_PreservesNonTableErrors(t *testing.T) {
	// autoMigrate=true + migrated=false + 非表不存在错误 → 应直接透传，不尝试迁移
	r := &Repository[struct{ ID int }]{
		hooks: &repoHooks[struct{ ID int }]{autoMigrate: true},
	}
	called := 0
	otherErr := errors.New("connection refused")
	err := r.runWithAutoMigrate(t.Context(), "Test", func() error {
		called++
		return otherErr
	})
	if err != otherErr {
		t.Fatalf("err 应透传 otherErr，got %v", err)
	}
	if called != 1 {
		t.Fatalf("应只调用一次 fn，got %d", called)
	}
	if r.hooks.migrated.Load() {
		t.Fatal("非表不存在错误不应设置 migrated=true")
	}
}