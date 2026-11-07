package taskengine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"touchgocore/config"
	"touchgocore/db"
	"touchgocore/list"
	"touchgocore/util"
)

// 集成测试环境变量：全部提供才会执行依赖存储的用例，否则整组 Skip。
// MySQL 库名必须含 "test"，避免误连业务库。
const (
	// envMySQLHost MySQL 地址（host:port）。
	envMySQLHost = "TASKENGINE_TEST_MYSQL_HOST"
	// envMySQLUser MySQL 用户名。
	envMySQLUser = "TASKENGINE_TEST_MYSQL_USER"
	// envMySQLPassword MySQL 密码。
	envMySQLPassword = "TASKENGINE_TEST_MYSQL_PASSWORD"
	// envMySQLDB MySQL 库名，必须含 "test"。
	envMySQLDB = "TASKENGINE_TEST_MYSQL_DB"
	// envRedisAddr Redis 地址。
	envRedisAddr = "TASKENGINE_TEST_REDIS_ADDR"
	// envRedisPassword Redis 密码，可空。
	envRedisPassword = "TASKENGINE_TEST_REDIS_PASSWORD"
	// envRedisDB Redis 库编号，缺省 0。
	envRedisDB = "TASKENGINE_TEST_REDIS_DB"
	// testKeyPrefix 测试用缓存键前缀，清理时按 *task_* 删除。
	testKeyPrefix = "tgtest"
)

// testEnv 集成测试连接参数。
type testEnv struct {
	// mysqlHost MySQL 地址。
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

// 集成测试基座状态：整个进程只初始化一次。
var (
	// setupOnce 保证缓存层与表绑定只做一次。
	setupOnce sync.Once
	// setupErr 首次初始化错误。
	setupErr error
	// resetTests 记录本次 testing.T 是否已重置过存储。
	resetTests sync.Map
	// testClient 测试共享的 MySQL 客户端。
	testClient *db.Client
	// testRedis 测试共享的 Redis 客户端。
	testRedis *redis.Client
)

// TestMain 在所有用例结束后卸载缓存层。
func TestMain(m *testing.M) {
	code := m.Run()
	db.CloseCache()
	os.Exit(code)
}

// loadTestEnv 读取集成测试环境变量；缺任一 MySQL 必填项时返回 false。
func loadTestEnv() (testEnv, bool) {
	env := testEnv{
		mysqlHost:     os.Getenv(envMySQLHost),
		mysqlUser:     os.Getenv(envMySQLUser),
		mysqlPassword: os.Getenv(envMySQLPassword),
		mysqlDB:       os.Getenv(envMySQLDB),
		redisAddr:     os.Getenv(envRedisAddr),
		redisPassword: os.Getenv(envRedisPassword),
	}
	if env.mysqlHost == "" || env.mysqlUser == "" || env.mysqlDB == "" || env.redisAddr == "" {
		return testEnv{}, false
	}
	if raw := os.Getenv(envRedisDB); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return testEnv{}, false
		}
		env.redisDB = n
	}
	return env, true
}

// setupStorage 初始化集成存储基座，并在每个用例首次调用时重置数据。
func setupStorage(t *testing.T) {
	t.Helper()
	env, ok := loadTestEnv()
	if !ok {
		t.Skipf("集成测试未配置：需要 %s/%s/%s/%s/%s", envMySQLHost, envMySQLUser, envMySQLDB, envRedisAddr, envRedisPassword)
	}
	// 库名护栏：只允许在测试库上跑，避免误删业务数据。
	if !strings.Contains(strings.ToLower(env.mysqlDB), "test") {
		t.Fatalf("%s 必须包含 test，当前 %q", envMySQLDB, env.mysqlDB)
	}
	setupOnce.Do(func() { setupErr = initStorage(env) })
	if setupErr != nil {
		t.Fatalf("初始化集成测试存储失败: %v", setupErr)
	}
	if _, done := resetTests.LoadOrStore(t, struct{}{}); done {
		return
	}
	resetStorage(t)
}

