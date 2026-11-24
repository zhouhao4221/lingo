package knowledge

import (
	"regexp"
	"strings"
	"unicode"
)

// MaxQueryTokens 是一次检索最多使用的词数。
const MaxQueryTokens = 32

func cjk(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

// Tokens 把文本切成检索用的词：中日韩文字按二元切词（单字成词），其他字母与数字连续成词，统一小写；去重，保持顺序。
func Tokens(s string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(t string) {
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	var run []rune
	kind := 0 // 0 无，1 中日韩，2 字母数字
	flush := func() {
		switch {
		case kind == 1 && len(run) == 1:
			add(string(run))
		case kind == 1:
			for i := 0; i+1 < len(run); i++ {
				add(string(run[i : i+2]))
			}
		case kind == 2:
			add(string(run))
		}
		run, kind = run[:0], 0
	}
	for _, r := range strings.ToLower(s) {
		k := 0
		switch {
		case cjk(r):
			k = 1
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			k = 2
		}
		if k != kind {
			flush()
		}
		if k != 0 {
			run, kind = append(run, r), k
		}
	}
	flush()
	return out
}

// words 返回关键词兜底用的片段：按空白与标点切开、至少 2 个字符的片段，最多 5 个。
func words(s string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return !(cjk(r) || unicode.IsLetter(r) || unicode.IsDigit(r)) }) {
		if len([]rune(w)) >= 2 && len(out) < 5 {
			out = append(out, w)
		}
	}
	return out
}

var (
	emailRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	phoneRe = regexp.MustCompile(`(?:\+?86[\s\-]?)?1[3-9]\d[\s\-]?\d{4}[\s\-]?\d{4}`)
)

// Scrub 清理文本中的邮箱与手机号（入库前兜底；改写为通用内容由后台人员负责）。
func Scrub(s string) string {
	s = emailRe.ReplaceAllString(s, "[邮箱]")
	return phoneRe.ReplaceAllString(s, "[手机号]")
}
