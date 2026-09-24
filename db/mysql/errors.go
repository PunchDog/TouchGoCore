package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// Kind 表示错误的分类
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

// Error 是 MySQL 包对外暴露的标准错误类型
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
	b.WriteString("mysql.")
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
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case 1045:
			return KindAuth
		case 1049:
			return KindConnFailed
		case 1062:
			return KindDuplicate
		case 1064:
			return KindSyntax
		case 1146:
			return KindSyntax
		case 1205:
			return KindLockWait
		case 1213:
			return KindDeadlock
		case 1216, 1451, 1452, 1217, 1215:
			return KindConstraint
		case 2002, 2003, 2006, 2013:
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

// IsDeadlock 断言为死锁
func IsDeadlock(err error) bool { return kindOf(err) == KindDeadlock }

// IsTableNotExist 断言为目标表不存在（MySQL 1146）。
// 用于驱动自动建表决策；与 KindSyntax 语义不同，需要单独判定。
func IsTableNotExist(err error) bool {
	if err == nil {
		return false
	}
	// MySQL 错误码 1146（Table 'db.table' doesn't exist）
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number == 1146
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

// redactSQL 简单脱敏
func redactSQL(s string) string {
	if len(s) > 512 {
		s = s[:512] + "..."
	}
	return s
}
