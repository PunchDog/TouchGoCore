package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

// Kind 表示错误的分类（与 mysql 包同构，便于缓存层统一断言）
type Kind uint8

const (
	KindUnknown Kind = iota
	KindConnFailed
	KindAuth
	KindNotFound
	KindDuplicate
	KindTimeout
	KindCanceled
	KindDeadlock
	KindLockWait
	KindSyntax
	KindConstraint
	KindInvalidColumn
	KindPoolExhausted
	KindSlowQuery
)

// String 返回 Kind 的稳定字符串标识
func (k Kind) String() string {
	switch k {
	case KindConnFailed:
		return "conn_failed"
	case KindAuth:
		return "auth"
	case KindNotFound:
		return "not_found"
	case KindDuplicate:
		return "duplicate"
	case KindTimeout:
		return "timeout"
	case KindCanceled:
		return "canceled"
	case KindDeadlock:
		return "deadlock"
	case KindLockWait:
		return "lock_wait"
	case KindSyntax:
		return "syntax"
	case KindConstraint:
		return "constraint"
	case KindInvalidColumn:
		return "invalid_column"
	case KindPoolExhausted:
		return "pool_exhausted"
	case KindSlowQuery:
		return "slow_query"
	default:
		return "unknown"
	}
}

// Error 是 PostgreSQL 包对外暴露的标准错误类型
type Error struct {
	Kind     Kind
	Op       string
	SQL      string
	Args     []any
	Duration time.Duration
	Err      error
}

// Error 实现 error 接口
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("postgres.")
	b.WriteString(e.Op)
	b.WriteString(": ")
	if e.Kind != KindUnknown {
		b.WriteString("[")
		b.WriteString(e.Kind.String())
		b.WriteString("] ")
	}
	if e.Err != nil {
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap 返回内部 error
func (e *Error) Unwrap() error { return e.Err }

// Is 支持 errors.Is
func (e *Error) Is(target error) bool {
	var t *Error
	if errors.As(target, &t) {
		return e.Kind == t.Kind
	}
	return false
}

// newError 构造结构化错误
func newError(op string, err error, kind Kind, sql string, args []any, dur time.Duration) *Error {
	if err == nil {
		return nil
	}
	return &Error{
		Op:       op,
		Kind:     kind,
		SQL:      sql,
		Args:     args,
		Duration: dur,
		Err:      err,
	}
}

// wrap 包裹 error
func wrap(op string, err error, kind Kind) *Error {
	if err == nil {
		return nil
	}
	return newError(op, fmt.Errorf("%s: %w", op, err), kind, "", nil, 0)
}

// PostgreSQL SQLSTATE 错误码（Class 前缀 + 常用具体码）。
// 完整清单见 https://www.postgresql.org/docs/current/errcodes-appendix.html
const (
	sqlstateUniqueViolation     = "23505"
	sqlstateNotNullViolation    = "23502"
	sqlstateForeignKeyViolation = "23503"
	sqlstateCheckViolation      = "23514"
	sqlstateExclusionViolation  = "23P01"
	sqlstateSyntaxError         = "42601"
	sqlstateUndefinedTable      = "42P01"
	sqlstateUndefinedColumn     = "42703"
	sqlstateInvalidAuthSpec     = "28000"
	sqlstateInvalidPassword     = "28P01"
	sqlstateInvalidCatalogName  = "3D000"
	sqlstateTooManyConnections  = "53300"
	sqlstateSerializationFail   = "40001"
	sqlstateDeadlockDetected    = "40P01"
	sqlstateLockNotAvailable    = "55P03"
	sqlstateQueryCanceled       = "57014"
	sqlstateAdminShutdown       = "57P01"
	sqlstateCrashShutdown       = "57P02"
	sqlstateCannotConnectNow    = "57P03"
)

// classify 根据 error 推断 Kind
func classify(err error) Kind {
	if err == nil {
		return KindUnknown
	}
	if errors.Is(err, context.Canceled) {
		return KindCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return KindTimeout
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return KindNotFound
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return KindDuplicate
	}
	if errors.Is(err, gorm.ErrInvalidData) {
		return KindConstraint
	}
	if errors.Is(err, gorm.ErrInvalidField) {
		return KindInvalidColumn
	}
	if errors.Is(err, sql.ErrNoRows) {
		return KindNotFound
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case sqlstateInvalidAuthSpec, sqlstateInvalidPassword:
			return KindAuth
		case sqlstateInvalidCatalogName:
			return KindConnFailed
		case sqlstateUniqueViolation:
			return KindDuplicate
		case sqlstateSyntaxError, sqlstateUndefinedTable:
			return KindSyntax
		case sqlstateUndefinedColumn:
			return KindInvalidColumn
		case sqlstateNotNullViolation, sqlstateForeignKeyViolation,
			sqlstateCheckViolation, sqlstateExclusionViolation:
			return KindConstraint
		case sqlstateSerializationFail, sqlstateDeadlockDetected:
			return KindDeadlock
		case sqlstateLockNotAvailable:
			return KindLockWait
		case sqlstateQueryCanceled:
			return KindCanceled
		case sqlstateTooManyConnections:
			return KindPoolExhausted
		case sqlstateAdminShutdown, sqlstateCrashShutdown, sqlstateCannotConnectNow:
			return KindConnFailed
		}
		// Class 08 = connection_exception，一律归连接失败
		if strings.HasPrefix(pe.Code, "08") {
			return KindConnFailed
		}
	}
	return KindUnknown
}

// IsNotFound 断言语义为未找到记录
func IsNotFound(err error) bool { return kindOf(err) == KindNotFound }

// IsDuplicate 断言为唯一键冲突
func IsDuplicate(err error) bool { return kindOf(err) == KindDuplicate }

// IsTimeout 断言为超时
func IsTimeout(err error) bool { return kindOf(err) == KindTimeout }

// IsCanceled 断言为上下文取消
func IsCanceled(err error) bool { return kindOf(err) == KindCanceled }

// IsDeadlock 断言为死锁/序列化失败
func IsDeadlock(err error) bool { return kindOf(err) == KindDeadlock }

// IsTableNotExist 断言为目标表不存在（SQLSTATE 42P01 undefined_table）。
// 用于驱动自动建表决策；与 KindSyntax 语义不同，需要单独判定。
func IsTableNotExist(err error) bool {
	if err == nil {
		return false
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code == sqlstateUndefinedTable
	}
	return false
}

// kindOf 抽取错误链中的 Kind
func kindOf(err error) Kind {
	if err == nil {
		return KindUnknown
	}
	var de *Error
	if errors.As(err, &de) {
		return de.Kind
	}
	return classify(err)
}

// redactDSN 去除 PostgreSQL DSN（key=value 空格分隔，值可单引号包裹含空格）中的 password 段。
// 用于 Error.SQL 字段卫生：完整 DSN 含密码，绝不允许进入错误对象/日志。
// 未能定位并替换掉密码段时整串丢弃（返回空）——宁可丢掉诊断串也不泄露凭证。
func redactDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	const key = "password="
	idx := strings.Index(dsn, key)
	if idx < 0 {
		return ""
	}
	rest := dsn[idx+len(key):]
	// 定位密码值结束位置：单引号包裹时找到闭合引号，否则到下一个空白
	var end int
	if strings.HasPrefix(rest, "'") {
		closing := strings.Index(rest[1:], "'")
		if closing < 0 {
			return ""
		}
		end = 1 + closing + 1
	} else {
		end = strings.IndexAny(rest, " \t\n")
		if end < 0 {
			end = len(rest)
		}
	}
	return dsn[:idx] + key + "***" + rest[end:]
}
