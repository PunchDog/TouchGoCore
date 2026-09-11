package category

import "testing"

func TestCategory_Contains(t *testing.T) {
	tests := []struct {
		name     string
		category Category
		other    Category
		want     bool
	}{
		// 基本分类测试
		{
			name:     "单个分类-相同分类",
			category: Pornography,
			other:    Pornography,
			want:     true,
		},
		{
			name:     "单个分类-不同分类",
			category: Pornography,
			other:    Political,
			want:     false,
		},
		{
			name:     "组合分类-包含其中一个",
			category: Pornography | Political,
			other:    Political,
			want:     true,
		},
		{
			name:     "组合分类-都不包含",
			category: Pornography | Political,
			other:    Violence,
			want:     false,
		},
		{
			name:     "None分类-检测其他分类",
			category: None,
			other:    Pornography,
			want:     false,
		},
		{
			name:     "None分类-检测None",
			category: None,
			other:    None,
			want:     true,
		},

		// All分类测试
		{
			name:     "All-检测单个分类",
			category: All,
			other:    Pornography,
			want:     true,
		},
		{
			name:     "All-检测组合分类",
			category: All,
			other:    Pornography | Political,
			want:     true,
		},
		{
			name:     "All-检测All",
			category: All,
			other:    All,
			want:     true,
		},
		{
			name:     "All-检测None",
			category: All,
			other:    None,
			want:     false,
		},
		{
			name:     "单个分类-检测All",
			category: Pornography,
			other:    All,
			want:     false,
		},
		{
			name:     "组合分类-检测All(不完整)",
			category: Pornography | Political | Violence,
			other:    All,
			want:     false,
		},
		{
			name:     "组合分类-检测All(完整)",
			category: All,
			other:    All,
			want:     true,
		},

		// 边界情况
		{
			name:     "无效分类-检测有效分类",
			category: Category(1<<63 - 1),
			other:    Pornography,
			want:     true, // 由于位运算特性，包含该位
		},
		{
			name:     "有效分类-检测无效分类",
			category: Pornography,
			other:    Category(1<<63 - 1),
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.category.Contains(tt.other); got != tt.want {
				t.Errorf("Category.Contains() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCategory_Contains_Comprehensive 全面测试所有预定义分类的组合情况
func TestCategory_Contains_Comprehensive(t *testing.T) {
	// 所有预定义分类
	categories := []Category{
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

	// 测试每个分类与All的关系
	for _, cat := range categories {
		t.Run("Single_Category_"+cat.String(), func(t *testing.T) {
			// 单个分类不应该包含All
			if cat.Contains(All) {
				t.Errorf("Single category %v should not contain All", cat)
			}
			// All应该包含所有单个分类
			if !All.Contains(cat) {
				t.Errorf("All should contain category %v", cat)
			}
		})
	}

	// 测试All与自身的关系
	t.Run("All_Contains_All", func(t *testing.T) {
		if !All.Contains(All) {
			t.Error("All should contain itself")
		}
	})

	// 测试组合分类
	t.Run("Combined_Categories", func(t *testing.T) {
		// 创建一个不完整的组合分类
		partial := Pornography | Political | Violence
		if partial.Contains(All) {
			t.Error("Partial combination should not contain All")
		}

		// 创建一个完整的组合分类
		complete := Category(0)
		for _, cat := range categories {
			complete |= cat
		}
		if !complete.Contains(All) {
			t.Error("Complete combination should contain All")
		}
		if complete != All {
			t.Error("Complete combination should equal All")
		}
	})
}
