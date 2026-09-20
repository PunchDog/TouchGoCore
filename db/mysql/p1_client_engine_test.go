package mysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"testing"
	"time"

	"touchgocore/config"
	"touchgocore/db/dbmap"

	"gorm.io/gorm"
)

// 离线探测用的假驱动：sql.DB 只需能建立/关闭连接即可，
// 未实现 driver.Pinger 的连接会让 Ping 直接返回 nil，用于走通健康检查分支。
const fakeDriverName = "touchgocore-mysql-fake"

type fakeDriver struct{}

type fakeConn struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return fakeConn{}, nil }

func (fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (fakeConn) Close() error                        { return nil }
func (fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }

var registerFakeDriver sync.Once

func newFakeEngine(t *testing.T) *gorm.DB {
	t.Helper()
	registerFakeDriver.Do(func() { sql.Register(fakeDriverName, fakeDriver{}) })
	sqlDB, err := sql.Open(fakeDriverName, "fake-dsn")
	if err != nil {
		t.Fatalf("打开假连接失败: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	// 只填 Config.ConnPool：DB() 会走 *sql.DB 断言分支，不需要完整 gorm 初始化
	return &gorm.DB{Config: &gorm.Config{ConnPool: sqlDB}}
}

func probeCfg() *config.MySqlDBConfig {
	return &config.MySqlDBConfig{Host: "127.0.0.1:1", Username: "u", Password: "p", DBName: "probe"}
}

type engineProbeRow struct {
	ID uint `gorm:"primaryKey"`
}

func (engineProbeRow) TableName() string { return "engine_probe_row" }

// TestConnectOnlyHitAdoptEngine 命中注册表必须接管引擎实例。
// 修复前 connectOnly 只把引擎留在局部变量里，复用路径返回空壳客户端，
// 任何后续查询都在 nil *gorm.DB 上 panic。
func TestConnectOnlyHitAdoptEngine(t *testing.T) {
	key := "probe-adopt-engine"
	dbmap.Global.Delete(key)
	defer dbmap.Global.Delete(key)

	engine := newFakeEngine(t)
	dbmap.Global.Store(key, engine)

	c := &Client{cfg: probeCfg()}
	if !c.connectOnly(key) {
		t.Fatal("健康引擎应当命中复用")
	}
	if got := c.engine.Load(); got != engine {
		t.Fatalf("复用路径未接管引擎: got %p want %p", got, engine)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("接管后 Ping 应成功: %v", err)
	}
	if _, err := c.Stats(); err != nil {
		t.Fatalf("接管后 Stats 应成功: %v", err)
	}
}

// TestConnectOnlyEvictsDeadEngine 已关闭的连接必须被判定失效并清出注册表，
// 否则坏实例会永久留在表里让后续客户端全部拿到死连接。
func TestConnectOnlyEvictsDeadEngine(t *testing.T) {
	key := "probe-dead-engine"
	dbmap.Global.Delete(key)
	defer dbmap.Global.Delete(key)

	engine := newFakeEngine(t)
	sqlDB, err := engine.DB()
	if err != nil {
		t.Fatalf("取底层 sql.DB 失败: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("关闭假连接失败: %v", err)
	}
	dbmap.Global.Store(key, engine)

	c := &Client{cfg: probeCfg()}
	if c.connectOnly(key) {
		t.Fatal("已关闭的连接不应命中复用")
	}
	if _, ok := dbmap.Global.Load(key); ok {
		t.Fatal("失效引擎未从注册表清除")
	}
	if c.engine.Load() != nil {
		t.Fatal("未命中时不应填充引擎")
	}
}

// TestClosedClientReturnsErrorInsteadOfPanic 关闭后所有读取引擎的入口都要返回错误。
// 修复前 Stats/Raw/BeginTx 在 Close 置空 engine 后直接 nil 解引用。
func TestClosedClientReturnsErrorInsteadOfPanic(t *testing.T) {
	c := &Client{cfg: probeCfg()}
	c.engine.Store(newFakeEngine(t))
	if err := c.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("重复关闭应为 no-op: %v", err)
	}

	if _, err := c.Stats(); err == nil {
		t.Fatal("关闭后 Stats 应返回错误")
	}
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("关闭后 Ping 应返回错误")
	}
	if _, err := c.BeginTx(context.Background()); err == nil {
		t.Fatal("关闭后 BeginTx 应返回错误")
	}
	called := false
	if err := c.Raw(context.Background(), func(*gorm.DB) error { called = true; return nil }); err == nil {
		t.Fatal("关闭后 Raw 应返回错误")
	}
	if called {
		t.Fatal("关闭后 Raw 不应执行回调")
	}
	if c.Engine() != nil {
		t.Fatal("关闭后 Engine 应为 nil")
	}
}

// TestRepositoryRejectsAfterClose 仓储层同样不得在空引擎上链式查询。
func TestRepositoryRejectsAfterClose(t *testing.T) {
	c := &Client{cfg: probeCfg()}
	c.engine.Store(newFakeEngine(t))
	repo := NewRepository[engineProbeRow](c)
	if err := c.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	row := &engineProbeRow{}
	if err := repo.Create(context.Background(), row); err == nil {
		t.Fatal("关闭后 Create 应返回错误")
	}
	if _, err := repo.FindByID(context.Background(), 1); err == nil {
		t.Fatal("关闭后 FindByID 应返回错误")
	}
	if err := repo.AutoMigrate(context.Background()); err == nil {
		t.Fatal("关闭后 AutoMigrate 应返回错误")
	}
}

// TestCloseKeepsNewerRegistryEntry Close 只能删掉自己那条注册表项。
// 无条件 Delete 会把同配置新建立的连接踢出注册表，导致后续客户端重复拨号。
func TestCloseKeepsNewerRegistryEntry(t *testing.T) {
	c := &Client{cfg: probeCfg()}
	older := newFakeEngine(t)
	c.engine.Store(older)
	key := c.poolKey()
	dbmap.Global.Delete(key)
	defer dbmap.Global.Delete(key)
	dbmap.Global.Store(key, older)

	newer := newFakeEngine(t)
	dbmap.Global.Store(key, newer)

	if err := c.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	v, ok := dbmap.Global.Load(key)
	if !ok {
		t.Fatal("注册表项被误删，后续同配置客户端会重复拨号")
	}
	if v != any(newer) {
		t.Fatal("注册表应保留新连接")
	}
}

// TestConcurrentEngineReadsDuringClose 读侧与 Close 并发不得 panic，
// 关闭后的读取一律转成错误。
func TestConcurrentEngineReadsDuringClose(t *testing.T) {
	c := &Client{cfg: probeCfg()}
	c.engine.Store(newFakeEngine(t))

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if err := c.Ping(context.Background()); err != nil && !errors.Is(err, context.Canceled) {
					// 关闭后返回错误即为预期，不中断轮询
					_ = err
				}
				if _, err := c.Stats(); err != nil {
					continue
				}
				if c.Engine() == nil {
					continue
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
		_ = c.Close()
	}()
	wg.Wait()

	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("关闭后 Ping 应返回错误")
	}
}
