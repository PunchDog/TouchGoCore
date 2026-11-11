package db

// ============================================================================
// dbcache 改造验收集成测试（真 Redis + 真 MySQL，env 门控，缺省 Skip）。
//
// 分两个独立用例，必须分两次 go test -run 调用执行（进程级默认引擎是单例，
// 表绑定一次性；分进程可让「纯 Redis 绑定」与「挂 MySQL sink 绑定」互不干扰）：
//
//	TestCacheVerifyPhaseARedisOnly   MySQL 完全缺席（CacheBindRedis）：
//	    CacheInsert / CacheQuery / CacheSave(update) / CacheDelete 四接口，
//	    外加 Increment / CAS / 事务，全部只靠 Redis 完成；逐键核对行 hash、
//	    键集、脏账、删除账与墓碑。
//
//	TestCacheVerifyPhaseBMySQLSink   挂真实 MySQL（CacheBindMySQL）：
//	    mustSave=true 立即落库；mustSave=false + FlushCache 异步落库；事务落库；
//	    Redis 清空后 CacheQuery 从 MySQL 回源并把行回填进 Redis；删除落库与墓碑
//	    防复活；CloseCache final flush。
//
// 环境变量沿用仓库既有 7 个 TASKENGINE_TEST_*（凭据只由运行环境注入，绝不落盘）。
// 本文件只新增验证代码，不修改任何生产实现。
// ============================================================================

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"touchgocore/config"
)

// verifyKeyPrefix 验证专用 Redis 键前缀，避免与业务键及其他测试互相污染。
const verifyKeyPrefix = "cacheverify"

// verifyDefTable 任务定义形状的验证表。
const verifyDefTable = "cache_verify_def"

// verifyProgressTable 进度形状的验证表（三列行键 + 自增主键）。
const verifyProgressTable = "cache_verify_progress"

// verifyVersionField 行 hash 的保留版本列（与 dbcache/writeop.go 的 versionField 同值）。
const verifyVersionField = "__ver"

// 集成测试环境变量名（与 task_engine / dbcache 既有约定一致）。
const (
	// verifyEnvMySQLHost MySQL 地址 host:port。
	verifyEnvMySQLHost = "TASKENGINE_TEST_MYSQL_HOST"
	// verifyEnvMySQLUser MySQL 用户名。
	verifyEnvMySQLUser = "TASKENGINE_TEST_MYSQL_USER"
	// verifyEnvMySQLPassword MySQL 密码。
	verifyEnvMySQLPassword = "TASKENGINE_TEST_MYSQL_PASSWORD"
	// verifyEnvMySQLDB MySQL 库名，必须含 test。
	verifyEnvMySQLDB = "TASKENGINE_TEST_MYSQL_DB"
	// verifyEnvRedisAddr Redis 地址。
	verifyEnvRedisAddr = "TASKENGINE_TEST_REDIS_ADDR"
	// verifyEnvRedisPassword Redis 密码，可为空。
	verifyEnvRedisPassword = "TASKENGINE_TEST_REDIS_PASSWORD"
	// verifyEnvRedisDB Redis 库编号，缺省 0。
	verifyEnvRedisDB = "TASKENGINE_TEST_REDIS_DB"
)

// cacheVerifyDef 定义表模型：单列主键 + 数值列 + 状态列。
type cacheVerifyDef struct {
	// ID 主键，同时是 Redis 行键。
	ID int64 `cache:"id,pk" gorm:"column:id;primaryKey" json:"id"`
	// Title 标题，用于验证整行覆盖写。
	Title string `cache:"title" gorm:"column:title" json:"title"`
	// Point 数值列，用于 Increment / CAS。
	Point int64 `cache:"point" gorm:"column:point" json:"point"`
	// Enabled 状态列，用于条件更新与列表过滤。
	Enabled int8 `cache:"enabled" gorm:"column:enabled" json:"enabled"`
	// UpdatedAt 更新时间，验证时间编解码。
	UpdatedAt time.Time `cache:"updated_at" gorm:"column:updated_at" json:"updated_at"`
}

// TableName 固定验证表名，避免 gorm 按结构体名推导出别的表。
func (cacheVerifyDef) TableName() string { return verifyDefTable }

