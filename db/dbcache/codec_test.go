package dbcache

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

type codecModel struct {
	ID     int64          `cache:"id"`
	Int8   int8           `cache:"int8"`
	Uint32 uint32         `cache:"uint32"`
	Float  float64        `cache:"float"`
	Bool   bool           `cache:"bool"`
	Text   string         `cache:"text"`
	Bytes  []byte         `cache:"bytes"`
	At     time.Time      `cache:"at"`
	Dur    time.Duration  `cache:"dur"`
	Nested map[string]int `cache:"nested"`
	List   []string       `cache:"list"`
	Ptr    *string        `cache:"ptr"`
}

func testCodecMeta(t *testing.T) *tableMeta {
	t.Helper()
	m, err := buildMeta(typeOf[codecModel](), []string{"id"}, backendMySQL)
	if err != nil {
		t.Fatalf("buildMeta: %v", err)
	}
	return m
}

func TestCodecRoundTrip(t *testing.T) {
	m := testCodecMeta(t)
	ptr := "hello"
	want := codecModel{
		ID:     1,
		Int8:   -8,
		Uint32: 42,
		Float:  3.25,
		Bool:   true,
		Text:   "文本",
		Bytes:  []byte{0, 1, 2, 255},
		At:     time.Date(2026, 11, 7, 6, 0, 0, 123456789, time.UTC),
		Dur:    3 * time.Second,
		Nested: map[string]int{"a": 1},
		List:   []string{"x", "y"},
		Ptr:    &ptr,
	}
	fields, err := encodeRow(m, reflect.ValueOf(want))
	if err != nil {
		t.Fatalf("encodeRow: %v", err)
	}
	h := make(map[string]string, len(fields)/2)
	for i := 0; i+1 < len(fields); i += 2 {
		h[fields[i]] = fields[i+1]
	}
	var got codecModel
	if err := decodeRow(m, reflect.ValueOf(&got).Elem(), h); err != nil {
		t.Fatalf("decodeRow: %v", err)
	}
	if !got.At.Equal(want.At) {
		t.Fatalf("时间=%v，期望 %v", got.At, want.At)
	}
	got.At = want.At
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip 不一致:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestCodecNilPointerAndMissingColumn(t *testing.T) {
	m := testCodecMeta(t)
	fields, err := encodeRow(m, reflect.ValueOf(codecModel{ID: 1}))
	if err != nil {
		t.Fatalf("encodeRow: %v", err)
	}
	foundEmptyPtr := false
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] == "ptr" {
			if fields[i+1] != "" {
				t.Fatalf("nil 指针应编码为空串，实际 %q", fields[i+1])
			}
			foundEmptyPtr = true
		}
	}
	if !foundEmptyPtr {
		t.Fatal("nil 指针字段必须写入空串，避免旧值残留")
	}
	h := map[string]string{"id": "1"}
	var got codecModel
	if err := decodeRow(m, reflect.ValueOf(&got).Elem(), h); err != nil {
		t.Fatalf("decodeRow: %v", err)
	}
	if got.Ptr != nil {
		t.Fatalf("缺失列应保持 nil，实际 %v", got.Ptr)
	}
}

func TestCodecInvalidValue(t *testing.T) {
	m := testCodecMeta(t)
	var got codecModel
	err := decodeRow(m, reflect.ValueOf(&got).Elem(), map[string]string{"id": "1", "int8": "abc"})
	if !errors.Is(err, ErrDecode) {
		t.Fatalf("期望 ErrDecode，实际 %v", err)
	}
}

func TestDecodeTimeFallbackFormats(t *testing.T) {
	tm, err := decodeValue("2026-11-07 06:00:00", timeType)
	if err != nil {
		t.Fatalf("decodeValue: %v", err)
	}
	if tm.Interface().(time.Time).Year() != 2026 {
		t.Fatalf("解析结果=%v", tm.Interface())
	}
}