// initStorage 建客户端、重建表、初始化缓存层并绑定两张表。
func initStorage(env testEnv) error {
	client, err := db.NewMySql(&config.MySqlDBConfig{
		Host:     env.mysqlHost,
		Username: env.mysqlUser,
		Password: env.mysqlPassword,
		DBName:   env.mysqlDB,
	})
	if err != nil {
		return fmt.Errorf("连接 MySQL: %w", err)
	}
	if err := recreateSchema(client); err != nil {
		return err
	}
	rdb := redis.NewClient(&redis.Options{
		Addr:     env.redisAddr,
		Password: env.redisPassword,
		DB:       env.redisDB,
	})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		return fmt.Errorf("连接 Redis: %w", err)
	}
	// 落库定时器拉长到 1 小时：用例内不自动 flush，断言优先读 Redis；
	// 需要 DB 真相时显式 db.FlushCache，避免定时器与断言竞争。
	cfg := db.CacheConfigFrom(&config.CacheConfig{
		Enabled:         true,
		KeyPrefix:       testKeyPrefix,
		FlushIntervalMS: 3600 * 1000,
	})
	if err := db.InitCache(cfg, rdb); err != nil {
		return fmt.Errorf("初始化缓存层: %w", err)
	}
	if err := OpenStorage(client); err != nil {
		return fmt.Errorf("绑定存储表: %w", err)
	}
	testClient = client
	testRedis = rdb
	return nil
}

// recreateSchema 删表后按 schema.sql 重建。
func recreateSchema(client *db.Client) error {
	ctx := context.Background()
	for _, stmt := range []string{
		"DROP TABLE IF EXISTS task_progress",
		"DROP TABLE IF EXISTS task_def",
	} {
		if err := client.Raw(ctx, func(gx *gorm.DB) error { return gx.Exec(stmt).Error }); err != nil {
			return err
		}
	}
	data, err := os.ReadFile("schema.sql")
	if err != nil {
		return err
	}
	for _, part := range strings.Split(string(data), ";") {
		stmt := stripSQLComments(part)
		if stmt == "" {
			continue
		}
		if err := client.Raw(ctx, func(gx *gorm.DB) error { return gx.Exec(stmt).Error }); err != nil {
			return fmt.Errorf("执行 %q: %w", stmt, err)
		}
	}
	return nil
}

