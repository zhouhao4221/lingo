// Package textutil 是各包共用的文本小工具（按字符截断、判断字段是否有值）。
package textutil

import (
	"strings"
	"unicode/utf8"
)

// Clip 把 s 截到最多 n 个字符（按 rune）。
func Clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ClipEllipsis 同 Clip，截断时以 "…" 结尾（总长仍为 n）。
func ClipEllipsis(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// HasValue 报告表单字段取值中是否有非空白内容。
func HasValue(vs []string) bool {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// AnyValue 报告表单中是否有任一字段有值。
func AnyValue(fields map[string][]string) bool {
	for _, vs := range fields {
		if HasValue(vs) {
			return true
		}
	}
	return false
}
