package swd

import "touchgocore/go-swd/core"

// WithOptions 设置所有配置选项
func (swd *SWD) WithOptions(options *core.SWDOptions) core.SWD {
	if swd.options == nil {
		swd.options = &core.SWDOptions{}
	}
	*swd.options = *options
	if swd.detector != nil {
		swd.detector.UpdateAlgoCache()
	}
	return swd
}

// WithSkipWhitespace 设置是否忽略空白字符
func (swd *SWD) WithSkipWhitespace(skip bool) core.SWD {
	if swd.options == nil {
		swd.options = &core.SWDOptions{}
	}
	swd.options.SkipWhitespace = skip
	if swd.detector != nil {
		swd.detector.UpdateAlgoCache()
	}
	return swd
}

// WithIgnoreCase 设置是否忽略大小写
func (swd *SWD) WithIgnoreCase(ignore bool) core.SWD {
	if swd.options == nil {
		swd.options = &core.SWDOptions{}
	}
	swd.options.IgnoreCase = ignore
	if swd.detector != nil {
		swd.detector.UpdateAlgoCache()
	}
	return swd
}

// WithIgnoreWidth 设置是否忽略全角和半角字符差异
func (swd *SWD) WithIgnoreWidth(ignore bool) core.SWD {
	if swd.options == nil {
		swd.options = &core.SWDOptions{}
	}
	swd.options.IgnoreWidth = ignore
	if swd.detector != nil {
		swd.detector.UpdateAlgoCache()
	}
	return swd
}

// WithMaxDistance 设置字符间最大距离
func (swd *SWD) WithMaxDistance(distance int) core.SWD {
	if swd.options == nil {
		swd.options = &core.SWDOptions{}
	}
	swd.options.MaxDistance = distance
	if swd.detector != nil {
		swd.detector.UpdateAlgoCache()
	}
	return swd
}

// EnablePinyin 启用拼音检测
func (swd *SWD) EnablePinyin() core.SWD {
	if swd.options == nil {
		swd.options = &core.SWDOptions{}
	}
	swd.options.EnablePinyin = true
	if swd.detector != nil {
		swd.detector.UpdateAlgoCache()
	}
	return swd
}

// DisablePinyin 禁用拼音检测
func (swd *SWD) DisablePinyin() core.SWD {
	if swd.options == nil {
		swd.options = &core.SWDOptions{}
	}
	swd.options.EnablePinyin = false
	if swd.detector != nil {
		swd.detector.UpdateAlgoCache()
	}
	return swd
}

// EnableHomophone 启用同音字检测
func (swd *SWD) EnableHomophone() core.SWD {
	if swd.options == nil {
		swd.options = &core.SWDOptions{}
	}
	swd.options.EnableHomophone = true
	if swd.detector != nil {
		swd.detector.UpdateAlgoCache()
	}
	return swd
}

// DisableHomophone 禁用同音字检测
func (swd *SWD) DisableHomophone() core.SWD {
	if swd.options == nil {
		swd.options = &core.SWDOptions{}
	}
	swd.options.EnableHomophone = false
	if swd.detector != nil {
		swd.detector.UpdateAlgoCache()
	}
	return swd
}

// EnableSimilarShape 启用形近字检测
func (swd *SWD) EnableSimilarShape() core.SWD {
	if swd.options == nil {
		swd.options = &core.SWDOptions{}
	}
	swd.options.EnableSimilarShape = true
	if swd.detector != nil {
		swd.detector.UpdateAlgoCache()
	}
	return swd
}

// DisableSimilarShape 禁用形近字检测
func (swd *SWD) DisableSimilarShape() core.SWD {
	if swd.options == nil {
		swd.options = &core.SWDOptions{}
	}
	swd.options.EnableSimilarShape = false
	if swd.detector != nil {
		swd.detector.UpdateAlgoCache()
	}
	return swd
}

// WithIgnoreNumStyle 启用数字样式忽略
func (swd *SWD) WithIgnoreNumStyle(ignore bool) core.SWD {
	if swd.options == nil {
		swd.options = &core.SWDOptions{}
	}
	swd.options.IgnoreNumStyle = ignore
	if swd.detector != nil {
		swd.detector.UpdateAlgoCache()
	}
	return swd
}

// EnableZhPYMix 启用中文拼音混合检测
func (swd *SWD) EnableZhPYMix() core.SWD {
	if swd.options == nil {
		swd.options = &core.SWDOptions{}
	}
	swd.options.EnableZhPYMix = true
	if swd.detector != nil {
		swd.detector.UpdateAlgoCache()
	}
	return swd
}

// DisableZhPYMix 禁用中文拼音混合检测
func (swd *SWD) DisableZhPYMix() core.SWD {
	if swd.options == nil {
		swd.options = &core.SWDOptions{}
	}
	swd.options.EnableZhPYMix = false
	if swd.detector != nil {
		swd.detector.UpdateAlgoCache()
	}
	return swd
}

// EnableNumCheck 启用数字检测
func (swd *SWD) EnableNumCheck() core.SWD {
	if swd.options == nil {
		swd.options = &core.SWDOptions{}
	}
	swd.options.EnableNumCheck = true
	if swd.detector != nil {
		swd.detector.UpdateAlgoCache()
	}
	return swd
}

// DisableNumCheck 禁用数字检测
func (swd *SWD) DisableNumCheck() core.SWD {
	if swd.options == nil {
		swd.options = &core.SWDOptions{}
	}
	swd.options.EnableNumCheck = false
	if swd.detector != nil {
		swd.detector.UpdateAlgoCache()
	}
	return swd
}

// EnableURLCheck 启用URL检测
func (swd *SWD) EnableURLCheck() core.SWD {
	if swd.options == nil {
		swd.options = &core.SWDOptions{}
	}
	swd.options.EnableURLCheck = true
	if swd.detector != nil {
		swd.detector.UpdateAlgoCache()
	}
	return swd
}

// DisableURLCheck 禁用URL检测
func (swd *SWD) DisableURLCheck() core.SWD {
	if swd.options == nil {
		swd.options = &core.SWDOptions{}
	}
	swd.options.EnableURLCheck = false
	if swd.detector != nil {
		swd.detector.UpdateAlgoCache()
	}
	return swd
}

// EnableEmailCheck 启用Email检测
func (swd *SWD) EnableEmailCheck() core.SWD {
	if swd.options == nil {
		swd.options = &core.SWDOptions{}
	}
	swd.options.EnableEmailCheck = true
	if swd.detector != nil {
		swd.detector.UpdateAlgoCache()
	}
	return swd
}

// DisableEmailCheck 禁用Email检测
func (swd *SWD) DisableEmailCheck() core.SWD {
	if swd.options == nil {
		swd.options = &core.SWDOptions{}
	}
	swd.options.EnableEmailCheck = false
	if swd.detector != nil {
		swd.detector.UpdateAlgoCache()
	}
	return swd
}
