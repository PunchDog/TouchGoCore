package dbcache

// ============================================================================
// P2-P5 验收缺口补齐：
//  1. 资金并发回归四件套：余额审计链（先红后绿）、并发压力、奖池守卫 decrement
//     精确下限、回执幂等单赢家、CAS 状态单赢家。
//  2. 行锁边界：等待超时 ErrTxConflict、TTL 过期提交中止、嵌套事务实际语义。
//  3. 命名拆 MySQL 演练：四接口 + 事务 + Spec 全形状在无 source/sink 下全通。
//  4. TestSpecParityVsMySQL：Spec 引擎与真 MySQL 同数据双跑对拍（env 闸）。
//
// 确定性交错用例走 fakeStore（单 goroutine 编排，不碰运气）；并发压力与原子
// 竞争用例走真 Redis（TASKENGINE_TEST_REDIS_* env 闸，缺省 Skip），因为被测
// 机制正是 Lua 脚本的原子性，内存 fake 的互斥锁会掩盖脚本缺陷。
// ============================================================================

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"touchgocore/config"
	mysqldb "touchgocore/db/mysql"
)

// gapMember 资金余额模型：point 是被守护的余额列，state 供 CAS 单赢家用。
type gapMember struct {
	ID    int64  `cache:"id,pk"`
	Point int64  `cache:"point"`
	State string `cache:"state"`
}

// gapLog 余额审计链：每次余额变动追加一行 balance=变动后余额，链上不得重复。
type gapLog struct {
	ID      int64 `cache:"id,pk"`
	Member  int64 `cache:"member"`
	Balance int64 `cache:"balance"`
}

// gapReceipt 幂等回执：op_id 唯一守卫，并发插入只允许一个赢家。
type gapReceipt struct {
	ID   int64  `cache:"id,pk"`
	OpID string `cache:"op_id"`
}

// gapCond 单列行键条件。
type gapCond struct {
	ID *int64 `cache:"id"`
}

// gapParityUser Spec 对拍用户表（与 MySQL 侧 DDL 同构）。
type gapParityUser struct {
	ID   int64  `cache:"id,pk"`
	Name string `cache:"name"`
	Age  int64  `cache:"age"`
}

// gapGroupRow 分组聚合投影目标。
type gapGroupRow struct {
	Member int64 `gorm:"column:member"`
	Total  int64 `gorm:"column:total"`
}

// gapJoinRow JOIN 投影目标。
type gapJoinRow struct {
	ID    int64 `gorm:"column:id"`
	Point int64 `gorm:"column:point"`
}

// 本文件独占的键前缀与表名，避免与既有用例互相污染。
const (
	gapFakeKeyPrefix  = "gapcase20261111"
	gapRedisKeyPrefix = "gapredis20261111"
	gapParityPrefix   = "gapparity20261111"
	gapMemberTable    = "gap_member"
	gapLogTable       = "gap_log"
	gapReceiptTable   = "gap_receipt"
)

// MySQL 对拍 env 闸：值只在进程内存中，绝不写入仓库任何文件。
const (
	gapEnvMySQLHost     = "TASKENGINE_TEST_MYSQL_HOST"
	gapEnvMySQLUser     = "TASKENGINE_TEST_MYSQL_USER"
	gapEnvMySQLPassword = "TASKENGINE_TEST_MYSQL_PASSWORD"
	gapEnvMySQLDB       = "TASKENGINE_TEST_MYSQL_DB"
)

// gapLogSeq 并发压力用例的审计行取号（显式 ID，不依赖自增）。
var gapLogSeq atomic.Int64

// bindGap 在给定引擎上纯 Redis 绑定一张表（无 source/sink）。
func bindGap[T any](t *testing.T, e *Engine, table string, opt BindOption[T]) *binding {
	t.Helper()
	keys, err := makeTableKeys(e.cfg.KeyPrefix, table, false)
	if err != nil {
		t.Fatalf("makeTableKeys %s: %v", table, err)
	}
	b, err := newBinding[T](table, backendMySQL, opt, keys, e.cfg)
	if err != nil {
		t.Fatalf("newBinding %s: %v", table, err)
	}
	if err := e.register(b); err != nil {
		t.Fatalf("register %s: %v", table, err)
	}
	return b
}

// gapBindAll 绑定资金三表：余额 / 审计链（自增）/ 回执（op_id 唯一）。
func gapBindAll(t *testing.T, e *Engine) {
	t.Helper()
	bindGap[gapMember](t, e, gapMemberTable, BindOption[gapMember]{})
	bindGap[gapLog](t, e, gapLogTable, BindOption[gapLog]{AutoIncrement: "id"})
	bindGap[gapReceipt](t, e, gapReceiptTable, BindOption[gapReceipt]{UniqueColumns: [][]string{{"op_id"}}})
}

// gapEngine 装配 fakeStore 夹具并安装为 defaultEngine；locks 控制行锁开关。
// 等待钳到 200ms：竞争用例快速失败，不拖慢套件。
func gapEngine(t *testing.T, locks bool) (*Engine, *fakeStore) {
	t.Helper()
	fs := newFakeStore()
	cfg := DefaultConfig()
	cfg.KeyPrefix = gapFakeKeyPrefix
	cfg.RequireAOF = false
	cfg.TxRowLocks = locks
	cfg.TxLockWaitMS = 200
	e := newEngine(cfg, fs)
	gapBindAll(t, e)
	old := defaultEngine.Swap(e)
	t.Cleanup(func() { defaultEngine.Store(old) })
	return e, fs
}

