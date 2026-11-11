package dbcache

import (
	"encoding"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"time"
)

var textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
var textUnmarshalerType = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()

// encodeRow 把行对象编码成 HSET 用的扁平 field/value 列表。
func encodeRow(meta *tableMeta, rv reflect.Value) ([]string, error) {
	out := make([]string, 0, len(meta.Fields)*2)
	for _, f := range meta.Fields {
		s, err := encodeValue(rv.Field(f.Index))
		if err != nil {
			return nil, fmt.Errorf("%w: column %s: %v", ErrDecode, f.Column, err)
		}
		out = append(out, f.Column, s)
	}
	return out, nil
}

// decodeRow 把 HGETALL 结果填进结构体；缺失列保持零值。
func decodeRow(meta *tableMeta, rv reflect.Value, h map[string]string) error {
	for _, f := range meta.Fields {
		raw, ok := h[f.Column]
		if !ok {
			continue
		}
		fv := rv.Field(f.Index)
		if !fv.CanSet() {
			continue
		}
		val, err := decodeValue(raw, f.Type)
		if err != nil {
			return fmt.Errorf("%w: column %s: %v", ErrDecode, f.Column, err)
		}
		fv.Set(val)
	}
	return nil
}

// encodeValue 把单个字段编码为 hash 字符串。
func encodeValue(v reflect.Value) (string, error) {
	if !v.IsValid() {
		return "", nil
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return "", nil
		}
		return encodeValue(v.Elem())
	case reflect.Interface:
		if v.IsNil() {
			return "", nil
		}
		return encodeValue(v.Elem())
	}
	if v.Type() == timeType {
		// 定宽 UTC（9 位纳秒）：lex 索引字典序=时间序；RFC3339Nano 变宽会破坏排序。
		// decodeTime 兼容旧变宽格式，旧缓存行解码不受影响。
		return v.Interface().(time.Time).UTC().Format("2006-01-02T15:04:05.000000000Z"), nil
	}
	if v.Type() == durationType {
		return strconv.FormatInt(v.Int(), 10), nil
	}
	if v.CanInterface() {
		if m, ok := v.Interface().(encoding.TextMarshaler); ok {
			b, err := m.MarshalText()
			if err != nil {
				return "", err
			}
			return string(b), nil
		}
	}
	switch v.Kind() {
	case reflect.Bool:
		if v.Bool() {
			return "1", nil
		}
		return "0", nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10), nil
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'g', -1, 64), nil
	case reflect.String:
		return v.String(), nil
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return base64.StdEncoding.EncodeToString(v.Bytes()), nil
		}
		return marshalJSON(v)
	case reflect.Struct, reflect.Map, reflect.Array:
		return marshalJSON(v)
	default:
		return "", fmt.Errorf("unsupported kind %s", v.Kind())
	}
}

// marshalJSON 是嵌套结构/映射/切片的统一编码。
func marshalJSON(v reflect.Value) (string, error) {
	b, err := json.Marshal(v.Interface())
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// decodeValue 把 hash 字符串还原为指定类型。
func decodeValue(raw string, t reflect.Type) (reflect.Value, error) {
	if t.Kind() == reflect.Pointer {
		if raw == "" {
			return reflect.Zero(t), nil
		}
		elem, err := decodeValue(raw, t.Elem())
		if err != nil {
			return reflect.Zero(t), err
		}
		p := reflect.New(t.Elem())
		p.Elem().Set(elem)
		return p, nil
	}
	if t == timeType {
		return decodeTime(raw, t)
	}
	if t == durationType {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return reflect.Zero(t), err
		}
		return reflect.ValueOf(time.Duration(n)), nil
	}
	if reflect.PointerTo(t).Implements(textUnmarshalerType) {
		p := reflect.New(t)
		if err := p.Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(raw)); err != nil {
			return reflect.Zero(t), err
		}
		return p.Elem(), nil
	}
	switch t.Kind() {
	case reflect.Bool:
		switch raw {
		case "1", "true", "TRUE", "True":
			return reflect.ValueOf(true).Convert(t), nil
		case "", "0", "false", "FALSE", "False":
			return reflect.ValueOf(false).Convert(t), nil
		default:
			return reflect.Zero(t), fmt.Errorf("invalid bool %q", raw)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, t.Bits())
		if err != nil {
			return reflect.Zero(t), err
		}
		return reflect.ValueOf(n).Convert(t), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(raw, 10, t.Bits())
		if err != nil {
			return reflect.Zero(t), err
		}
		return reflect.ValueOf(n).Convert(t), nil
	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(raw, t.Bits())
		if err != nil {
			return reflect.Zero(t), err
		}
		return reflect.ValueOf(n).Convert(t), nil
	case reflect.String:
		return reflect.ValueOf(raw).Convert(t), nil
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			b, err := base64.StdEncoding.DecodeString(raw)
			if err != nil {
				return reflect.Zero(t), err
			}
			out := reflect.MakeSlice(t, len(b), len(b))
			reflect.Copy(out, reflect.ValueOf(b))
			return out, nil
		}
		return unmarshalJSON(raw, t)
	case reflect.Struct, reflect.Map, reflect.Array, reflect.Interface:
		return unmarshalJSON(raw, t)
	default:
		return reflect.Zero(t), fmt.Errorf("unsupported kind %s", t.Kind())
	}
}

// decodeTime 兼容缓存写入格式与常见数据库时间字符串。
func decodeTime(raw string, t reflect.Type) (reflect.Value, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02"} {
		if tm, err := time.Parse(layout, raw); err == nil {
			return reflect.ValueOf(tm).Convert(t), nil
		}
	}
	return reflect.Zero(t), fmt.Errorf("invalid time %q", raw)
}

// unmarshalJSON 是嵌套结构/映射/切片的统一解码。
func unmarshalJSON(raw string, t reflect.Type) (reflect.Value, error) {
	p := reflect.New(t)
	if err := json.Unmarshal([]byte(raw), p.Interface()); err != nil {
		return reflect.Zero(t), err
	}
	return p.Elem(), nil
}
