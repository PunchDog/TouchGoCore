package mysql

import "unicode"

// toSnakeCase 将 CamelCase 转为 snake_case
func toSnakeCase(s string) string {
	var b []rune
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b = append(b, '_')
			}
			b = append(b, unicode.ToLower(r))
		} else {
			b = append(b, r)
		}
	}
	return string(b)
}