// gapRedis 装配真 Redis 夹具（env 闸缺省 Skip）；用例前后清理独占前缀键。
func gapRedis(t *testing.T, locks bool) (*redis.Client, *Engine) {
	t.Helper()
	addr := os.Getenv(txEnvRedisAddr)
	if addr == "" {
		t.Skipf("跳过：未设 %s（真 Redis 并发回归）", txEnvRedisAddr)
	}
	dbNum := 0
	if raw := os.Getenv(txEnvRedisDB); raw != "" {
		dbNum, _ = strconv.Atoi(raw)
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr, Password: os.Getenv(txEnvRedisPassword), DB: dbNum})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		t.Skipf("跳过：Redis 不可用: %v", err)
	}
	cleanup := func() {
		keys, _ := rdb.Keys(ctx, "{"+gapRedisKeyPrefix+":*").Result()
		if len(keys) > 0 {
			rdb.Del(ctx, keys...)
		}
	}
	cleanup()
	cfg := DefaultConfig()
	cfg.KeyPrefix = gapRedisKeyPrefix
	cfg.RequireAOF = false
	cfg.TxRowLocks = locks
	cfg.TxLockWaitMS = 500
	e := newEngine(cfg, newRedisStore(rdb))
	gapBindAll(t, e)
	old := defaultEngine.Swap(e)
	t.Cleanup(func() {
		defaultEngine.Store(old)
		cleanup()
		rdb.Close()
	})
	return rdb, e
}

// seedGapMember 非事务 Save 写入余额行（Redis 权威真源）。
func seedGapMember(t *testing.T, row *gapMember) {
	t.Helper()
	if err := Save[gapMember](context.Background(), gapMemberTable, "", row, false); err != nil {
		t.Fatalf("seed member %+v: %v", row, err)
	}
}

// readGapMember 点查余额行。
func readGapMember(t *testing.T, id int64) gapMember {
	t.Helper()
	var out gapMember
	if err := Query[gapMember](context.Background(), gapMemberTable, &gapCond{ID: &id}, &out); err != nil {
		t.Fatalf("read member %d: %v", id, err)
	}
	return out
}

// gapLogList 按 id 升序取全部审计行（spec 排序，键集扫描）。
func gapLogList(t *testing.T) []gapLog {
	t.Helper()
	var out []gapLog
	o := &Options{Orders: []Order{{Column: "id"}}}
	if err := Query[gapLog](context.Background(), gapLogTable, nil, &out, func(oo *Options) { *oo = *o }); err != nil {
		t.Fatalf("list logs: %v", err)
	}
	return out
}

// gapForUpdate 与 db.ForUpdate 等价：事务内点查取行锁（SELECT FOR UPDATE）。
func gapForUpdate(o *Options) { o.ForUpdate = true }