// stripSQLComments 去掉整行注释并裁剪空白；纯注释块返回空串。
func stripSQLComments(stmt string) string {
	lines := strings.Split(stmt, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// resetStorage 清 Redis 键、清两张表、重置进程级定义索引与引擎链。
func resetStorage(t *testing.T) {
	t.Helper()
	clearRedisKeys(t)
	execSQL(t, "TRUNCATE TABLE task_progress")
	execSQL(t, "TRUNCATE TABLE task_def")
	taskglobal.defs.Clear()
	taskglobal.engineList = list.NewUnindexedList()
}

// clearRedisKeys 删除本包两张表相关的全部缓存键。
func clearRedisKeys(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var cursor uint64
	for {
		keys, next, err := testRedis.Scan(ctx, cursor, "*task_*", 200).Result()
		if err != nil {
			t.Fatalf("扫描 Redis 键: %v", err)
		}
		if len(keys) > 0 {
			if err := testRedis.Del(ctx, keys...).Err(); err != nil {
				t.Fatalf("删除 Redis 键: %v", err)
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
}

// execSQL 执行一条测试 SQL。
func execSQL(t *testing.T, sql string) {
	t.Helper()
	if err := testClient.Raw(context.Background(), func(gx *gorm.DB) error {
		return gx.Exec(sql).Error
	}); err != nil {
		t.Fatalf("执行 SQL %q: %v", sql, err)
	}
}

// seedDefs 写入任务定义（幂等：同 id 覆盖）。
func seedDefs(t *testing.T, defs ...*TaskDef) {
	t.Helper()
	setupStorage(t)
	now := time.Now().UTC()
	for _, d := range defs {
		if d == nil {
			continue
		}
		row := *d
		if row.CreatedAt.IsZero() {
			row.CreatedAt = now
		}
		if row.UpdatedAt.IsZero() {
			row.UpdatedAt = now
		}
		if err := testClient.Raw(context.Background(), func(gx *gorm.DB) error {
			return gx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
		}); err != nil {
			t.Fatalf("写入任务定义 %d: %v", row.ID, err)
		}
	}
}

// seedProgress 直接写入进度行（幂等：同三列键覆盖）。
func seedProgress(t *testing.T, rows ...TaskProgress) {
	t.Helper()
	setupStorage(t)
	now := time.Now().UTC()
	for i := range rows {
		row := rows[i]
		if row.SnapshotJSON == "" {
			row.SnapshotJSON = "{}"
		}
		if row.CreatedAt.IsZero() {
			row.CreatedAt = now
		}
		if row.UpdatedAt.IsZero() {
			row.UpdatedAt = now
		}
		if err := testClient.Raw(context.Background(), func(gx *gorm.DB) error {
			return gx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
		}); err != nil {
			t.Fatalf("写入进度 member=%d task=%d: %v", row.MemberID, row.TaskID, err)
		}
	}
}

// newTestEngine 构造会员 1 的引擎，并写入给定定义。
func newTestEngine(t *testing.T, defs ...*TaskDef) *Engine {
	return newTestEngineFor(t, 1, defs...)
}

// newTestEngineFor 构造指定会员的引擎，并写入给定定义。
func newTestEngineFor(t *testing.T, memberID int64, defs ...*TaskDef) *Engine {
	t.Helper()
	setupStorage(t)
	seedDefs(t, defs...)
	return newEngineFor(t, memberID)
}

// newEngineFor 只构造引擎，不再重置或写定义。
func newEngineFor(t *testing.T, memberID int64) *Engine {
	t.Helper()
	e, err := New(Options{memberID: memberID})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return e
}

// progressAt 按三列键读进度行（Redis 优先）。
func progressAt(t *testing.T, memberID, taskID int64, cycleKey string) *TaskProgress {
	t.Helper()
	p, err := loadProgressRow(context.Background(), memberID, taskID, cycleKey)
	if err != nil {
		t.Fatalf("读进度 member=%d task=%d: %v", memberID, taskID, err)
	}
	return p
}

// progressOf 读会员 1 在给定定义当前周期的进度行。
func progressOf(t *testing.T, def *TaskDef) *TaskProgress {
	t.Helper()
	return progressAt(t, 1, def.ID, TaskCycleKey(def.Cycle, util.CurrentTime(), def.ID))
}

// flushProgress 把进度表脏键落库。
func flushProgress(t *testing.T) {
	t.Helper()
	if err := db.FlushCache(context.Background(), tableTaskProgress); err != nil {
		t.Fatalf("flush 进度: %v", err)
	}
}

// dbProgressRows 直读数据库中的进度行，供持久化用例断言。
func dbProgressRows(t *testing.T, memberID int64) []TaskProgress {
	t.Helper()
	var rows []TaskProgress
	if err := testClient.Raw(context.Background(), func(gx *gorm.DB) error {
		return gx.Where("member_id = ?", memberID).Order("id ASC").Find(&rows).Error
	}); err != nil {
		t.Fatalf("查询进度行: %v", err)
	}
	return rows
}

// setDefEnabled 通过 UpdateDef（CacheQuery+CacheSave）更新启用状态。
func setDefEnabled(t *testing.T, def *TaskDef, enabled int8) {
	t.Helper()
	clone := *def
	clone.Enabled = enabled
	ok, err := UpdateDef(context.Background(), &clone, util.CurrentTime())
	if err != nil || !ok {
		t.Fatalf("更新定义 %d 失败: ok=%v err=%v", def.ID, ok, err)
	}
	def.Enabled = enabled
}

// dbDef 直读数据库中的定义行。
func dbDef(t *testing.T, id int64) *TaskDef {
	t.Helper()
	var rows []TaskDef
	if err := testClient.Raw(context.Background(), func(gx *gorm.DB) error {
		return gx.Where("id = ?", id).Find(&rows).Error
	}); err != nil {
		t.Fatalf("查询定义 %d: %v", id, err)
	}
	if len(rows) == 0 {
		return nil
	}
	return &rows[0]
}

// TestProgressPersistsOnce 验证延迟写落库走唯一键 Upsert，同周期始终一行。
func TestProgressPersistsOnce(t *testing.T) {
	ctx := context.Background()
	def := sampleDef(1)
	def.GrantMode = GrantAuto
	def.TargetCount = 3
	e := newTestEngine(t, def)

	for i := 1; i <= 3; i++ {
		if err := e.OnEvent(ctx, addParm("CHECKIN")); err != nil {
			t.Fatalf("第 %d 次事件: %v", i, err)
		}
	}
	flushProgress(t)
	rows := dbProgressRows(t, 1)
	if len(rows) != 1 {
		t.Fatalf("同周期应只有一行，实际 %d", len(rows))
	}
	if rows[0].Progress != 3 || rows[0].Status != ProgressClaimed || rows[0].ID == 0 {
		t.Fatalf("落库行=%+v", rows[0])
	}

	// 终态后不再推进，再次 flush 仍是一行。
	if err := e.OnEvent(ctx, addParm("CHECKIN")); err != nil {
		t.Fatalf("终态事件: %v", err)
	}
	flushProgress(t)
	if rows := dbProgressRows(t, 1); len(rows) != 1 {
		t.Fatalf("终态后不得新增行，实际 %d", len(rows))
	}
}

// TestCreateDefAndRead 验证 CreateDef 写入后可由 CacheQuery 回源读到。
func TestCreateDefAndRead(t *testing.T) {
	ctx := context.Background()
	newTestEngine(t, sampleDef(1))

	def := sampleDef(1)
	def.ID = 500
	def.Title = "新任务"
	def.Trigger = "OTHER"
	id, err := CreateDef(ctx, def, util.CurrentTime())
	if err != nil || id != 500 {
		t.Fatalf("CreateDef: id=%d err=%v", id, err)
	}
	got, err := loadDef(ctx, id)
	if err != nil || got == nil || got.Title != "新任务" {
		t.Fatalf("回读定义=%+v err=%v", got, err)
	}
	if dbDef(t, id) == nil {
		t.Fatal("定义应已落库")
	}

	// 行键依赖主键：缺 id 直接拒绝。
	noID := sampleDef(1)
	noID.ID = 0
	if _, err := CreateDef(ctx, noID, util.CurrentTime()); !errors.Is(err, db.ErrCacheKeyMissing) {
		t.Fatalf("缺主键应返回 ErrCacheKeyMissing，实际 %v", err)
	}
}

// TestUpdateDefWritesThroughCache 验证 UpdateDef 用 CacheSave 同步缓存与数据库。
func TestUpdateDefWritesThroughCache(t *testing.T) {
	ctx := context.Background()
	def := sampleDef(1)
	_ = newTestEngine(t, def)

	updated := *def
	updated.Title = "改过"
	updated.Enabled = 0
	ok, err := UpdateDef(ctx, &updated, util.CurrentTime())
	if err != nil || !ok {
		t.Fatalf("UpdateDef: ok=%v err=%v", ok, err)
	}
	got, err := loadDef(ctx, def.ID)
	if err != nil || got == nil || got.Title != "改过" || got.Enabled != 0 {
		t.Fatalf("缓存读回=%+v err=%v", got, err)
	}
	row := dbDef(t, def.ID)
	if row == nil || row.Title != "改过" || row.Enabled != 0 {
		t.Fatalf("数据库行=%+v", row)
	}
	// 不存在的 id 返回未命中。
	missing := *def
	missing.ID = 99999
	if ok, err := UpdateDef(ctx, &missing, util.CurrentTime()); err != nil || ok {
		t.Fatalf("缺失定义应返回 false: ok=%v err=%v", ok, err)
	}
}