// cacheVerifyProgress 进度表模型：三列行键 + Redis 取号的自增主键。
type cacheVerifyProgress struct {
	// ID 自增主键；Insert 传 0 时由 Redis 取号回填。
	ID int64 `cache:"id,pk" gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	// MemberID 会员 ID（行键第一列）。
	MemberID int64 `cache:"member_id" gorm:"column:member_id" json:"member_id"`
	// TaskID 任务 ID（行键第二列）。
	TaskID int64 `cache:"task_id" gorm:"column:task_id" json:"task_id"`
	// CycleKey 周期片（行键第三列）。
	CycleKey string `cache:"cycle_key" gorm:"column:cycle_key" json:"cycle_key"`
	// Progress 当前进度。
	Progress int64 `cache:"progress" gorm:"column:progress" json:"progress"`
	// Status 状态。
	Status string `cache:"status" gorm:"column:status" json:"status"`
	// SnapshotJSON 快照，DDL 为 NOT NULL，必须给值。
	SnapshotJSON string `cache:"snapshot_json" gorm:"column:snapshot_json" json:"snapshot_json"`
}

// TableName 固定验证表名。
func (cacheVerifyProgress) TableName() string { return verifyProgressTable }

// cacheVerifyIDCond 单列主键条件。
type cacheVerifyIDCond struct {
	// ID 主键指针，nil 表示不过滤。
	ID *int64 `cache:"id"`
}

// cacheVerifyEnabledCond 启用状态条件。
type cacheVerifyEnabledCond struct {
	// Enabled 启用状态指针。
	Enabled *int8 `cache:"enabled"`
}

// cacheVerifyProgressCond 三列行键条件。
type cacheVerifyProgressCond struct {
	// MemberID 会员 ID。
	MemberID *int64 `cache:"member_id"`
	// TaskID 任务 ID。
	TaskID *int64 `cache:"task_id"`
	// CycleKey 周期片。
	CycleKey *string `cache:"cycle_key"`
}

// verifyEnv 集成测试连接参数。
type verifyEnv struct {
	// mysqlHost MySQL 地址 host:port。
	mysqlHost string
	// mysqlUser MySQL 用户名。
	mysqlUser string
	// mysqlPassword MySQL 密码。
	mysqlPassword string
	// mysqlDB MySQL 库名。
	mysqlDB string
	// redisAddr Redis 地址。
	redisAddr string
	// redisPassword Redis 密码。
	redisPassword string
	// redisDB Redis 库编号。
	redisDB int
}

// loadVerifyEnv 读取 7 个环境变量；缺任一 MySQL 必填项或 Redis 地址时 Skip。
func loadVerifyEnv(t *testing.T) verifyEnv {
	t.Helper()
	env := verifyEnv{
		mysqlHost:     os.Getenv(verifyEnvMySQLHost),
		mysqlUser:     os.Getenv(verifyEnvMySQLUser),
		mysqlPassword: os.Getenv(verifyEnvMySQLPassword),
		mysqlDB:       os.Getenv(verifyEnvMySQLDB),
		redisAddr:     os.Getenv(verifyEnvRedisAddr),
		redisPassword: os.Getenv(verifyEnvRedisPassword),
	}
	if env.mysqlHost == "" || env.mysqlUser == "" || env.mysqlDB == "" || env.redisAddr == "" {
		t.Skipf("跳过：需要 %s/%s/%s/%s/%s",
			verifyEnvMySQLHost, verifyEnvMySQLUser, verifyEnvMySQLDB, verifyEnvRedisAddr, verifyEnvRedisPassword)
	}
	// 库名护栏：验证会 DROP/TRUNCATE 验证表，只允许指向测试库。
	if !strings.Contains(strings.ToLower(env.mysqlDB), "test") {
		t.Fatalf("%s 必须包含 test，当前 %q", verifyEnvMySQLDB, env.mysqlDB)
	}
	if raw := os.Getenv(verifyEnvRedisDB); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("%s 不是合法整数: %q", verifyEnvRedisDB, raw)
		}
		env.redisDB = n
	}
	return env
}

// newVerifyRedis 连接真 Redis 并连通性自检。
func newVerifyRedis(t *testing.T, env verifyEnv) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{
		Addr:     env.redisAddr,
		Password: env.redisPassword,
		DB:       env.redisDB,
	})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		rdb.Close()
		t.Fatalf("连接 Redis 失败: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// clearVerifyKeys 删除本前缀下的全部键（用例前后各清一次）。
func clearVerifyKeys(t *testing.T, rdb *redis.Client) {
	t.Helper()
	ctx := context.Background()
	var cursor uint64
	for {
		keys, next, err := rdb.Scan(ctx, cursor, "{"+verifyKeyPrefix+":*", 500).Result()
		if err != nil {
			t.Fatalf("扫描验证键失败: %v", err)
		}
		if len(keys) > 0 {
			if err := rdb.Del(ctx, keys...).Err(); err != nil {
				t.Fatalf("删除验证键失败: %v", err)
			}
		}
		cursor = next
		if cursor == 0 {
			return
		}
	}
}

// verifyInitCache 用验证前缀初始化默认缓存引擎（落库定时器拉长，靠显式 flush）。
func verifyInitCache(t *testing.T, rdb *redis.Client) {
	t.Helper()
	cfg := CacheConfigFrom(&config.CacheConfig{
		Enabled:         true,
		KeyPrefix:       verifyKeyPrefix,
		FlushIntervalMS: 3600 * 1000,
		QueryTimeoutMS:  5000,
	})
	if err := InitCache(cfg, rdb); err != nil {
		t.Fatalf("InitCache: %v", err)
	}
	t.Cleanup(func() { CloseCache() })
}

// verifyDBCreate 建真 MySQL 客户端；验证库必须已存在。
func verifyDBCreate(t *testing.T, env verifyEnv) *Client {
	t.Helper()
	client, err := NewMySql(&config.MySqlDBConfig{
		Host:     env.mysqlHost,
		Username: env.mysqlUser,
		Password: env.mysqlPassword,
		DBName:   env.mysqlDB,
		Loc:      "UTC",
	})
	if err != nil {
		t.Fatalf("连接 MySQL 失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// verifyDDL 重建验证表（MySQL 8.0，utf8mb4）。
func verifyDDL(t *testing.T, client *Client) {
	t.Helper()
	gdb := client.Engine()
	if gdb == nil {
		t.Fatal("MySQL engine 为空")
	}
	stmts := []string{
		"DROP TABLE IF EXISTS `cache_verify_progress`",
		"DROP TABLE IF EXISTS `cache_verify_def`",
		"CREATE TABLE `cache_verify_def` (" +
			"`id` bigint NOT NULL," +
			"`title` varchar(128) NOT NULL DEFAULT ''," +
			"`point` bigint NOT NULL DEFAULT 0," +
			"`enabled` tinyint NOT NULL DEFAULT 1," +
			"`updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"PRIMARY KEY (`id`)" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",
		"CREATE TABLE `cache_verify_progress` (" +
			"`id` bigint NOT NULL AUTO_INCREMENT," +
			"`member_id` bigint NOT NULL," +
			"`task_id` bigint NOT NULL," +
			"`cycle_key` varchar(32) NOT NULL," +
			"`progress` bigint NOT NULL DEFAULT 0," +
			"`status` varchar(16) NOT NULL DEFAULT 'active'," +
			"`snapshot_json` text NOT NULL," +
			"PRIMARY KEY (`id`)," +
			"UNIQUE KEY `uk_cycle` (`member_id`,`task_id`,`cycle_key`)" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",
	}
	for _, stmt := range stmts {
		if err := gdb.Exec(stmt).Error; err != nil {
			t.Fatalf("执行 DDL 失败 %q: %v", stmt, err)
		}
	}
}