// TestFundBalanceChainRowLockRedGreen 资金 F1 审计链先红后绿：
// 红（关行锁）：A/B 都读到旧余额 0，各自写回绝对值 1 → 终值 1（丢更新），
// 审计链出现重复余额 [1,1]；绿（开行锁）：B 读时即被 A 的锁拒绝，A 提交后
// B 重试读到 1 → 终值 2，审计链 [1,2] 无重复。全程单 goroutine 确定性编排。
func TestFundBalanceChainRowLockRedGreen(t *testing.T) {
	ctx := context.Background()
	id := int64(1)
	cond := &gapCond{ID: &id}

	t.Run("red_locks_off_lost_update", func(t *testing.T) {
		_, _ = gapEngine(t, false)
		seedGapMember(t, &gapMember{ID: 1, Point: 0})

		txA, err := BeginTx(ctx)
		if err != nil {
			t.Fatalf("BeginTx A: %v", err)
		}
		var mA gapMember
		if err := TxLockOne[gapMember](txA, txA.Context(), gapMemberTable, cond, &mA); err != nil {
			t.Fatalf("A lock read: %v", err)
		}
		txB, err := BeginTx(ctx)
		if err != nil {
			t.Fatalf("BeginTx B: %v", err)
		}
		var mB gapMember
		// 关锁时 B 畅通无阻读到同一旧值——这正是缺陷所在。
		if err := TxLockOne[gapMember](txB, txB.Context(), gapMemberTable, cond, &mB); err != nil {
			t.Fatalf("B lock read: %v", err)
		}
		if mA.Point != 0 || mB.Point != 0 {
			t.Fatalf("A/B 应同读旧值 0，实际 %d/%d", mA.Point, mB.Point)
		}
		// B 先提交：余额 1 + 审计行 balance=1。
		if err := TxSave[gapMember](txB, txB.Context(), gapMemberTable, &gapMember{ID: 1, Point: mB.Point + 1}); err != nil {
			t.Fatalf("B save: %v", err)
		}
		if err := TxSave[gapLog](txB, txB.Context(), gapLogTable, &gapLog{ID: 100, Member: 1, Balance: mB.Point + 1}); err != nil {
			t.Fatalf("B log: %v", err)
		}
		if err := txB.Commit(); err != nil {
			t.Fatalf("B commit: %v", err)
		}
		// A 用陈旧读值写回绝对值——覆盖 B 的提交。
		if err := TxSave[gapMember](txA, txA.Context(), gapMemberTable, &gapMember{ID: 1, Point: mA.Point + 1}); err != nil {
			t.Fatalf("A save: %v", err)
		}
		if err := TxSave[gapLog](txA, txA.Context(), gapLogTable, &gapLog{ID: 200, Member: 1, Balance: mA.Point + 1}); err != nil {
			t.Fatalf("A log: %v", err)
		}
		if err := txA.Commit(); err != nil {
			t.Fatalf("A commit: %v", err)
		}

		// 红断言：两次 +1 只留下终值 1，审计链重复 [1,1]。
		if final := readGapMember(t, 1); final.Point != 1 {
			t.Fatalf("关锁丢更新终值应为 1，实际 %d", final.Point)
		}
		logs := gapLogList(t)
		if len(logs) != 2 || logs[0].Balance != 1 || logs[1].Balance != 1 {
			t.Fatalf("关锁审计链应重复 [1,1]，实际 %+v", logs)
		}
	})

	t.Run("green_locks_on_serialized", func(t *testing.T) {
		_, _ = gapEngine(t, true)
		seedGapMember(t, &gapMember{ID: 1, Point: 0})

		txA, err := BeginTx(ctx)
		if err != nil {
			t.Fatalf("BeginTx A: %v", err)
		}
		var mA gapMember
		if err := TxLockOne[gapMember](txA, txA.Context(), gapMemberTable, cond, &mA); err != nil {
			t.Fatalf("A lock read: %v", err)
		}
		txB, err := BeginTx(ctx)
		if err != nil {
			t.Fatalf("BeginTx B: %v", err)
		}
		var mB gapMember
		// 开锁后 B 在读取时即被拒（等待超时），而不是提交时才发现。
		err = TxLockOne[gapMember](txB, txB.Context(), gapMemberTable, cond, &mB)
		if !errors.Is(err, ErrTxConflict) {
			t.Fatalf("B 应 ErrTxConflict，实际 %v", err)
		}
		_ = txB.Rollback()
		// A 独占完成：余额 1 + 审计行 1。
		if err := TxSave[gapMember](txA, txA.Context(), gapMemberTable, &gapMember{ID: 1, Point: mA.Point + 1}); err != nil {
			t.Fatalf("A save: %v", err)
		}
		if err := TxSave[gapLog](txA, txA.Context(), gapLogTable, &gapLog{ID: 100, Member: 1, Balance: mA.Point + 1}); err != nil {
			t.Fatalf("A log: %v", err)
		}
		if err := txA.Commit(); err != nil {
			t.Fatalf("A commit: %v", err)
		}
		// B 重试：读到 A 提交后的 1，串行追加。
		txB2, err := BeginTx(ctx)
		if err != nil {
			t.Fatalf("BeginTx B2: %v", err)
		}
		var mB2 gapMember
		if err := TxLockOne[gapMember](txB2, txB2.Context(), gapMemberTable, cond, &mB2); err != nil {
			t.Fatalf("B2 lock read: %v", err)
		}
		if mB2.Point != 1 {
			t.Fatalf("B2 应读到 A 提交后的 1，实际 %d", mB2.Point)
		}
		if err := TxSave[gapMember](txB2, txB2.Context(), gapMemberTable, &gapMember{ID: 1, Point: mB2.Point + 1}); err != nil {
			t.Fatalf("B2 save: %v", err)
		}
		if err := TxSave[gapLog](txB2, txB2.Context(), gapLogTable, &gapLog{ID: 200, Member: 1, Balance: mB2.Point + 1}); err != nil {
			t.Fatalf("B2 log: %v", err)
		}
		if err := txB2.Commit(); err != nil {
			t.Fatalf("B2 commit: %v", err)
		}

		if final := readGapMember(t, 1); final.Point != 2 {
			t.Fatalf("串行化终值应为 2，实际 %d", final.Point)
		}
		logs := gapLogList(t)
		if len(logs) != 2 || logs[0].Balance != 1 || logs[1].Balance != 2 {
			t.Fatalf("审计链应为 [1,2]，实际 %+v", logs)
		}
	})
}

// TestFundBalanceChainConcurrentStress 真 Redis 16 并发读改写 + 审计链：
// 行锁串行化后终值必须恰为 16，审计链是 1..16 的一个排列（无重复无空洞）。
func TestFundBalanceChainConcurrentStress(t *testing.T) {
	gapRedis(t, true)
	ctx := context.Background()
	seedGapMember(t, &gapMember{ID: 1, Point: 0})
	id := int64(1)
	cond := &gapCond{ID: &id}

	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for attempt := 0; attempt < 200; attempt++ {
				err := RunTx(ctx, func(tctx context.Context) error {
					var m gapMember
					if err := Query[gapMember](tctx, gapMemberTable, cond, &m, gapForUpdate); err != nil {
						return err
					}
					if err := Save[gapMember](tctx, gapMemberTable, "", &gapMember{ID: 1, Point: m.Point + 1}, false); err != nil {
						return err
					}
					_, err := Insert[gapLog](tctx, gapLogTable, &gapLog{ID: gapLogSeq.Add(1), Member: 1, Balance: m.Point + 1})
					return err
				})
				if err == nil {
					return
				}
				if !errors.Is(err, ErrTxConflict) {
					t.Errorf("并发事务非冲突错误: %v", err)
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
			t.Errorf("并发事务 200 次重试仍未提交")
		}()
	}
	wg.Wait()

	if final := readGapMember(t, 1); final.Point != n {
		t.Fatalf("16 并发 +1 终值应为 %d，实际 %d", n, final.Point)
	}
	logs := gapLogList(t)
	if len(logs) != n {
		t.Fatalf("审计行应为 %d 条，实际 %d", n, len(logs))
	}
	balances := make([]int64, 0, n)
	for _, l := range logs {
		balances = append(balances, l.Balance)
	}
	sort.Slice(balances, func(i, j int) bool { return balances[i] < balances[j] })
	for i, v := range balances {
		if v != int64(i+1) {
			t.Fatalf("审计链应为 1..%d 排列，实际 %v", n, balances)
		}
	}
}

