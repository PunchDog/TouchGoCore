package category

// Category 敏感词分类
type Category int

const (
	// None 未分类
	None Category = 0
	// Pornography 涉黄
	Pornography Category = 1 << iota
	// Political 涉政
	Political
	// Violence 暴力
	Violence
	// Gambling 赌博
	Gambling
	// Drugs 毒品
	Drugs
	// Profanity 脏话
	Profanity
	// Discrimination 歧视
	Discrimination
	// Scam 诈骗
	Scam
	// Custom 自定义
	Custom
)

// String 返回分类的字符串表示（中文）
func (c Category) String() string {
	switch c {
	case None:
		return "未分类"
	case Pornography:
		return "涉黄"
	case Political:
		return "涉政"
	case Violence:
		return "暴力"
	case Gambling:
		return "赌博"
	case Drugs:
		return "毒品"
	case Profanity:
		return "脏话"
	case Discrimination:
		return "歧视"
	case Scam:
		return "诈骗"
	case Custom:
		return "自定义"
	default:
		return "未知分类"
	}
}

// EnglishString 返回分类的英文名称
func (c Category) EnglishString() string {
	switch c {
	case None:
		return "None"
	case Pornography:
		return "Pornography"
	case Political:
		return "Political"
	case Violence:
		return "Violence"
	case Gambling:
		return "Gambling"
	case Drugs:
		return "Drugs"
	case Profanity:
		return "Profanity"
	case Discrimination:
		return "Discrimination"
	case Scam:
		return "Scam"
	case Custom:
		return "Custom"
	default:
		return "Unknown"
	}
}

// Contains 检查当前分类是否包含指定分类
func (c Category) Contains(other Category) bool {
	// 处理None分类的特殊情况
	if other == None {
		return c == None
	}

	// 处理All分类的情况
	if other == All {
		return (c & All) == All
	}

	// 验证other分类的有效性
	if !other.IsValid() {
		return false
	}

	return c&other != 0
}

// All 所有预定义分类
var All = Pornography | Political | Violence | Gambling | Drugs | Profanity | Discrimination | Scam | Custom

// IsValid 检查分类是否有效
func (c Category) IsValid() bool {
	// None 分类是有效的
	if c == None {
		return true
	}

	// 检查是否是预定义的分类
	validCategories := []Category{
		Pornography,
		Political,
		Violence,
		Gambling,
		Drugs,
		Profanity,
		Discrimination,
		Scam,
		Custom,
	}

	for _, validCat := range validCategories {
		if c == validCat {
			return true
		}
	}

	// 检查是否是组合分类
	return (c & All) == c
}