// verifyTableTag 返回表在 Redis 中的 hash tag。
func verifyTableTag(table string) string { return "{" + verifyKeyPrefix + ":" + table + "}" }

// verifyRowKey 行 hash 键。
func verifyRowKey(table, key string) string { return verifyTableTag(table) + ":r:" + key }

// verifyKeySet 存活行键集。
func verifyKeySet(table string) string { return verifyTableTag(table) + ":k" }

// verifyDirty 异步落库脏账。
func verifyDirty(table string) string { return verifyTableTag(table) + ":dirty" }

// verifyDirtyDel 异步删除账。
func verifyDirtyDel(table string) string { return verifyTableTag(table) + ":dirtyDel" }

// verifyTomb 删除墓碑键。
func verifyTomb(table, key string) string { return verifyTableTag(table) + ":t:" + key }

// verifyMySQLDef 读定义表一行；不存在返回 ok=false。
func verifyMySQLDef(t *testing.T, client *Client, id int64) (title string, point int64, ok bool) {
	t.Helper()
	row := client.Engine().Raw("SELECT title, point FROM cache_verify_def WHERE id = ?", id).Row()
	if err := row.Scan(&title, &point); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", 0, false
		}
		t.Fatalf("查询 MySQL 定义行失败: %v", err)
	}
	return title, point, true
}

// verifyMySQLDefCount 统计定义表行数。
func verifyMySQLDefCount(t *testing.T, client *Client) int64 {
	t.Helper()
	var n int64
	if err := client.Engine().Raw("SELECT COUNT(*) FROM cache_verify_def").Scan(&n).Error; err != nil {
		t.Fatalf("统计 MySQL 定义行失败: %v", err)
	}
	return n
}

// verifyMySQLProgressCount 统计进度表行数。
func verifyMySQLProgressCount(t *testing.T, client *Client) int64 {
	t.Helper()
	var n int64
	if err := client.Engine().Raw("SELECT COUNT(*) FROM cache_verify_progress").Scan(&n).Error; err != nil {
		t.Fatalf("统计 MySQL 进度行失败: %v", err)
	}
	return n
}

// mustHGetAll 读取行 hash 供日志取证。
func mustHGetAll(t *testing.T, rdb *redis.Client, key string) map[string]string {
	t.Helper()
	h, err := rdb.HGetAll(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("HGETALL %s: %v", key, err)
	}
	return h
}

// verifyAssertRedisRow 断言 Redis 行 hash 存在且指定列值匹配。
func verifyAssertRedisRow(t *testing.T, rdb *redis.Client, key string, want map[string]string) map[string]string {
	t.Helper()
	h, err := rdb.HGetAll(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("HGETALL %s: %v", key, err)
	}
	if len(h) == 0 {
		t.Fatalf("Redis 行不存在: %s", key)
	}
	for col, v := range want {
		if h[col] != v {
			t.Fatalf("Redis %s 列 %s=%q，期望 %q", key, col, h[col], v)
		}
	}
	return h
}

// verifyAssertKeySetMember 断言键集成员存在/不存在。
func verifyAssertKeySetMember(t *testing.T, rdb *redis.Client, key, member string, want bool) {
	t.Helper()
	ok, err := rdb.SIsMember(context.Background(), key, member).Result()
	if err != nil {
		t.Fatalf("SISMEMBER %s: %v", key, err)
	}
	if ok != want {
		t.Fatalf("键集 %s 成员 %q 存在=%v，期望 %v", key, member, ok, want)
	}
}

// verifyAssertZSetMember 断言 ZSET 成员存在/不存在。
func verifyAssertZSetMember(t *testing.T, rdb *redis.Client, key, member string, want bool) {
	t.Helper()
	_, err := rdb.ZScore(context.Background(), key, member).Result()
	if want {
		if err != nil {
			t.Fatalf("ZSET %s 应含成员 %q: %v", key, member, err)
		}
		return
	}
	if !errors.Is(err, redis.Nil) {
		t.Fatalf("ZSET %s 不应含成员 %q，实际 err=%v", key, member, err)
	}
}

