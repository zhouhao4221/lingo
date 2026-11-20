package lingo

import (
	"context"
	"net/http"

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

type openAIModel struct{ c *llm.OpenAI }

func (m openAIModel) Chat(ctx context.Context, messages []Message) (string, error) {
	ms := make([]llm.Message, 0, len(messages))
	for _, x := range messages {
		ms = append(ms, llm.Message{Role: x.Role, Content: x.Content})
	}
	return m.c.Chat(ctx, ms)
}

func (m openAIModel) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return m.c.Embed(ctx, texts)
}
