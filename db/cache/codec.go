package cache

import "encoding/json"

// Codec 值序列化接口
type Codec interface {
	Marshal(v any) ([]byte, error)
	Unmarshal(data []byte, v any) error
	Name() string
}

type jsonCodec struct{}

// NewJSONCodec 标准库 JSON 编解码（默认）
func NewJSONCodec() Codec { return jsonCodec{} }

func (jsonCodec) Marshal(v any) ([]byte, error)      { return json.Marshal(v) }
func (jsonCodec) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
func (jsonCodec) Name() string                       { return "json" }
