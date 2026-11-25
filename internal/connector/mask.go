package connector

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

const maskText = "***"

// parsePaths 解析 Sensitive 路径（"phone"、"customer.phone"、"items.*.price"）。
func parsePaths(ss []string) [][]string {
	var out [][]string
	for _, s := range ss {
		var segs []string
		for _, seg := range strings.Split(strings.TrimSpace(s), ".") {
			if seg != "" {
				segs = append(segs, seg)
			}
		}
		if len(segs) > 0 {
			out = append(out, segs)
		}
	}
	return out
}

// mask 把 v（JSON 解码后的值）中匹配路径的值替换为 ***（原地修改并返回）。"*" 匹配对象的任意键或数组的任意元素；
// 普通路径段不区分大小写，遇到数组时对每个元素生效。
func mask(v any, paths [][]string) any {
	for _, p := range paths {
		v = maskPath(v, p)
	}
	return v
}

func maskPath(v any, path []string) any {
	if len(path) == 0 {
		return maskText
	}
	switch x := v.(type) {
	case map[string]any:
		if path[0] == "*" {
			for k := range x {
				x[k] = maskPath(x[k], path[1:])
			}
		} else {
			// 默认安全：字段名不区分大小写匹配（与 Go 的 JSON 解码一致；未写 json tag 的字段编码为 "Phone" 等）
			for k := range x {
				if strings.EqualFold(k, path[0]) {
					x[k] = maskPath(x[k], path[1:])
				}
			}
		}
	case []any:
		for i := range x {
			if path[0] == "*" {
				x[i] = maskPath(x[i], path[1:])
			} else {
				x[i] = maskPath(x[i], path)
			}
		}
	}
	return v
}

// encodeResult 序列化、脱敏并截断结果：未截断时 data 为 JSON；超过 MaxResultBytes 时在字符边界截断为 text。
func encodeResult(data any, paths [][]string) (out json.RawMessage, text string, truncated bool, err error) {
	b, err := json.Marshal(data)
	if err != nil {
		return nil, "", false, err
	}
	if len(paths) > 0 {
		d := json.NewDecoder(bytes.NewReader(b))
		d.UseNumber()
		var v any
		if err := d.Decode(&v); err != nil {
			return nil, "", false, err
		}
		if b, err = json.Marshal(mask(v, paths)); err != nil {
			return nil, "", false, err
		}
	}
	if len(b) <= MaxResultBytes {
		return b, "", false, nil
	}
	cut := b[:MaxResultBytes]
	for len(cut) > 0 && !utf8.Valid(cut) {
		cut = cut[:len(cut)-1]
	}
	return nil, string(cut), true, nil
}

// safeEncode 同 encodeResult，并把编码过程中的 panic（如数据的 MarshalJSON 出错）转为错误；错误内容不外传。
func safeEncode(data any, paths [][]string) (out json.RawMessage, text string, truncated bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, text, truncated, err = nil, "", false, errEncode
		}
	}()
	out, text, truncated, err = encodeResult(data, paths)
	if err != nil {
		err = errEncode // 不保留原错误：其中可能含数据内容
	}
	return out, text, truncated, err
}

var errEncode = errors.New("result encoding failed")

// 参数摘要的限制。
const (
	maxArgs      = 20
	argNameRunes = 64
	argValRunes  = 32
)

// summarize 返回参数名（排序）与截断后的值；m 应已脱敏。
func summarize(m map[string]any) ([]string, map[string]string) {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sortStrings(names)
	if len(names) > maxArgs {
		names = names[:maxArgs]
	}
	vals := make(map[string]string, len(names))
	for i, k := range names {
		v := m[k]
		var s string
		if str, ok := v.(string); ok {
			s = str
		} else {
			b, _ := json.Marshal(v)
			s = string(b)
		}
		short := clip(k, argNameRunes)
		names[i] = short
		vals[short] = clip(s, argValRunes)
	}
	return names, vals
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func sortStrings(ss []string) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j] < ss[j-1]; j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
}
