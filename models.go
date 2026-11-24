package lingo

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/zhouhao4221/lingo/internal/llm"
)

// 常用服务的接口地址。
const (
	OpenAIBaseURL   = "https://api.openai.com/v1"
	DeepSeekBaseURL = "https://api.deepseek.com"
)

// OpenAIConfig 配置 OpenAI 兼容接口（Chat Completions 与 Embeddings）：OpenAI、DeepSeek，以及兼容该协议的网关、
// 云服务与本地推理服务（如 vLLM、Ollama 的 /v1）。只用标准库 HTTP 调用，不引入 SDK。
type OpenAIConfig struct {
	BaseURL        string // 含版本路径，如 "https://api.openai.com/v1"；为空取 OpenAIBaseURL
	APIKey         string // 以 Authorization: Bearer 发送；本地服务可空
	Model          string // 对话模型，必填
	EmbeddingModel string // 向量模型；为空时 Embed 返回错误（对话类功能不受影响）
	Temperature    *float64
	Headers        map[string]string // 额外请求头
	HTTPClient     *http.Client      // 为空时用 90 秒超时的默认客户端
}

// NewOpenAIModel 返回基于 OpenAI 兼容接口的 Model，可用作 Config.Model 或 Config.LightModel。
// 429、5xx 与网络错误自动重试两次（遵循 Retry-After）。
func NewOpenAIModel(c OpenAIConfig) (Model, error) {
	if c.BaseURL == "" {
		c.BaseURL = OpenAIBaseURL
	}
	cl, err := llm.NewOpenAI(llm.OpenAIOptions{BaseURL: c.BaseURL, APIKey: c.APIKey, Model: c.Model, EmbeddingModel: c.EmbeddingModel,
		Temperature: c.Temperature, Headers: c.Headers, HTTPClient: c.HTTPClient})
	if err != nil {
		return nil, err
	}
	return openAIModel{cl}, nil
}

// NewDeepSeekModel 返回 DeepSeek 的 Model（OpenAI 兼容接口，地址 DeepSeekBaseURL）；model 为空取 "deepseek-chat"。
// DeepSeek 不提供向量接口，Embed 返回错误；需要向量时用 NewOpenAIModel 另配向量服务。
func NewDeepSeekModel(apiKey, model string) (Model, error) {
	if model == "" {
		model = "deepseek-chat"
	}
	return NewOpenAIModel(OpenAIConfig{BaseURL: DeepSeekBaseURL, APIKey: apiKey, Model: model})
}

// Usage 是一次模型调用消耗的 token。Cached 是输入中命中服务端前缀缓存的部分。
type Usage struct {
	Model  string
	Input  int
	Output int
	Cached int
}

// UsageModel 是能报告 token 用量的 Model（可选）：实现它的模型，其用量会记入用量统计（LLMUsage）。
// 内置的 NewOpenAIModel / NewDeepSeekModel 已实现。
type UsageModel interface {
	Model
	ChatUsage(ctx context.Context, messages []Message) (string, Usage, error)
}

// StreamModel 是能流式输出的 Model（可选）：实现它的模型，知识问答的回答逐片段推送（REST 的 SSE、
// SendMessageStream 的回调）；未实现时整段生成后一次推送。内置的 NewOpenAIModel / NewDeepSeekModel 已实现
// （注意 OpenAIConfig.HTTPClient 的超时是整个回答的时限，默认 90 秒）。
type StreamModel interface {
	Model
	// ChatStream 逐片段调用 onDelta，返回的全文为各片段拼接；ctx 取消时应尽快返回。不报告用量时返回零值 Usage。
	ChatStream(ctx context.Context, messages []Message, onDelta func(delta string)) (string, Usage, error)
}

// LLMUsageStat 是按天（UTC）、用途、模型汇总的模型用量。CacheHitRate = Cached / Input。
type LLMUsageStat struct {
	Day time.Time `json:"day"`
	// Kind 是用途：draft_issue（建单整理）、assistant_intent（问答意图解析）、assistant_confirm（问答确认判断）、
	// assistant_answer（问答组织回答）
	Kind         string  `json:"kind"`
	Model        string  `json:"model"`
	Calls        int     `json:"calls"`
	Failed       int     `json:"failed"`
	Input        int64   `json:"input_tokens"`
	Output       int64   `json:"output_tokens"`
	Cached       int64   `json:"cached_tokens"`
	CacheHitRate float64 `json:"cache_hit_rate"`
	AvgLatencyMS int64   `json:"avg_latency_ms"`
}

// LLMUsage 返回 [from, to) 内的模型用量汇总（仅 admin）；零值 from / to 表示最近 30 天。
// 只有实现了 UsageModel 的模型（含内置适配器）有 token 数，其余只计调用次数。
func (l *Lingo) LLMUsage(ctx context.Context, u User, from, to time.Time) ([]LLMUsageStat, error) {
	if u.Role != RoleAdmin {
		return nil, ErrForbidden
	}
	if to.IsZero() {
		to = time.Now()
	}
	if from.IsZero() {
		from = to.AddDate(0, 0, -30)
	}
	if !from.Before(to) {
		return nil, fmt.Errorf("%w: from must be before to", ErrInvalidInput)
	}
	rows, err := l.store.LLMUsageSummary(ctx, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]LLMUsageStat, 0, len(rows))
	for _, r := range rows {
		s := LLMUsageStat{Day: r.Day, Kind: r.Kind, Model: r.Model, Calls: r.Calls, Failed: r.Failed, Input: r.Input,
			Output: r.Output, Cached: r.Cached, AvgLatencyMS: r.AvgMilli}
		if r.Input > 0 {
			s.CacheHitRate = float64(r.Cached) / float64(r.Input)
		}
		out = append(out, s)
	}
	return out, nil
}

type openAIModel struct{ c *llm.OpenAI }

func (m openAIModel) ChatUsage(ctx context.Context, messages []Message) (string, Usage, error) {
	ms := make([]llm.Message, 0, len(messages))
	for _, x := range messages {
		ms = append(ms, llm.Message{Role: x.Role, Content: x.Content})
	}
	s, u, err := m.c.ChatUsage(ctx, ms)
	return s, Usage(u), err
}

func (m openAIModel) ChatStream(ctx context.Context, messages []Message, onDelta func(string)) (string, Usage, error) {
	s, u, err := m.c.ChatStream(ctx, llmMessages(messages), onDelta)
	return s, Usage(u), err
}

func llmMessages(messages []Message) []llm.Message {
	ms := make([]llm.Message, 0, len(messages))
	for _, x := range messages {
		ms = append(ms, llm.Message{Role: x.Role, Content: x.Content})
	}
	return ms
}

func (m openAIModel) Chat(ctx context.Context, messages []Message) (string, error) {
	s, _, err := m.ChatUsage(ctx, messages)
	return s, err
}

func (m openAIModel) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return m.c.Embed(ctx, texts)
}

// EmbeddingModelName 实现 EmbeddingModelNamer。
func (m openAIModel) EmbeddingModelName() string { return m.c.EmbeddingModelName() }