// TestFundPoolConcurrentDecrementGuardedExactFloor 真 Redis 奖池守卫 decrement：
// 初始 100，30 并发各扣 10（条件 point>=10 在 Lua 内原子校验），恰好 10 次
// 成功、终值精确 0，绝不穿底。
func TestFundPoolConcurrentDecrementGuardedExactFloor(t *testing.T) {
	gapRedis(t, true)
	ctx := context.Background()
	seedGapMember(t, &gapMember{ID: 1, Point: 100})
	id := int64(1)
	cond := &gapCond{ID: &id}

	const n = 30
	var success atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for attempt := 0; attempt < 200; attempt++ {
				var affected int64
				err := RunTx(ctx, func(tctx context.Context) error {
					return Save[gapMember](tctx, gapMemberTable, "", nil, false, func(o *Options) {
						o.Increment = "point"
						o.Delta = -10
						o.Cond = cond
						o.Conditions = []Range{{Column: "point", Op: ">=", Value: int64(10)}}
						o.Affected = &affected
					})
				})
				if err == nil {
					if affected == 1 {
						success.Add(1)
					}
					return
				}
				if !errors.Is(err, ErrTxConflict) {
					t.Errorf("守卫 decrement 非冲突错误: %v", err)
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
			t.Errorf("守卫 decrement 200 次重试仍未完成")
		}()
	}
	wg.Wait()

	if got := success.Load(); got != 10 {
		t.Fatalf("100/10 应恰好 10 次成功，实际 %d", got)
	}
	if final := readGapMember(t, 1); final.Point != 0 {
		t.Fatalf("奖池终值应精确 0（不穿底），实际 %d", final.Point)
	}
}

// TestReceiptConcurrentInsertSingleWinner 真 Redis 幂等回执：20 并发事务插入
// 同一 op_id（不同行键），唯一守卫在提交重放时原子复核——安全不变量是「存活
// 恰 1 行」。注意：事务内 Insert 返回的 affected 是 overlay 阶段值（加载时守卫
// 为空即 1），输家提交时静默 affected=0 但 fn 内拿到的仍是 1；判定赢家必须提交
// 后按 op_id 回读比对（见 TestTxInsertAffectedIsOverlayTime，契约缺口已上报）。
func TestReceiptConcurrentInsertSingleWinner(t *testing.T) {
	gapRedis(t, true)
	ctx := context.Background()

	const n = 20
	var winners atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for attempt := 0; attempt < 200; attempt++ {
				var affected int64
				err := RunTx(ctx, func(tctx context.Context) error {
					var n int64
					var ierr error
					n, ierr = Insert[gapReceipt](tctx, gapReceiptTable, &gapReceipt{ID: int64(idx + 1), OpID: "op-1"})
					affected = n
					return ierr
				})
				if err == nil {
					if affected == 1 {
						winners.Add(1)
					}
					return
				}
				if !errors.Is(err, ErrTxConflict) {
					t.Errorf("回执插入非冲突错误: %v", err)
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
			t.Errorf("回执插入 200 次重试仍未完成")
		}(i)
	}
	wg.Wait()

	var receipts []gapReceipt
	o := &Options{Orders: []Order{{Column: "id"}}}
	if err := Query[gapReceipt](ctx, gapReceiptTable, nil, &receipts, func(oo *Options) { *oo = *o }); err != nil {
		t.Fatalf("list receipts: %v", err)
	}
	if len(receipts) != 1 || receipts[0].OpID != "op-1" {
		t.Fatalf("存活回执应恰 1 行 op-1，实际 %d 行 %+v（winners=%d）", len(receipts), receipts, winners.Load())
	}
	if winners.Load() < 1 {
		t.Fatalf("至少应有 1 个 overlay 阶段赢家，实际 %d", winners.Load())
	}
}

// TestTxInsertAffectedIsOverlayTime 钉死当前契约：事务内 Insert 的 affected 是
// overlay 阶段值，不是提交重放终值。两个事务同插一个唯一键，各自 overlay 都
// affected=1；后提交者实际落库 affected=0 但 Commit 不报错、fn 内返回值不变。
// 资金/幂等场景判定赢家必须提交后按唯一键回读。此语义与「提交终值回填 affected」
// 的期望存在缺口，已上报用户拍板；本用例防止语义被悄悄改动。
func TestTxInsertAffectedIsOverlayTime(t *testing.T) {
	_, _ = gapEngine(t, true)
	ctx := context.Background()

	txA, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx A: %v", err)
	}
	nA, err := Insert[gapReceipt](txA.Context(), gapReceiptTable, &gapReceipt{ID: 1, OpID: "op-x"})
	if err != nil || nA != 1 {
		t.Fatalf("A overlay 插入应 affected=1，实际 n=%d err=%v", nA, err)
	}
	txB, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx B: %v", err)
	}
	// B 的 overlay 加载时守卫仍空 → 同样 affected=1（尽管最终会输）。
	nB, err := Insert[gapReceipt](txB.Context(), gapReceiptTable, &gapReceipt{ID: 2, OpID: "op-x"})
	if err != nil || nB != 1 {
		t.Fatalf("B overlay 插入应 affected=1，实际 n=%d err=%v", nB, err)
	}
	if err := txA.Commit(); err != nil {
		t.Fatalf("A commit: %v", err)
	}
	// 后提交者：守卫重查生效，行不落库，但 Commit 静默成功。
	if err := txB.Commit(); err != nil {
		t.Fatalf("B commit 应静默成功（affected=0 不报错），实际 %v", err)
	}
	var receipts []gapReceipt
	o := &Options{Orders: []Order{{Column: "id"}}}
	if err := Query[gapReceipt](ctx, gapReceiptTable, nil, &receipts, func(oo *Options) { *oo = *o }); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(receipts) != 1 || receipts[0].ID != 1 {
		t.Fatalf("存活行应恰 1 行且属先提交者 ID=1，实际 %+v", receipts)
	}
}

