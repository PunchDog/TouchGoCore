package config

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// tagIndex 收集一个结构体类型下所有合法 json 键名。
func tagIndex(t reflect.Type) map[string]bool {
	if t.Kind() != reflect.Struct {
		return nil
	}
	out := make(map[string]bool, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			name = f.Name
		}
		out[name] = true
	}
	return out
}

// isDocKey 示例配置用「xxx说明」「说明:」这类伴生键写注释，不参与绑定。
func isDocKey(k string) bool {
	t := strings.TrimRight(k, " :：,.，。")
	return t == "说明" || strings.HasSuffix(t, "说明")
}

// derefNode 把指针/切片/数组归一到元素类型，使数组元素里的对象同样接受键名校验。
func derefNode(t reflect.Type) reflect.Type {
	for t != nil {
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			t = t.Elem()
		default:
			return t
		}
	}
	return t
}

// fieldOf 按 json 键名取字段类型。
func fieldOf(t reflect.Type, key string) reflect.Type {
	if t.Kind() != reflect.Struct {
		return nil
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			name = f.Name
		}
		if name == key {
			return f.Type
		}
	}
	return nil
}

// jsonObjects 把任意 JSON 值归一成「其中的对象」：对象返回自身，数组逐元素展开。
func jsonObjects(raw json.RawMessage) []map[string]json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil && len(obj) > 0 {
		return []map[string]json.RawMessage{obj}
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		out := make([]map[string]json.RawMessage, 0, len(arr))
		for _, el := range arr {
			out = append(out, jsonObjects(el)...)
		}
		return out
	}
	return nil
}

func readExampleJson(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("example.json")
	if err != nil {
		t.Fatalf("读取 example.json 失败: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("example.json 不是合法 JSON: %v", err)
	}
	return doc
}

// TestExampleJsonKeysBindToStructFields 示例配置里的每个业务键都必须能被结构体接收到。
//
// encoding/json 对未知键静默丢弃：键名写错的配置项看着生效、实则永远走默认值，
// 运行期没有任何痕迹（阶段1 的 S13 就是同名键分裂）。
func TestExampleJsonKeysBindToStructFields(t *testing.T) {
	doc := readExampleJson(t)

	var check func(node reflect.Type, obj map[string]json.RawMessage, path string)
	check = func(node reflect.Type, obj map[string]json.RawMessage, path string) {
		node = derefNode(node)
		keys := tagIndex(node)
		for k, rawValue := range obj {
			if isDocKey(k) {
				continue
			}
			if keys != nil && !keys[k] {
				t.Errorf("%s.%s 在 Cfg 结构体上没有对应的 json 键，配置会被静默丢弃", path, k)
				continue
			}
			child := derefNode(fieldOf(node, k))
			if child == nil {
				continue
			}
			for _, sub := range jsonObjects(rawValue) {
				check(child, sub, path+"."+k)
			}
		}
	}
	check(reflect.TypeOf(Cfg{}), doc, "example.json")
}

// TestExampleJsonUsesCurrentRpcKey 示例配置不得再教用户写已废弃的 rpc_port。
// 废弃键一旦出现在示例里，新用户会以为它是首选写法，而两套键并存时只有一套生效。
func TestExampleJsonUsesCurrentRpcKey(t *testing.T) {
	doc := readExampleJson(t)
	if _, ok := doc["rpc_port"]; ok {
		t.Error(`example.json 仍使用已废弃的 "rpc_port"，应改为 "rpc"`)
	}
	if _, ok := doc["rpc"]; !ok {
		t.Error(`example.json 缺少 "rpc" 配置块`)
	}
}

// TestRpcAddrNamePresentInExample RPC 客户端/服务端以 name 作为注册表键，
// 缺 name 会让 RemoveRpcClient 无从寻址，示例必须给出可直接使用的形态。
func TestRpcAddrNamePresentInExample(t *testing.T) {
	raw, err := os.ReadFile("example.json")
	if err != nil {
		t.Fatalf("读取 example.json 失败: %v", err)
	}
	var cfg Cfg
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("example.json 反序列化失败: %v", err)
	}
	if cfg.RpcOf() == nil {
		t.Fatal("example.json 未提供任何 RPC 配置")
	}
	groups := map[string][]*RpcAddr{"server": cfg.RpcOf().Server, "client": cfg.RpcOf().Client}
	for group, list := range groups {
		if len(list) == 0 {
			t.Errorf("rpc.%s 为空，示例应给出可用条目", group)
			continue
		}
		for i, addr := range list {
			if addr == nil || strings.TrimSpace(addr.Name) == "" {
				t.Errorf("rpc.%s[%d] 缺少 name，注册表无法按名字寻址", group, i)
			}
		}
	}
}