// verifyIntField 解析 hash 里的整型列。
func verifyIntField(t *testing.T, h map[string]string, col string) int64 {
	t.Helper()
	raw, ok := h[col]
	if !ok {
		t.Fatalf("行 hash 缺少列 %q", col)
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("列 %q=%q 不是整数: %v", col, raw, err)
	}
	return n
}

// TestCacheVerifyPhaseARedisOnly 阶段 A：MySQL 完全缺席（CacheBindRedis）。
func TestCacheVerifyPhaseARedisOnly(t *testing.T) {
	env := loadVerifyEnv(t)
	rdb := newVerifyRedis(t, env)
	clearVerifyKeys(t, rdb)
	t.Cleanup(func() { clearVerifyKeys(t, rdb) })

	verifyInitCache(t, rdb)
	// 纯 Redis 权威绑定：不构造、不注入任何 MySQL client。
	if err := CacheBindRedis[cacheVerifyDef](verifyDefTable, CacheBindOption[cacheVerifyDef]{
		KeyColumns: []string{"id"},
	}); err != nil {
		t.Fatalf("绑定 %s: %v", verifyDefTable, err)
	}
	if err := CacheBindRedis[cacheVerifyProgress](verifyProgressTable, CacheBindOption[cacheVerifyProgress]{
		KeyColumns:      []string{"member_id", "task_id", "cycle_key"},
		ConflictColumns: []string{"member_id", "task_id", "cycle_key"},
		AutoIncrement:   "id",
	}); err != nil {
		t.Fatalf("绑定 %s: %v", verifyProgressTable, err)
	}

	ctx := context.Background()
	id1 := int64(1)
	cond1 := &cacheVerifyIDCond{ID: &id1}
	rowKey := verifyRowKey(verifyDefTable, "1")

	t.Run("insert_and_redis_layout", func(t *testing.T) {
		def := &cacheVerifyDef{ID: 1, Title: "v1", Point: 100, Enabled: 1, UpdatedAt: time.Now().UTC()}
		n, err := CacheInsert[cacheVerifyDef](ctx, verifyDefTable, def)
		if err != nil || n != 1 {
			t.Fatalf("首次 Insert 应 affected=1，实际 n=%d err=%v", n, err)
		}
		h := verifyAssertRedisRow(t, rdb, rowKey, map[string]string{
			"id": "1", "title": "v1", "point": "100", "enabled": "1",
		})
		verifyAssertKeySetMember(t, rdb, verifyKeySet(verifyDefTable), "1", true)
		verifyAssertZSetMember(t, rdb, verifyDirty(verifyDefTable), "1", true)
		t.Logf("Redis 行 %s = %v", rowKey, h)
		if ver := verifyIntField(t, h, verifyVersionField); ver < 1 {
			t.Fatalf("首次写入版本应 >=1，实际 %d", ver)
		}

		// 重复 Insert：唯一冲突 affected=0，且不得覆盖已有行。
		dup := &cacheVerifyDef{ID: 1, Title: "should-not-overwrite", Point: 999, Enabled: 1}
		n, err = CacheInsert[cacheVerifyDef](ctx, verifyDefTable, dup)
		if err != nil || n != 0 {
			t.Fatalf("重复 Insert 应 affected=0，实际 n=%d err=%v", n, err)
		}
		verifyAssertRedisRow(t, rdb, rowKey, map[string]string{"title": "v1", "point": "100"})
	})

	t.Run("query_point_and_list", func(t *testing.T) {
		var got cacheVerifyDef
		if err := CacheQuery[cacheVerifyDef](ctx, verifyDefTable, cond1, &got); err != nil {
			t.Fatalf("点查: %v", err)
		}
		if got.ID != 1 || got.Title != "v1" || got.Point != 100 {
			t.Fatalf("点查结果不符: %+v", got)
		}

		one := int8(1)
		var rows []cacheVerifyDef
		if err := CacheQuery[cacheVerifyDef](ctx, verifyDefTable, &cacheVerifyEnabledCond{Enabled: &one}, &rows); err != nil {
			t.Fatalf("列表查询: %v", err)
		}
		if len(rows) != 1 || rows[0].ID != 1 {
			t.Fatalf("列表查询应命中 1 行，实际 %+v", rows)
		}

		missing := int64(999)
		var none cacheVerifyDef
		if err := CacheQuery[cacheVerifyDef](ctx, verifyDefTable, &cacheVerifyIDCond{ID: &missing}, &none); !errors.Is(err, ErrCacheNoRows) {
			t.Fatalf("不存在行应 ErrCacheNoRows，实际 %v", err)
		}
	})

	t.Run("save_update", func(t *testing.T) {
		before, err := rdb.HGet(ctx, rowKey, verifyVersionField).Result()
		if err != nil {
			t.Fatalf("读取写前版本: %v", err)
		}
		def := &cacheVerifyDef{ID: 1, Title: "v2", Point: 100, Enabled: 1, UpdatedAt: time.Now().UTC()}
		if err := CacheSave[cacheVerifyDef](ctx, verifyDefTable, "", def, false); err != nil {
			t.Fatalf("更新写: %v", err)
		}
		after := verifyAssertRedisRow(t, rdb, rowKey, map[string]string{"title": "v2"})
		if after[verifyVersionField] == before {
			t.Fatalf("更新后版本应递增，实际仍为 %q", before)
		}
		var got cacheVerifyDef
		if err := CacheQuery[cacheVerifyDef](ctx, verifyDefTable, cond1, &got); err != nil || got.Title != "v2" {
			t.Fatalf("更新后点查应 v2，实际 %+v err=%v", got, err)
		}
	})

	t.Run("increment_and_cas", func(t *testing.T) {
		var affected int64
		// 原子累加：Redis Lua 对 live 行 +5。
		if err := CacheSave[cacheVerifyDef](ctx, verifyDefTable, "", nil, false,
			MatchStruct(cond1), Increment("point", 5), Affected(&affected)); err != nil {
			t.Fatalf("Increment: %v", err)
		}
		if affected != 1 {
			t.Fatalf("Increment affected 应为 1，实际 %d", affected)
		}
		verifyAssertRedisRow(t, rdb, rowKey, map[string]string{"point": "105"})

		// CAS 命中：enabled 1 → 0。
		affected = 0
		if err := CacheSave[cacheVerifyDef](ctx, verifyDefTable, "", nil, false,
			MatchStruct(cond1),
			CompareAndSwap(map[string]any{"enabled": int8(1)}, map[string]any{"enabled": int8(0)}),
			Affected(&affected)); err != nil {
			t.Fatalf("CAS 命中: %v", err)
		}
		if affected != 1 {
			t.Fatalf("CAS 命中 affected 应为 1，实际 %d", affected)
		}
		verifyAssertRedisRow(t, rdb, rowKey, map[string]string{"enabled": "0"})

		// CAS 未命中：expect 已不成立 → affected=0 且零副作用。
		affected = 0
		if err := CacheSave[cacheVerifyDef](ctx, verifyDefTable, "", nil, false,
			MatchStruct(cond1),
			CompareAndSwap(map[string]any{"enabled": int8(1)}, map[string]any{"enabled": int8(9)}),
			Affected(&affected)); err != nil {
			t.Fatalf("CAS 未命中: %v", err)
		}
		if affected != 0 {
			t.Fatalf("CAS 未命中 affected 应为 0，实际 %d", affected)
		}
		verifyAssertRedisRow(t, rdb, rowKey, map[string]string{"enabled": "0"})
	})

	t.Run("tx_commit_and_rollback", func(t *testing.T) {
		member, taskID := int64(7), int64(11)
		cycle := "2026-11-11"

		// 提交：整行写 + 进度插入。
		err := CacheSave[cacheVerifyDef](ctx, verifyDefTable, "", nil, false, InTx(func(tctx context.Context) error {
			if err := CacheSave[cacheVerifyDef](tctx, verifyDefTable, "", &cacheVerifyDef{
				ID: 1, Title: "v3", Point: 105, Enabled: 0, UpdatedAt: time.Now().UTC(),
			}, false); err != nil {
				return err
			}
			_, ierr := CacheInsert[cacheVerifyProgress](tctx, verifyProgressTable, &cacheVerifyProgress{
				MemberID: member, TaskID: taskID, CycleKey: cycle,
				Progress: 3, Status: "active", SnapshotJSON: "{}",
			})
			return ierr
		}))
		if err != nil {
			t.Fatalf("事务提交: %v", err)
		}
		verifyAssertRedisRow(t, rdb, rowKey, map[string]string{"title": "v3"})
		var prog cacheVerifyProgress
		if err := CacheQuery[cacheVerifyProgress](ctx, verifyProgressTable, &cacheVerifyProgressCond{
			MemberID: &member, TaskID: &taskID, CycleKey: &cycle,
		}, &prog); err != nil || prog.Progress != 3 {
			t.Fatalf("事务提交后进度应可读，实际 %+v err=%v", prog, err)
		}

		// 回滚：函数返回错误 → 本次写不得生效。
		rollbackErr := errors.New("verify rollback")
		err = CacheSave[cacheVerifyDef](ctx, verifyDefTable, "", nil, false, InTx(func(tctx context.Context) error {
			if err := CacheSave[cacheVerifyDef](tctx, verifyDefTable, "", &cacheVerifyDef{
				ID: 1, Title: "rolled-back", Point: 999, Enabled: 0, UpdatedAt: time.Now().UTC(),
			}, false); err != nil {
				return err
			}
			return rollbackErr
		}))
		if !errors.Is(err, rollbackErr) {
			t.Fatalf("事务应返回回滚错误，实际 %v", err)
		}
		verifyAssertRedisRow(t, rdb, rowKey, map[string]string{"title": "v3", "point": "105"})
	})

	t.Run("delete_and_no_sink_flush", func(t *testing.T) {
		if err := CacheDelete(ctx, verifyDefTable, cond1); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		// 行 hash / 键集 / 脏账即清，删除账与墓碑建立。
		if n, err := rdb.HLen(ctx, rowKey).Result(); err != nil || n != 0 {
			t.Fatalf("删除后行 hash 应为空，实际 n=%d err=%v", n, err)
		}
		verifyAssertKeySetMember(t, rdb, verifyKeySet(verifyDefTable), "1", false)
		verifyAssertZSetMember(t, rdb, verifyDirty(verifyDefTable), "1", false)
		// 无 sink 时删除不产生异步删除账（op.Sink=false），只留墓碑。
		verifyAssertZSetMember(t, rdb, verifyDirtyDel(verifyDefTable), "1", false)
		if ok, err := rdb.Exists(ctx, verifyTomb(verifyDefTable, "1")).Result(); err != nil || ok != 1 {
			t.Fatalf("删除后墓碑应存在，实际 %d err=%v", ok, err)
		}

		var gone cacheVerifyDef
		if err := CacheQuery[cacheVerifyDef](ctx, verifyDefTable, cond1, &gone); !errors.Is(err, ErrCacheNoRows) {
			t.Fatalf("删除后点查应 ErrCacheNoRows，实际 %v", err)
		}
		// 无 sink：Flush 必须 no-op 且不报错。
		if err := FlushCache(ctx, verifyDefTable); err != nil {
			t.Fatalf("无 MySQL 时 Flush 应 no-op，实际 %v", err)
		}
	})
}

