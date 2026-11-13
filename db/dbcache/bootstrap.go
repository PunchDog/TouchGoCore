package dbcache

// ============================================================================
// 装配入口：Redis 是唯一读写权威，MySQL 是可拆卸的异步 sink（滞后物化视图）。
//
//	Init(cfg, rdb)            // 只依赖 Redis；RequireAOF 缺失只告警不拒启动
//	BindRedis[T](table, opt)  // 纯 Redis 权威表：无回源、无 sink，拔掉 MySQL 不影响读写
//	BindMySQL[T](table, c, opt) // 额外挂载 MySQL 回源（迁移期）+ 异步落库 sink
//	BackfillTable[T](ctx, table, pageSize) // 迁移工具：源表分页全量播种（幂等）
//	Start(ctx)                // 启动脏账定时器，把提交后的写异步物化到 sink
//
// 资金表用 BindRedis（AllowSourceBackfill 强制 false，fail-closed）；迁移期表用
// BindMySQL 并显式 AllowSourceBackfill=true 允许冷启动回源。sink 缺席时 mustSave
// 按 Config.SinkRequired 决定告警放行或 ErrSinkRequired。
// ============================================================================

import (
	"context"
	"fmt"
	"time"
)

// BindRedis 把一张表绑定为纯 Redis 权威：无 MySQL 回源、无异步 sink。等价于
// BindMySQL(table, nil, opt)，但让「不挂 sink」的装配意图显式化，且强制关闭回源
// （无源可回）。Init 之后即可调用；拔掉 MySQL 不影响本表读写/事务/列表。
func BindRedis[T any](table string, opt BindOption[T]) error {
	return BindMySQL[T](table, nil, opt)
}

// BackfillTable 把源表全量分页播种进 Redis（迁移工具）：srcPage 逐页捞行，
// 每行 save NX 写入——已有行不覆盖、墓碑存活不复活，重复调用幂等。返回本次
// 实际新播种的行数。无 srcPage（纯 Redis 绑定 / Mongo 适配器）时明确报错。
func BackfillTable[T any](ctx context.Context, table string, pageSize int) (int64, error) {
	e := Default()
	if e == nil {
		return 0, ErrNotInitialized
	}
	b, err := e.lookupTyped(table, typeOf[T]())
	if err != nil {
		return 0, err
	}
	if b.srcPage == nil {
		return 0, fmt.Errorf("dbcache: %s 无源分页读取器，无法 backfill（需 BindMySQL/BindPostgres 挂源）", table)
	}
	if pageSize <= 0 {
		pageSize = 500
	}
	var seeded int64
	for offset := 0; ; offset += pageSize {
		rows, perr := b.srcPage(ctx, offset, pageSize)
		if perr != nil {
			return seeded, perr
		}
		for _, row := range rows {
			key, kerr := b.keyFromRow(row)
			if kerr != nil {
				return seeded, kerr
			}
			fields, ferr := b.encodeFields(row)
			if ferr != nil {
				return seeded, ferr
			}
			op := &writeOp{
				Op:      "save",
				NX:      true,
				Fields:  fieldsToMap(fields),
				Due:     time.Now().Add(b.interval).UnixMilli(),
				TombTTL: e.tombTTL().Milliseconds(),
			}
			res, aerr := e.applyWrite(ctx, b, key, op)
			if aerr != nil {
				return seeded, aerr
			}
			if res.Affected > 0 {
				seeded++
			}
		}
		if len(rows) < pageSize {
			break
		}
	}
	// 回填把源表历史行（连同自增 id）灌进了 Redis，序列必须跟着抬上去，
	// 否则接下来第一次取号就会撞上刚灌进来的行键（Insert 返回 affected=0）。
	if b.seedSeq != nil {
		if _, serr := b.seedSeq(ctx); serr != nil {
			return seeded, serr
		}
	}
	return seeded, nil
}