// TestCasStatusSingleWinner 真 Redis CAS 状态机：10 并发 open→done，
// Lua 内期望值校验保证恰 1 次 affected=1。
func TestCasStatusSingleWinner(t *testing.T) {
	gapRedis(t, true)
	ctx := context.Background()
	seedGapMember(t, &gapMember{ID: 2, State: "open"})
	id := int64(2)
	cond := &gapCond{ID: &id}

	const n = 10
	var winners atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			affected, err := CompareAndSwap(ctx, gapMemberTable, cond,
				map[string]any{"state": "open"}, map[string]any{"state": "done"})
			if err != nil {
				t.Errorf("CAS 错误: %v", err)
				return
			}
			if affected == 1 {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := winners.Load(); got != 1 {
		t.Fatalf("CAS open→done 应恰 1 赢家，实际 %d", got)
	}
	if m := readGapMember(t, 2); m.State != "done" {
		t.Fatalf("终态应为 done，实际 %q", m.State)
	}
}

// TestTxLockWaitTimeoutConflict 行锁等待边界：B 必须等满等待预算才报
// ErrTxConflict（不是立即失败）；A 回滚释放锁后 B 重试立即成功。
func TestTxLockWaitTimeoutConflict(t *testing.T) {
	_, _ = gapEngine(t, true)
	ctx := context.Background()
	id := int64(1)
	cond := &gapCond{ID: &id}
	seedGapMember(t, &gapMember{ID: 1, Point: 7})

	txA, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx A: %v", err)
	}
	var mA gapMember
	if err := TxLockOne[gapMember](txA, txA.Context(), gapMemberTable, cond, &mA); err != nil {
		t.Fatalf("A lock read: %v", err)
	}

	txB, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx B: %v", err)
	}
	start := time.Now()
	var mB gapMember
	err = TxLockOne[gapMember](txB, txB.Context(), gapMemberTable, cond, &mB)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTxConflict) {
		t.Fatalf("B 应 ErrTxConflict，实际 %v", err)
	}
	if elapsed < 150*time.Millisecond {
		t.Fatalf("B 应等满 ~200ms 预算，实际 %v", elapsed)
	}
	_ = txB.Rollback()
	// A 回滚释放锁 → B 重试无需等待。
	if err := txA.Rollback(); err != nil {
		t.Fatalf("A rollback: %v", err)
	}
	txB2, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx B2: %v", err)
	}
	var mB2 gapMember
	if err := TxLockOne[gapMember](txB2, txB2.Context(), gapMemberTable, cond, &mB2); err != nil {
		t.Fatalf("锁释放后 B2 应立即取锁成功: %v", err)
	}
	_ = txB2.Rollback()
}

// TestTxLockTTLExpiryAbortsCommit 行锁 TTL 边界：持锁事务的锁过期后，
// 提交必须整批中止（lock_lost → ErrTxConflict），Redis 不留任何部分写。
func TestTxLockTTLExpiryAbortsCommit(t *testing.T) {
	e, fs := gapEngine(t, true)
	ctx := context.Background()

	txA, err := BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := TxSave[gapMember](txA, txA.Context(), gapMemberTable, &gapMember{ID: 1, Point: 5}); err != nil {
		t.Fatalf("A save: %v", err)
	}
	b, err := e.lookupTable(gapMemberTable)
	if err != nil {
		t.Fatalf("lookupTable: %v", err)
	}
	// 模拟锁 TTL 到期：把过期时间拨到过去（生产中等价于 PX 自然到期）。
	lockKey := b.keys.lockPrefix + "1"
	fs.mu.Lock()
	fs.m.expiry[lockKey] = time.Now().UnixMilli() - 1000
	fs.mu.Unlock()

	err = txA.Commit()
	if !errors.Is(err, ErrTxConflict) {
		t.Fatalf("锁过期提交应 ErrTxConflict，实际 %v", err)
	}
	if h := fs.hash(b.keys.rowPrefix + "1"); len(h) != 0 {
		t.Fatalf("中止提交不得留下部分写，实际 %v", h)
	}
}

