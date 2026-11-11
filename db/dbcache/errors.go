package dbcache

import "errors"

var (
	// ErrNotInitialized 表示 Init 尚未调用。
	ErrNotInitialized = errors.New("dbcache: not initialized")
	// ErrTableUnbound 表示表未通过 Bind 注册后端。
	ErrTableUnbound = errors.New("dbcache: table not bound")
	// ErrTableBound 表示同名表重复绑定。
	ErrTableBound = errors.New("dbcache: table already bound")
	// ErrTypeMismatch 表示调用方模型类型与绑定时注册的类型不一致。
	ErrTypeMismatch = errors.New("dbcache: model type mismatch")
	// ErrEmptyCondition 表示条件为空（拒绝全表查询）。
	ErrEmptyCondition = errors.New("dbcache: empty condition")
	// ErrNoRows 表示数据库中没有符合条件的行。
	ErrNoRows = errors.New("dbcache: no rows")
	// ErrBadOut 表示 out 参数不是 *T 或 *[]T。
	ErrBadOut = errors.New("dbcache: out must be *T or *[]T")
	// ErrBadCond 表示条件不是结构体指针。
	ErrBadCond = errors.New("dbcache: condition must be a struct pointer")
	// ErrKeyMissing 表示无法从行或调用参数得到主键。
	ErrKeyMissing = errors.New("dbcache: primary key missing")
	// ErrDecode 表示 hash 行无法解码回模型。
	ErrDecode = errors.New("dbcache: decode row")
	// ErrFlushIncomplete 表示停机时仍有未落库的脏键。
	ErrFlushIncomplete = errors.New("dbcache: flush incomplete")
	// ErrBadColumn 表示列名不合法或不存在。
	ErrBadColumn = errors.New("dbcache: invalid column")
	// ErrNilRow 表示 Save 收到 nil 行。
	ErrNilRow = errors.New("dbcache: nil row")
	// ErrPartialKey 表示删除条件未覆盖全部行键列（拒绝多行删除）。
	ErrPartialKey = errors.New("dbcache: delete requires all key columns")
	// ErrDuplicateKey 表示唯一守卫冲突（Save 撞上其它行的唯一列值）。
	ErrDuplicateKey = errors.New("dbcache: duplicate unique key")
	// ErrTxConflict 表示事务冲突：行锁等待超时、锁归属丢失或提交校验失败。
	ErrTxConflict = errors.New("dbcache: transaction conflict")
	// ErrSinkRequired 表示 SinkRequired=true 时 mustSave 写入没有可用 sink。
	ErrSinkRequired = errors.New("dbcache: sink required but absent")
	// ErrScanBudgetExceeded 表示无索引扫描超过 MaxScanRows 预算（fail-closed）。
	ErrScanBudgetExceeded = errors.New("dbcache: scan budget exceeded")
)
