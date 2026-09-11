package mysql

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

func TestKind_String(t *testing.T) {
	cases := map[Kind]string{
		KindUnknown:    "unknown",
		KindConnFailed: "conn_failed",
		KindAuth:       "auth",
		KindNotFound:   "not_found",
		KindDuplicate:  "duplicate",
		KindTimeout:    "timeout",
		KindCanceled:   "canceled",
		KindDeadlock:   "deadlock",
		KindLockWait:   "lock_wait",
		KindSyntax:     "syntax",
		KindSlowQuery:  "slow_query",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Fatalf("Kind(%d)=%s want=%s", k, got, want)
		}
	}
}

func TestError_ErrorAndUnwrap(t *testing.T) {
	inner := errors.New("connection refused")
	e := newError("Open", inner, KindConnFailed, "SELECT 1", nil, 100)
	if e.Error() == "" {
		t.Fatal("Error string 不应为空")
	}
	if e.Kind != KindConnFailed {
		t.Fatalf("Kind=%v", e.Kind)
	}
	if e.Unwrap() != inner {
		t.Fatal("Unwrap 应返回原始 error")
	}
	if !errors.Is(e, inner) {
		t.Fatal("errors.Is(e, inner) 应为 true")
	}
}

func TestError_Is(t *testing.T) {
	e1 := newError("Open", errors.New("dup"), KindDuplicate, "", nil, 0)
	e2 := newError("Open", errors.New("dup"), KindDuplicate, "", nil, 0)
	if !errors.Is(e1, e2) {
		t.Fatal("同 Kind 的两个 Error 应互相 Is")
	}
	e3 := newError("Open", errors.New("nf"), KindNotFound, "", nil, 0)
	if errors.Is(e1, e3) {
		t.Fatal("不同 Kind 不应 Is")
	}
}

func TestClassify_GormNotFound(t *testing.T) {
	if got := classify(gorm.ErrRecordNotFound); got != KindNotFound {
		t.Fatalf("classify(ErrRecordNotFound)=%v want=KindNotFound", got)
	}
}

func TestClassify_SqlNoRows(t *testing.T) {
	if got := classify(sql.ErrNoRows); got != KindNotFound {
		t.Fatalf("classify(ErrNoRows)=%v want=KindNotFound", got)
	}
}

func TestClassify_ContextCanceled(t *testing.T) {
	if got := classify(context.Canceled); got != KindCanceled {
		t.Fatalf("classify(context.Canceled)=%v want=KindCanceled", got)
	}
}

func TestClassify_ContextDeadline(t *testing.T) {
	if got := classify(context.DeadlineExceeded); got != KindTimeout {
		t.Fatalf("classify(context.DeadlineExceeded)=%v want=KindTimeout", got)
	}
}

func TestClassify_MySQLDuplicate(t *testing.T) {
	merr := &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}
	if got := classify(merr); got != KindDuplicate {
		t.Fatalf("classify(1062)=%v want=KindDuplicate", got)
	}
}

func TestClassify_MySQLDeadlock(t *testing.T) {
	merr := &mysql.MySQLError{Number: 1213, Message: "Deadlock"}
	if got := classify(merr); got != KindDeadlock {
		t.Fatalf("classify(1213)=%v want=KindDeadlock", got)
	}
}

func TestClassify_MySQLAccessDenied(t *testing.T) {
	merr := &mysql.MySQLError{Number: 1045, Message: "Access denied"}
	if got := classify(merr); got != KindAuth {
		t.Fatalf("classify(1045)=%v want=KindAuth", got)
	}
}

func TestClassify_Unknown(t *testing.T) {
	if got := classify(errors.New("some random error")); got != KindUnknown {
		t.Fatalf("classify(未知)=%v want=KindUnknown", got)
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(gorm.ErrRecordNotFound) {
		t.Fatal("IsNotFound(ErrRecordNotFound) 应为 true")
	}
	if IsNotFound(errors.New("other")) {
		t.Fatal("IsNotFound(其他) 应为 false")
	}
}

func TestIsDuplicate(t *testing.T) {
	if !IsDuplicate(&mysql.MySQLError{Number: 1062}) {
		t.Fatal("IsDuplicate(1062) 应为 true")
	}
}

func TestIsTimeout(t *testing.T) {
	if !IsTimeout(context.DeadlineExceeded) {
		t.Fatal("IsTimeout(DeadlineExceeded) 应为 true")
	}
}

func TestKindOf(t *testing.T) {
	inner := errors.New("x")
	e := newError("op", inner, KindDeadlock, "", nil, 0)
	if kindOf(e) != KindDeadlock {
		t.Fatalf("kindOf(*Error)=%v want=KindDeadlock", kindOf(e))
	}
	if kindOf(nil) != KindUnknown {
		t.Fatal("kindOf(nil) 应为 KindUnknown")
	}
}

func TestWrap_NilSafe(t *testing.T) {
	if wrap("op", nil, KindUnknown) != nil {
		t.Fatal("wrap(nil) 应返回 nil")
	}
}