// TestNestedTxIndependentCommit 记录嵌套 RunTx 的实际语义：内层无条件新建
// txState 并独立提交（不并入外层批）。外层回滚只丢弃自己的 overlay，内层
// 已提交的行不受影响。注意：这与重构计划「嵌套并入外层批」存在偏差，已向
// 用户标注待拍板；本用例按磁盘上的实际行为断言，防止语义被悄悄改动。
func TestNestedTxIndependentCommit(t *testing.T) {
	ctx := context.Background()

	t.Run("inner_commits_independently", func(t *testing.T) {
		_, _ = gapEngine(t, true)
		one, two := int64(1), int64(2)
		err := RunTx(ctx, func(outer context.Context) error {
			if err := Save[gapMember](outer, gapMemberTable, "", &gapMember{ID: 1, Point: 10}, false); err != nil {
				return err
			}
			innerErr := RunTx(outer, func(inner context.Context) error {
				return Save[gapMember](inner, gapMemberTable, "", &gapMember{ID: 2, Point: 20}, false)
			})
			if innerErr != nil {
				return innerErr
			}
			// 内层已独立提交：事务外点查立即可见 member2。
			var m gapMember
			if err := Query[gapMember](ctx, gapMemberTable, &gapCond{ID: &two}, &m); err != nil {
				return fmt.Errorf("内层提交后应立即可见: %w", err)
			}
			return errors.New("outer abort")
		})
		if err == nil || err.Error() != "outer abort" {
			t.Fatalf("外层应以 outer abort 回滚，实际 %v", err)
		}
		// 外层回滚：member1 不存在；内层独立提交：member2 存活。
		var m1 gapMember
		if err := Query[gapMember](ctx, gapMemberTable, &gapCond{ID: &one}, &m1); !errors.Is(err, ErrNoRows) {
			t.Fatalf("外层回滚后 member1 应 ErrNoRows，实际 %v", err)
		}
		if m2 := readGapMember(t, 2); m2.Point != 20 {
			t.Fatalf("内层独立提交 member2 应存活 point=20，实际 %+v", m2)
		}
	})

	t.Run("same_row_self_conflict", func(t *testing.T) {
		_, _ = gapEngine(t, true)
		start := time.Now()
		err := RunTx(ctx, func(outer context.Context) error {
			if err := Save[gapMember](outer, gapMemberTable, "", &gapMember{ID: 1, Point: 10}, false); err != nil {
				return err
			}
			// 内层触碰外层持锁的同一行 → 等满预算后冲突（自锁）。
			return RunTx(outer, func(inner context.Context) error {
				return Save[gapMember](inner, gapMemberTable, "", &gapMember{ID: 1, Point: 99}, false)
			})
		})
		if !errors.Is(err, ErrTxConflict) {
			t.Fatalf("嵌套同行应 ErrTxConflict，实际 %v", err)
		}
		if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
			t.Fatalf("内层应等满锁预算，实际 %v", elapsed)
		}
		var m gapMember
		id := int64(1)
		if err := Query[gapMember](ctx, gapMemberTable, &gapCond{ID: &id}, &m); !errors.Is(err, ErrNoRows) {
			t.Fatalf("双层回滚后行应不存在，实际 %v", err)
		}
	})
}

