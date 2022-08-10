package util

// CommonError 微信返回的通用错误json
type Error struct {
	ErrCode int64  `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

func (this *Error) Error() string {
	return this.ErrMsg
}
