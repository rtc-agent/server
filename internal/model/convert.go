package model

// StrPtr 将非空字符串转为 *string，空字符串返回 nil。
func StrPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// DerefStr 解引用 *string，nil 返回空字符串。
func DerefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// strPtr 是 StrPtr 的别名，保留供包内使用。
func strPtr(s string) *string { return StrPtr(s) }