// TestNoMySQLFourInterfacesAndTx 命名拆 MySQL 演练：绑定不挂任何 source/sink，
// Insert（自增取号）/ Query（点查+列表）/ Save / Increment / CAS / Delete /
// RunTx / Flush 全链路必须仅凭 Redis 完成，Flush 无 sink 时静默 no-op。
func TestNoMySQLFourInterfacesAndTx(t *testing.T) {
	_, fs := gapEngine(t, true)
	ctx := context.Background()
	id := int64(1)
	cond := &gapCond{ID: &id}

	// Insert：自增列零值 → Redis INCR 即时取号回填。
	log := &gapLog{Member: 1, Balance: 5}
	n, err := Insert[gapLog](ctx, gapLogTable, log)
	if err != nil || n != 1 {
		t.Fatalf("Insert 应 affected=1，实际 n=%d err=%v", n, err)
	}
	if log.ID <= 0 {
		t.Fatalf("自增 ID 应即时回填 >0，实际 %d", log.ID)
	}

	// Save + 点查。
	seedGapMember(t, &gapMember{ID: 1, Point: 100, State: "open"})
	if m := readGapMember(t, 1); m.Point != 100 {
		t.Fatalf("点查应为 100，实际 %d", m.Point)
	}
	// 列表（键集扫描 + spec 排序）。
	var members []gapMember
	o := &Options{Orders: []Order{{Column: "id"}}}
	if err := Query[gapMember](ctx, gapMemberTable, nil, &members, func(oo *Options) { *oo = *o }); err != nil {
		t.Fatalf("列表查询: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("列表应 1 行，实际 %d", len(members))
	}

	// 事务外原子累加 / CAS（Lua 单脚本）。
	if n, err := Increment(ctx, gapMemberTable, cond, "point", 5); err != nil || n != 1 {
		t.Fatalf("Increment 应 affected=1，实际 n=%d err=%v", n, err)
	}
	if n, err := CompareAndSwap(ctx, gapMemberTable, cond,
		map[string]any{"state": "open"}, map[string]any{"state": "done"}); err != nil || n != 1 {
		t.Fatalf("CAS 应 affected=1，实际 n=%d err=%v", n, err)
	}
	if m := readGapMember(t, 1); m.Point != 105 || m.State != "done" {
		t.Fatalf("原子写后应 105/done，实际 %+v", m)
	}

	// RunTx 全链路：锁读 → 写 → 审计插入 → 提交。
	err = RunTx(ctx, func(tctx context.Context) error {
		var m gapMember
		if err := Query[gapMember](tctx, gapMemberTable, cond, &m, gapForUpdate); err != nil {
			return err
		}
		if err := Save[gapMember](tctx, gapMemberTable, "", &gapMember{ID: 1, Point: m.Point + 1, State: m.State}, false); err != nil {
			return err
		}
		_, err := Insert[gapLog](tctx, gapLogTable, &gapLog{Member: 1, Balance: m.Point + 1})
		return err
	})
	if err != nil {
		t.Fatalf("RunTx: %v", err)
	}
	if m := readGapMember(t, 1); m.Point != 106 {
		t.Fatalf("事务后应 106，实际 %d", m.Point)
	}

	// Delete：立墓碑，点查 ErrNoRows。
	if err := Delete(ctx, gapMemberTable, cond); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var gone gapMember
	if err := Query[gapMember](ctx, gapMemberTable, cond, &gone); !errors.Is(err, ErrNoRows) {
		t.Fatalf("删除后点查应 ErrNoRows，实际 %v", err)
	}

	// Flush 无 sink：静默 no-op，不报错（MySQL 完全缺席）。
	if err := Flush(ctx, gapMemberTable); err != nil {
		t.Fatalf("无 sink Flush 应 no-op，实际 %v", err)
	}
	b, _ := Default().lookupTable(gapMemberTable)
	if _, ok := fs.m.sets[b.keys.keySet]["1"]; ok {
		t.Fatal("删除后键集不应再含 1")
	}
}

// TestSpecFullShapeWithoutMySQL Spec 全形状拆 MySQL 演练：范围+排序+分页、
// 分组聚合、LEFT JOIN + COALESCE 全部仅凭 Redis 键集与 Go 求值完成。
func TestSpecFullShapeWithoutMySQL(t *testing.T) {
	_, _ = gapEngine(t, true)
	ctx := context.Background()
	seedGapMember(t, &gapMember{ID: 1, Point: 10})
	seedGapMember(t, &gapMember{ID: 2, Point: 20})
	seedGapMember(t, &gapMember{ID: 3, Point: 30})
	for _, l := range []*gapLog{{ID: 1, Member: 1, Balance: 100}, {ID: 2, Member: 1, Balance: 50}, {ID: 3, Member: 2, Balance: 70}} {
		if err := Save[gapLog](ctx, gapLogTable, "", l, false); err != nil {
			t.Fatalf("seed log %+v: %v", l, err)
		}
	}

	// 范围 + 降序 + 分页：point>=20 降序 [30,20]，Offset1 Limit1 → member2。
	var page []gapMember
	err := Query[gapMember](ctx, gapMemberTable, nil, &page, func(o *Options) {
		o.Ranges = []Range{{Column: "point", Op: ">=", Value: int64(20)}}
		o.Orders = []Order{{Column: "point", Desc: true}}
		o.Limit, o.Offset = 1, 1
	})
	if err != nil {
		t.Fatalf("范围分页: %v", err)
	}
	if len(page) != 1 || page[0].ID != 2 {
		t.Fatalf("分页应命中 member2，实际 %+v", page)
	}

	// 分组聚合：GROUP BY member + SUM(balance)。
	var groups []gapGroupRow
	err = Query[gapLog](ctx, gapLogTable, nil, &groups, func(o *Options) {
		o.AllowAnyDest = true
		o.Groups = []string{"member"}
		o.Selects = []SelectItem{
			{Expr: Col("member"), Alias: "member"},
			{Expr: SumExpr(Col("balance")), Alias: "total"},
		}
		o.Orders = []Order{{Column: "member"}}
	})
	if err != nil {
		t.Fatalf("分组聚合: %v", err)
	}
	want := []gapGroupRow{{Member: 1, Total: 150}, {Member: 2, Total: 70}}
	if !reflect.DeepEqual(groups, want) {
		t.Fatalf("分组聚合应为 %+v，实际 %+v", want, groups)
	}

	// LEFT JOIN + COALESCE：审计行挂余额表，无匹配补 0。
	var joined []gapJoinRow
	err = Query[gapLog](ctx, gapLogTable, nil, &joined, func(o *Options) {
		o.AllowAnyDest = true
		o.Joins = []Join{{Type: "LEFT", Table: gapMemberTable, Alias: "m", LeftColumn: "member", RightColumn: "id"}}
		o.Selects = []SelectItem{
			{Expr: Col("id"), Alias: "id"},
			{Expr: Coalesce(TCol("m", "point"), Lit(int64(0))), Alias: "point"},
		}
		o.Orders = []Order{{Column: "id"}}
	})
	if err != nil {
		t.Fatalf("JOIN: %v", err)
	}
	wantJoin := []gapJoinRow{{ID: 1, Point: 10}, {ID: 2, Point: 10}, {ID: 3, Point: 20}}
	if !reflect.DeepEqual(joined, wantJoin) {
		t.Fatalf("JOIN 应为 %+v，实际 %+v", wantJoin, joined)
	}
}

// TestSpecParityVsMySQL Spec 引擎与真 MySQL 同数据双跑对拍（env 闸缺省 Skip）：
// 范围/排序/分页、分组聚合、LEFT JOIN + COALESCE 三形状结果必须逐字一致。
// MySQL 库名必须含 test，临时表用例结束即 DROP，绝不触碰业务库。
func TestSpecParityVsMySQL(t *testing.T) {
	host := os.Getenv(gapEnvMySQLHost)
	if host == "" {
		t.Skipf("跳过：未设 %s（Spec 对拍真 MySQL）", gapEnvMySQLHost)
	}
	dbName := os.Getenv(gapEnvMySQLDB)
	if !strings.Contains(dbName, "test") {
		t.Fatalf("对拍库名必须含 test（安全闸），实际 %q", dbName)
	}
	mc, err := mysqldb.NewClient(&config.MySqlDBConfig{
		Host: host, Username: os.Getenv(gapEnvMySQLUser),
		Password: os.Getenv(gapEnvMySQLPassword), DBName: dbName, Loc: "UTC",
	})
	if err != nil {
		t.Skipf("跳过：MySQL 不可用: %v", err)
	}
	defer mc.Close()
	gdb := mc.Engine()

	// Redis 侧：纯 Redis 绑定 + 同数据种子。
	fs := newFakeStore()
	cfg := DefaultConfig()
	cfg.KeyPrefix = gapParityPrefix
	cfg.RequireAOF = false
	e := newEngine(cfg, fs)
	bindGap[gapParityUser](t, e, "gap_parity_user", BindOption[gapParityUser]{})
	bindGap[specWallet](t, e, "gap_parity_wallet", BindOption[specWallet]{})
	old := defaultEngine.Swap(e)
	t.Cleanup(func() { defaultEngine.Store(old) })
	ctx := context.Background()

	users := []*gapParityUser{{ID: 1, Name: "alice", Age: 30}, {ID: 2, Name: "bob", Age: 40}, {ID: 3, Name: "carol", Age: 30}, {ID: 4, Name: "dave", Age: 20}}
	wallets := []*specWallet{{ID: 10, UID: 1, Balance: 100}, {ID: 11, UID: 2, Balance: 200}, {ID: 12, UID: 99, Balance: 999}}
	for _, u := range users {
		if err := Save[gapParityUser](ctx, "gap_parity_user", "", u, false); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	for _, w := range wallets {
		if err := Save[specWallet](ctx, "gap_parity_wallet", "", w, false); err != nil {
			t.Fatalf("seed wallet: %v", err)
		}
	}

	// MySQL 侧：临时表 + 同数据。
	ddls := []string{
		"CREATE TABLE IF NOT EXISTS gap_parity_user (id BIGINT PRIMARY KEY, name VARCHAR(64), age BIGINT)",
		"CREATE TABLE IF NOT EXISTS gap_parity_wallet (id BIGINT PRIMARY KEY, uid BIGINT, balance BIGINT)",
	}
	for _, d := range ddls {
		if err := gdb.Exec(d).Error; err != nil {
			t.Fatalf("DDL: %v", err)
		}
	}
	t.Cleanup(func() {
		gdb.Exec("DROP TABLE IF EXISTS gap_parity_user")
		gdb.Exec("DROP TABLE IF EXISTS gap_parity_wallet")
	})
	gdb.Exec("TRUNCATE TABLE gap_parity_user")
	gdb.Exec("TRUNCATE TABLE gap_parity_wallet")
	for _, u := range users {
		if err := gdb.Exec("INSERT INTO gap_parity_user (id,name,age) VALUES (?,?,?)", u.ID, u.Name, u.Age).Error; err != nil {
			t.Fatalf("insert user: %v", err)
		}
	}
	for _, w := range wallets {
		if err := gdb.Exec("INSERT INTO gap_parity_wallet (id,uid,balance) VALUES (?,?,?)", w.ID, w.UID, w.Balance).Error; err != nil {
			t.Fatalf("insert wallet: %v", err)
		}
	}

	t.Run("range_order_limit", func(t *testing.T) {
		var sqlRows, redisRows []specProjRow
		if err := gdb.Raw("SELECT id,name,age FROM gap_parity_user WHERE age>=? ORDER BY age DESC, id LIMIT 2 OFFSET 1", 30).Scan(&sqlRows).Error; err != nil {
			t.Fatalf("MySQL: %v", err)
		}
		err := Query[gapParityUser](ctx, "gap_parity_user", nil, &redisRows, func(o *Options) {
			o.Ranges = []Range{{Column: "age", Op: ">=", Value: int64(30)}}
			o.Orders = []Order{{Column: "age", Desc: true}, {Column: "id"}}
			o.Limit, o.Offset = 2, 1
		})
		if err != nil {
			t.Fatalf("Spec: %v", err)
		}
		if !reflect.DeepEqual(sqlRows, redisRows) {
			t.Fatalf("范围/排序/分页不一致:\nMySQL=%+v\nSpec =%+v", sqlRows, redisRows)
		}
	})

	t.Run("group_sum_count", func(t *testing.T) {
		var sqlRows, redisRows []specGroupRow
		if err := gdb.Raw("SELECT name, SUM(age) AS total, COUNT(*) AS cnt FROM gap_parity_user GROUP BY name ORDER BY name").Scan(&sqlRows).Error; err != nil {
			t.Fatalf("MySQL: %v", err)
		}
		err := Query[gapParityUser](ctx, "gap_parity_user", nil, &redisRows, func(o *Options) {
			o.AllowAnyDest = true
			o.Groups = []string{"name"}
			o.Selects = []SelectItem{
				{Expr: Col("name"), Alias: "name"},
				{Expr: SumExpr(Col("age")), Alias: "total"},
				{Expr: CountExpr(Star(), false), Alias: "cnt"},
			}
			o.Orders = []Order{{Column: "name"}}
		})
		if err != nil {
			t.Fatalf("Spec: %v", err)
		}
		if !reflect.DeepEqual(sqlRows, redisRows) {
			t.Fatalf("分组聚合不一致:\nMySQL=%+v\nSpec =%+v", sqlRows, redisRows)
		}
	})

	t.Run("left_join_coalesce", func(t *testing.T) {
		var sqlRows, redisRows []specJoinRow
		if err := gdb.Raw("SELECT u.name, COALESCE(w.balance,0) AS balance FROM gap_parity_user u LEFT JOIN gap_parity_wallet w ON u.id=w.uid ORDER BY u.id").Scan(&sqlRows).Error; err != nil {
			t.Fatalf("MySQL: %v", err)
		}
		err := Query[gapParityUser](ctx, "gap_parity_user", nil, &redisRows, func(o *Options) {
			o.AllowAnyDest = true
			o.Joins = []Join{{Type: "LEFT", Table: "gap_parity_wallet", Alias: "w", LeftColumn: "id", RightColumn: "uid"}}
			o.Selects = []SelectItem{
				{Expr: Col("name"), Alias: "name"},
				{Expr: Coalesce(TCol("w", "balance"), Lit(int64(0))), Alias: "balance"},
			}
			o.Orders = []Order{{Column: "id"}}
		})
		if err != nil {
			t.Fatalf("Spec: %v", err)
		}
		if !reflect.DeepEqual(sqlRows, redisRows) {
			t.Fatalf("JOIN 不一致:\nMySQL=%+v\nSpec =%+v", sqlRows, redisRows)
		}
	})
}
