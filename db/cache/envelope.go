package cache

// envelope Redis 中存储的值封装：逻辑过期 + 空值标记 + 写线序号
type envelope[V any] struct {
	Value V     `json:"v"`
	ExpMS int64 `json:"e,omitempty"` // 逻辑过期时间（unix ms）；0=不逻辑过期
	Null  bool  `json:"n,omitempty"` // 空值标记：源确认不存在，防穿透
	Seq   int64 `json:"s,omitempty"` // 同源单调序号，诊断/竞态用
}

// stale 逻辑是否已过期（物理 TTL 由 Redis 自己管）
func (e *envelope[V]) stale(nowMS int64) bool {
	return e.ExpMS > 0 && nowMS >= e.ExpMS
}

func encode[V any](c Codec, e *envelope[V]) (string, error) {
	b, err := c.Marshal(e)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decode[V any](c Codec, s string) (*envelope[V], error) {
	var e envelope[V]
	if err := c.Unmarshal([]byte(s), &e); err != nil {
		return nil, err
	}
	return &e, nil
}
