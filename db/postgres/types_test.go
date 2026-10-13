package postgres

import (
	"reflect"
	"testing"
)

func TestJSONB_RoundTrip(t *testing.T) {
	in := JSONB(`{"name":"touchgo","n":42}`)
	v, err := in.Value()
	if err != nil {
		t.Fatalf("Value 失败: %v", err)
	}
	var out JSONB
	if err := out.Scan(v); err != nil {
		t.Fatalf("Scan 失败: %v", err)
	}
	if string(out) != string(in) {
		t.Fatalf("往返不一致: %s vs %s", out, in)
	}

	// nil / 空 → SQL NULL
	var empty JSONB
	vv, _ := empty.Value()
	if vv != nil {
		t.Fatalf("空 JSONB 应产出 nil, 得 %v", vv)
	}
	var sc JSONB
	if err := sc.Scan(nil); err != nil {
		t.Fatalf("Scan(nil) 失败: %v", err)
	}
	if sc != nil {
		t.Fatalf("Scan(nil) 应得 nil, 得 %v", sc)
	}

	// string 源
	var fromStr JSONB
	if err := fromStr.Scan(`{"a":1}`); err != nil {
		t.Fatalf("Scan(string) 失败: %v", err)
	}
	if string(fromStr) != `{"a":1}` {
		t.Fatalf("string 源解析错: %s", fromStr)
	}
}

func TestStringArray_RoundTrip(t *testing.T) {
	in := StringArray{"a", "b,c", `he said "hi"`, `back\slash`}
	v, err := in.Value()
	if err != nil {
		t.Fatalf("Value 失败: %v", err)
	}
	var out StringArray
	if err := out.Scan(v); err != nil {
		t.Fatalf("Scan 失败: %v", err)
	}
	if !reflect.DeepEqual([]string(in), []string(out)) {
		t.Fatalf("往返不一致:\n in=%q\nout=%q", []string(in), []string(out))
	}

	// nil → NULL
	var nilArr StringArray
	if vv, _ := nilArr.Value(); vv != nil {
		t.Fatalf("nil 数组应产出 nil")
	}
	// 空数组
	var empty StringArray
	if err := empty.Scan("{}"); err != nil {
		t.Fatalf("Scan 空数组失败: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("空数组应得 0 元素, 得 %v", empty)
	}
}

func TestInt64Array_RoundTrip(t *testing.T) {
	in := Int64Array{1, 2, 3, -100}
	v, err := in.Value()
	if err != nil {
		t.Fatalf("Value 失败: %v", err)
	}
	var out Int64Array
	if err := out.Scan(v); err != nil {
		t.Fatalf("Scan 失败: %v", err)
	}
	if !reflect.DeepEqual([]int64(in), []int64(out)) {
		t.Fatalf("往返不一致: %v vs %v", in, out)
	}
	// 非法数字应报错
	var bad Int64Array
	if err := bad.Scan("{1,notanumber}"); err == nil {
		t.Fatal("非法数字应报错")
	}
}

func TestHStore_RoundTrip(t *testing.T) {
	in := HStore{"k1": "v1", "k2": "v with space"}
	v, err := in.Value()
	if err != nil {
		t.Fatalf("Value 失败: %v", err)
	}
	var out HStore
	if err := out.Scan(v); err != nil {
		t.Fatalf("Scan 失败: %v", err)
	}
	if !reflect.DeepEqual(map[string]string(in), map[string]string(out)) {
		t.Fatalf("往返不一致: %v vs %v", in, out)
	}
	// nil → NULL
	var nilH HStore
	if vv, _ := nilH.Value(); vv != nil {
		t.Fatalf("nil HStore 应产出 nil")
	}
}

func TestParseArrayLiteral_EdgeCases(t *testing.T) {
	if got, _ := parseArrayLiteral(""); got != nil {
		t.Fatalf("空串应得 nil, 得 %v", got)
	}
	if got, _ := parseArrayLiteral("{}"); got != nil {
		t.Fatalf("空数组应得 nil, 得 %v", got)
	}
	if _, err := parseArrayLiteral("not-an-array"); err == nil {
		t.Fatal("非数组字面量应报错")
	}
	got, err := parseArrayLiteral(`{"a","b"}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("解析结果错: %v", got)
	}
}