// TestCacheVerifyPhaseBMySQLSink 阶段 B：挂真实 MySQL sink 与回源。
func TestCacheVerifyPhaseBMySQLSink(t *testing.T) {
	env := loadVerifyEnv(t)
	rdb := newVerifyRedis(t, env)
	clearVerifyKeys(t, rdb)
	t.Cleanup(func() { clearVerifyKeys(t, rdb) })

	client := verifyDBCreate(t, env)
	verifyDDL(t, client)

	verifyInitCache(t, rdb)
	if err := CacheBindMySQL[cacheVerifyDef](verifyDefTable, client, CacheBindOption[cacheVerifyDef]{
		KeyColumns:          []string{"id"},
		ConflictColumns:     []string{"id"},
		OrderBy:             []string{"id DESC"},
		AllowSourceBackfill: true,
	}); err != nil {
		t.Fatalf("绑定 %s: %v", verifyDefTable, err)
	}
	if err := CacheBindMySQL[cacheVerifyProgress](verifyProgressTable, client, CacheBindOption[cacheVerifyProgress]{
		KeyColumns:          []string{"member_id", "task_id", "cycle_key"},
		ConflictColumns:     []string{"member_id", "task_id", "cycle_key"},
		AutoIncrement:       "id",
		OrderBy:             []string{"id DESC"},
		AllowSourceBackfill: true,
	}); err != nil {
		t.Fatalf("绑定 %s: %v", verifyProgressTable, err)
	}

	ctx := context.Background()
	id1 := int64(1)
	cond1 := &cacheVerifyIDCond{ID: &id1}
	rowKey := verifyRowKey(verifyDefTable, "1")

	t.Run("mustsave_immediate", func(t *testing.T) {
		def := &cacheVerifyDef{ID: 1, Title: "db-v1", Point: 100, Enabled: 1, UpdatedAt: time.Now().UTC()}
		if err := CacheSave[cacheVerifyDef](ctx, verifyDefTable, "", def, true); err != nil {
			t.Fatalf("mustSave 写: %v", err)
		}
		title, point, ok := verifyMySQLDef(t, client, 1)
		if !ok || title != "db-v1" || point != 100 {
			t.Fatalf("mustSave=true 应立即落库，实际 ok=%v title=%q point=%d", ok, title, point)
		}
		verifyAssertRedisRow(t, rdb, rowKey, map[string]string{"title": "db-v1", "point": "100"})
		t.Logf("MySQL 立即可见 id=1 title=%q point=%d；Redis 行=%v", title, point, mustHGetAll(t, rdb, rowKey))
		if n, err := rdb.ZCard(ctx, verifyDirty(verifyDefTable)).Result(); err != nil || n != 0 {
			t.Fatalf("同步落库后脏账应为空，实际 n=%d err=%v", n, err)
		}
	})

	t.Run("async_flush", func(t *testing.T) {
		def := &cacheVerifyDef{ID: 1, Title: "db-v2", Point: 200, Enabled: 1, UpdatedAt: time.Now().UTC()}
		if err := CacheSave[cacheVerifyDef](ctx, verifyDefTable, "", def, false); err != nil {
			t.Fatalf("延迟落库写: %v", err)
		}
		// flush 前：Redis 已是新值，MySQL 仍是旧值（sink 滞后）。
		verifyAssertRedisRow(t, rdb, rowKey, map[string]string{"title": "db-v2", "point": "200"})
		if _, point, _ := verifyMySQLDef(t, client, 1); point != 100 {
			t.Fatalf("flush 前 MySQL 应为旧值 100，实际 %d", point)
		}
		verifyAssertZSetMember(t, rdb, verifyDirty(verifyDefTable), "1", true)

		if err := FlushCache(ctx, verifyDefTable); err != nil {
			t.Fatalf("FlushCache: %v", err)
		}
		title, point, ok := verifyMySQLDef(t, client, 1)
		if !ok || title != "db-v2" || point != 200 {
			t.Fatalf("flush 后 MySQL 应为 db-v2/200，实际 ok=%v title=%q point=%d", ok, title, point)
		}
		verifyAssertZSetMember(t, rdb, verifyDirty(verifyDefTable), "1", false)
	})

	t.Run("insert_auto_increment_and_conflict", func(t *testing.T) {
		member, taskID := int64(7), int64(11)
		cycle := "2026-11-11"
		row := &cacheVerifyProgress{MemberID: member, TaskID: taskID, CycleKey: cycle,
			Progress: 1, Status: "active", SnapshotJSON: "{}"}
		n, err := CacheInsert[cacheVerifyProgress](ctx, verifyProgressTable, row)
		if err != nil || n != 1 {
			t.Fatalf("Insert 进度应 affected=1，实际 n=%d err=%v", n, err)
		}
		if row.ID <= 0 {
			t.Fatalf("自增 ID 应即时回填 >0，实际 %d", row.ID)
		}
		if err := FlushCache(ctx, verifyProgressTable); err != nil {
			t.Fatalf("Flush progress: %v", err)
		}
		if got := verifyMySQLProgressCount(t, client); got != 1 {
			t.Fatalf("flush 后 MySQL 进度行应为 1，实际 %d", got)
		}

		dup := &cacheVerifyProgress{MemberID: member, TaskID: taskID, CycleKey: cycle,
			Progress: 99, Status: "active", SnapshotJSON: "{}"}
		n, err = CacheInsert[cacheVerifyProgress](ctx, verifyProgressTable, dup)
		if err != nil || n != 0 {
			t.Fatalf("同周期重复 Insert 应 affected=0，实际 n=%d err=%v", n, err)
		}
	})

	t.Run("atomic_increment_cas_sink", func(t *testing.T) {
		var affected int64
		if err := CacheSave[cacheVerifyDef](ctx, verifyDefTable, "", nil, false,
			MatchStruct(cond1), Increment("point", 7), Affected(&affected)); err != nil {
			t.Fatalf("Increment: %v", err)
		}
		if affected != 1 {
			t.Fatalf("Increment affected 应为 1，实际 %d", affected)
		}
		if err := CacheSave[cacheVerifyDef](ctx, verifyDefTable, "", nil, false,
			MatchStruct(cond1),
			CompareAndSwap(map[string]any{"enabled": int8(1)}, map[string]any{"enabled": int8(0)}),
			Affected(&affected)); err != nil {
			t.Fatalf("CAS: %v", err)
		}
		if affected != 1 {
			t.Fatalf("CAS affected 应为 1，实际 %d", affected)
		}
		if err := FlushCache(ctx, verifyDefTable); err != nil {
			t.Fatalf("FlushCache: %v", err)
		}
		// Redis 207 = 200 + 7；enabled 落库为 0。
		_, point, ok := verifyMySQLDef(t, client, 1)
		if !ok || point != 207 {
			t.Fatalf("原子累加落库应为 207，实际 ok=%v point=%d", ok, point)
		}
		var enabled int8
		if err := client.Engine().Raw("SELECT enabled FROM cache_verify_def WHERE id = ?", 1).Row().Scan(&enabled); err != nil {
			t.Fatalf("查 enabled: %v", err)
		}
		if enabled != 0 {
			t.Fatalf("CAS 落库 enabled 应为 0，实际 %d", enabled)
		}
	})

	t.Run("tx_commit_and_rollback_sink", func(t *testing.T) {
		member, taskID := int64(7), int64(12)
		cycle := "2026-11-11"

		// 提交：定义整行写 + 进度插入，flush 后两者都落库。
		err := CacheSave[cacheVerifyDef](ctx, verifyDefTable, "", nil, false, InTx(func(tctx context.Context) error {
			if err := CacheSave[cacheVerifyDef](tctx, verifyDefTable, "", &cacheVerifyDef{
				ID: 1, Title: "db-v3", Point: 207, Enabled: 0, UpdatedAt: time.Now().UTC(),
			}, false); err != nil {
				return err
			}
			_, ierr := CacheInsert[cacheVerifyProgress](tctx, verifyProgressTable, &cacheVerifyProgress{
				ID: 9001, MemberID: member, TaskID: taskID, CycleKey: cycle,
				Progress: 5, Status: "completed", SnapshotJSON: "{}",
			})
			return ierr
		}))
		if err != nil {
			t.Fatalf("事务提交: %v", err)
		}
		if err := FlushCache(ctx, verifyDefTable); err != nil {
			t.Fatalf("Flush def: %v", err)
		}
		if err := FlushCache(ctx, verifyProgressTable); err != nil {
			t.Fatalf("Flush progress: %v", err)
		}
		if title, _, _ := verifyMySQLDef(t, client, 1); title != "db-v3" {
			t.Fatalf("事务提交后定义应落库 db-v3，实际 %q", title)
		}
		if got := verifyMySQLProgressCount(t, client); got != 2 {
			t.Fatalf("事务提交后进度行应为 2，实际 %d", got)
		}

		// 回滚：函数返回错误 → 本次定义写与进度插入都不落库。
		rollbackErr := errors.New("verify rollback")
		err = CacheSave[cacheVerifyDef](ctx, verifyDefTable, "", nil, false, InTx(func(tctx context.Context) error {
			if err := CacheSave[cacheVerifyDef](tctx, verifyDefTable, "", &cacheVerifyDef{
				ID: 1, Title: "rolled-back", Point: 1, Enabled: 0, UpdatedAt: time.Now().UTC(),
			}, false); err != nil {
				return err
			}
			_, ierr := CacheInsert[cacheVerifyProgress](tctx, verifyProgressTable, &cacheVerifyProgress{
				ID: 9002, MemberID: member, TaskID: taskID, CycleKey: "rollback",
				Progress: 9, Status: "active", SnapshotJSON: "{}",
			})
			if ierr != nil {
				return ierr
			}
			return rollbackErr
		}))
		if !errors.Is(err, rollbackErr) {
			t.Fatalf("事务应返回回滚错误，实际 %v", err)
		}
		if err := FlushCache(ctx, verifyDefTable); err != nil {
			t.Fatalf("Flush def: %v", err)
		}
		if err := FlushCache(ctx, verifyProgressTable); err != nil {
			t.Fatalf("Flush progress: %v", err)
		}
		if title, _, _ := verifyMySQLDef(t, client, 1); title != "db-v3" {
			t.Fatalf("回滚后定义不应变化，实际 %q", title)
		}
		if got := verifyMySQLProgressCount(t, client); got != 2 {
			t.Fatalf("回滚后进度行应仍为 2，实际 %d", got)
		}
	})

	t.Run("backfill_from_mysql_into_redis", func(t *testing.T) {
		// 先清空该表全部 Redis 键（不留墓碑），模拟 Redis 冷启动。
		ctxDel := context.Background()
		var cursor uint64
		for {
			keys, next, err := rdb.Scan(ctxDel, cursor, verifyTableTag(verifyDefTable)+"*", 200).Result()
			if err != nil {
				t.Fatalf("扫描定义表键: %v", err)
			}
			if len(keys) > 0 {
				if err := rdb.Del(ctxDel, keys...).Err(); err != nil {
					t.Fatalf("清空定义表键: %v", err)
				}
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
		if n, err := rdb.HLen(ctx, rowKey).Result(); err != nil || n != 0 {
			t.Fatalf("清空后行 hash 应为空，实际 n=%d err=%v", n, err)
		}

		var got cacheVerifyDef
		if err := CacheQuery[cacheVerifyDef](ctx, verifyDefTable, cond1, &got); err != nil {
			t.Fatalf("回源查询: %v", err)
		}
		if got.ID != 1 || got.Title != "db-v3" {
			t.Fatalf("回源结果不符: %+v", got)
		}
		// 回源后必须已回填 Redis：行 hash + 键集都恢复。
		verifyAssertRedisRow(t, rdb, rowKey, map[string]string{"id": "1", "title": "db-v3"})
		verifyAssertKeySetMember(t, rdb, verifyKeySet(verifyDefTable), "1", true)
		t.Logf("回源后 Redis 行 %s = %v", rowKey, mustHGetAll(t, rdb, rowKey))
	})

	t.Run("delete_tombstone_and_sink", func(t *testing.T) {
		if err := CacheDelete(ctx, verifyDefTable, cond1); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if ok, err := rdb.Exists(ctx, verifyTomb(verifyDefTable, "1")).Result(); err != nil || ok != 1 {
			t.Fatalf("删除后墓碑应存在，实际 %d err=%v", ok, err)
		}
		verifyAssertZSetMember(t, rdb, verifyDirtyDel(verifyDefTable), "1", true)
		verifyAssertKeySetMember(t, rdb, verifyKeySet(verifyDefTable), "1", false)

		if err := FlushCache(ctx, verifyDefTable); err != nil {
			t.Fatalf("FlushCache: %v", err)
		}
		if _, _, ok := verifyMySQLDef(t, client, 1); ok {
			t.Fatal("flush 后 MySQL 定义行应被删除")
		}
		t.Logf("flush 后 MySQL id=1 已删除")
		var gone cacheVerifyDef
		if err := CacheQuery[cacheVerifyDef](ctx, verifyDefTable, cond1, &gone); !errors.Is(err, ErrCacheNoRows) {
			t.Fatalf("删除后点查应 ErrCacheNoRows（墓碑阻止回源），实际 %v", err)
		}
	})

	t.Run("close_final_flush", func(t *testing.T) {
		def := &cacheVerifyDef{ID: 2, Title: "final-flush", Point: 42, Enabled: 1, UpdatedAt: time.Now().UTC()}
		if err := CacheSave[cacheVerifyDef](ctx, verifyDefTable, "", def, false); err != nil {
			t.Fatalf("延迟写: %v", err)
		}
		if _, _, ok := verifyMySQLDef(t, client, 2); ok {
			t.Fatal("final flush 前 MySQL 不应有 id=2（定时器已拉长，未显式 flush）")
		}
		// CloseCache 内部 Stop → final flush，把未落库脏账物化。
		CloseCache()
		title, point, ok := verifyMySQLDef(t, client, 2)
		if !ok || title != "final-flush" || point != 42 {
			t.Fatalf("CloseCache 后应 final flush 落库，实际 ok=%v title=%q point=%d", ok, title, point)
		}
		if n := verifyMySQLDefCount(t, client); n != 1 {
			t.Fatalf("final flush 后定义表应仅有 id=2，实际 %d 行", n)
		}
	})

	// 留档：验证结束时尚存的缓存键（cleanup 会随后清空）。
	if keys, err := rdb.Keys(ctx, "{"+verifyKeyPrefix+":*").Result(); err == nil {
		t.Logf("验证结束 Redis 键（%d 个）：%v", len(keys), keys)
	}
}
