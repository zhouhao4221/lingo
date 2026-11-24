package datatool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// schema 是 InputSchema 中参与校验的部分：required 与 properties 的基本类型（不是完整的 JSON Schema 校验）。
type schema struct {
	props    map[string]prop
	required []string
}

type prop struct {
	types []string // 为空表示不限类型
	desc  string
}

// InputError 表示参数不合法。错误信息只含参数名、期望类型与参数说明（来自 InputSchema），不含参数值。
type InputError struct {
	Param string // 为空表示参数整体不是 JSON 对象
	Want  string // "required" 或期望类型
	Desc  string // 参数说明（InputSchema 中的 description）
}

func (e *InputError) Error() string {
	switch {
	case e.Param == "":
		return "input must be a JSON object"
	case e.Want == "required":
		return fmt.Sprintf("parameter %s is required", e.Param)
	}
	return fmt.Sprintf("parameter %s must be %s", e.Param, e.Want)
}

func parseSchema(raw json.RawMessage) (*schema, error) {
	var s struct {
		Properties map[string]struct {
			Type        json.RawMessage `json:"type"`
			Description string          `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '{' {
		return nil, errors.New("schema must be a JSON object")
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	out := &schema{props: map[string]prop{}, required: s.Required}
	for name, p := range s.Properties {
		var pr prop
		pr.desc = p.Description
		if len(p.Type) > 0 {
			var one string
			if json.Unmarshal(p.Type, &one) == nil {
				pr.types = []string{one}
			} else if err := json.Unmarshal(p.Type, &pr.types); err != nil {
				return nil, err
			}
		}
		out.props[name] = pr
	}
	return out, nil
}

// normalize 把空输入视为 {}。
func normalize(input json.RawMessage) json.RawMessage {
	if t := bytes.TrimSpace(input); len(t) == 0 || string(t) == "null" {
		return json.RawMessage("{}")
	}
	return input
}

// decodeObject 把输入解码为对象（数字保留为 json.Number）；不是对象时返回 nil。
func decodeObject(input json.RawMessage) map[string]any {
	d := json.NewDecoder(bytes.NewReader(normalize(input)))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil || m == nil {
		return nil
	}
	return m
}

// validate 校验必填参数与基本类型。
func (s *schema) validate(m map[string]any) error {
	if m == nil {
		return &InputError{}
	}
	for _, name := range s.required {
		if v, ok := m[name]; !ok || v == nil {
			return &InputError{Param: name, Want: "required", Desc: s.props[name].desc}
		}
	}
	for name, p := range s.props {
		v, ok := m[name]
		if !ok || len(p.types) == 0 {
			continue
		}
		match := false
		for _, t := range p.types {
			if typeOK(t, v) {
				match = true
				break
			}
		}
		if !match {
			want := p.types[0]
			if len(p.types) > 1 {
				want = fmt.Sprint(p.types)
			}
			return &InputError{Param: name, Want: want, Desc: p.desc}
		}
	}
	return nil
}

func typeOK(t string, v any) bool {
	switch t {
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		_, err := n.Int64()
		return err == nil
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "null":
		return v == nil
	}
	return true // 其他类型不校验
}
