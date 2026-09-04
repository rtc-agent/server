package model

// strPtr 将非空字符串转为 *string，空字符串返回 nil。
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
