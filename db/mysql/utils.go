package mysql

import "fmt"

// safeSQLColumn 校验列名合法性（保留兼容旧 API）
func safeSQLColumn(column string) error {
	if column == "" {
		return fmt.Errorf("invalid column name")
	}
	for i := 0; i < len(column); i++ {
		c := column[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '.' {
			continue
		}
		return fmt.Errorf("invalid column name: %s", column)
	}
	return nil
}