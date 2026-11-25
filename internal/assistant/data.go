package assistant

import (
	"context"
	"errors"
	"strings"

	"github.com/zhouhao4221/lingo/internal/connector"
	"github.com/zhouhao4221/lingo/internal/textutil"
)

// 业务数据问答的固定回复（REQ-008）：不调用模型，不说明工具或数据是否存在。
const (
	textDataNotFound    = "没有查到相关的业务数据。"
	textDataUnavailable = "该查询暂时不可用，请稍后再试。"
	textDataFailed      = "查询业务数据失败，请稍后再试。"
)

// factData 是交给模型的业务数据：result 是脱敏、截断后的 JSON 文本，只用于本次回答，不落库。
type factData struct {
	Tool        string `json:"tool"`
	Description string `json:"description,omitempty"`
	Result      string `json:"result"`
	Truncated   bool   `json:"truncated,omitempty"`
}

// intentTools 返回交给意图解析的工具：最多 MaxIntentTools 个，描述截断到 300 字。没有接入业务数据工具时为空。
func (s *Service) intentTools(ctx context.Context) []ToolInfo {
	if s.o.DataTools == nil {
		return nil
	}
	ts := s.o.DataTools.Visible(ctx)
	if len(ts) > MaxIntentTools {
		ts = ts[:MaxIntentTools]
	}
	out := make([]ToolInfo, 0, len(ts))
	for _, t := range ts {
		t.Description = textutil.ClipEllipsis(oneLine(t.Description), toolDescRunes)
		out = append(out, t)
	}
	return out
}

// queryData 调用意图选定的工具并据结果作答；一次提问最多调用一个工具。原始结果不进入会话、知识库、审计与日志，
// 引用只记工具名。
func (s *Service) queryData(ctx context.Context, tc *turnCtx, in *Intent) (*reply, error) {
	offered := false
	for _, t := range tc.tools {
		offered = offered || t.Name == in.Tool
	}
	if s.o.DataTools == nil || in.Tool == "" || !offered {
		return &reply{content: textClarify, outcome: OutcomeClarify}, nil
	}
	r, err := s.o.DataTools.Run(ctx, in.Tool, in.ToolInput, tc.convID)
	var ie *connector.InputError
	switch {
	case errors.As(err, &ie):
		return &reply{content: needInputText(ie), outcome: OutcomeClarify}, nil
	case errors.Is(err, connector.ErrNotFound):
		return &reply{content: textDataNotFound, outcome: OutcomeNotFound}, nil
	case errors.Is(err, connector.ErrUnavailable):
		return &reply{content: textDataUnavailable, outcome: OutcomeDataFailed}, nil
	case errors.Is(err, connector.ErrFailed):
		return &reply{content: textDataFailed, outcome: OutcomeDataFailed}, nil
	case err != nil:
		return nil, err
	}
	result := string(r.Data)
	if r.Truncated {
		result = r.Text
	}
	f := facts{Kind: "data", Total: 1, Shown: 1, Data: &factData{Tool: r.Tool,
		Description: textutil.ClipEllipsis(oneLine(r.Description), toolDescRunes), Result: result, Truncated: r.Truncated}}
	cites := Citations{Tools: []ToolRef{{Name: r.Tool}}}
	return s.answer(ctx, tc, f, cites, func() string { return textDataFailed })
}

// needInputText 是参数不合法时的追问：缺少参数时请用户补充；类型不符时明确说明格式不对（不做静默转换）。
func needInputText(ie *connector.InputError) string {
	if ie.Param == "" {
		return "查询需要的信息不完整，请补充后再问一次。"
	}
	what := ie.Param
	if d := strings.TrimSpace(oneLine(ie.Desc)); d != "" {
		what = textutil.Clip(d, 50) + "（" + ie.Param + "）"
	}
	if ie.Want == "required" {
		return "查询需要" + what + "，请补充后再问一次。"
	}
	want, ok := typeNamesZh[ie.Want]
	if !ok {
		want = "正确的格式"
	}
	return "查询参数" + what + "的格式不对，应为" + want + "。请换个说法或直接给出该值后再问一次。"
}

var typeNamesZh = map[string]string{"string": "文本", "number": "数字", "integer": "整数", "boolean": "是或否",
	"object": "对象", "array": "列表", "null": "空值"}
