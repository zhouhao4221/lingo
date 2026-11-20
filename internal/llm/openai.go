package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// OpenAIOptions 配置 OpenAI Chat Completions / Embeddings 兼容接口（OpenAI、DeepSeek、各类兼容网关与本地推理服务）。
type OpenAIOptions struct {
	BaseURL        string // 含版本路径，如 https://api.openai.com/v1；不含 /chat/completions
	APIKey         string // Authorization: Bearer；可空（本地服务）
	Model          string // 对话模型
	EmbeddingModel string // 向量模型；为空时 Embed 返回 ErrNoEmbedding
	Temperature    *float64
	Headers        map[string]string // 额外请求头（如网关要求的租户头）
	HTTPClient     *http.Client      // 为空时用 90 秒超时的默认客户端
	MaxRetries     int               // 429 / 5xx / 网络错误的重试次数，默认 2
}

// ErrNoEmbedding 表示未配置向量模型。
var ErrNoEmbedding = errors.New("llm: embedding model not configured")

// OpenAI 是 OpenAI 兼容接口的客户端，可并发使用。
type OpenAI struct {
	o OpenAIOptions
}

// NewOpenAI 校验配置并创建客户端。
func NewOpenAI(o OpenAIOptions) (*OpenAI, error) {
	o.BaseURL = strings.TrimRight(strings.TrimSpace(o.BaseURL), "/")
	switch {
	case o.BaseURL == "":
		return nil, errors.New("llm: BaseURL is required")
	case !strings.HasPrefix(o.BaseURL, "http://") && !strings.HasPrefix(o.BaseURL, "https://"):
		return nil, fmt.Errorf("llm: invalid BaseURL %q", o.BaseURL)
	case o.Model == "":
		return nil, errors.New("llm: Model is required")
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 90 * time.Second}
	}
	if o.MaxRetries == 0 {
		o.MaxRetries = 2
	}
	return &OpenAI{o: o}, nil
}

// APIError 是接口返回的非 2xx 响应。
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string { return fmt.Sprintf("llm: HTTP %d: %s", e.StatusCode, e.Message) }

// Chat 调用 /chat/completions，返回第一个候选的文本。
func (c *OpenAI) Chat(ctx context.Context, messages []Message) (string, error) {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	req := struct {
		Model       string   `json:"model"`
		Messages    []msg    `json:"messages"`
		Temperature *float64 `json:"temperature,omitempty"`
		Stream      bool     `json:"stream"`
	}{Model: c.o.Model, Temperature: c.o.Temperature}
	for _, m := range messages {
		req.Messages = append(req.Messages, msg{m.Role, m.Content})
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := c.post(ctx, "/chat/completions", req, &resp); err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("llm: empty choices")
	}
	return resp.Choices[0].Message.Content, nil
}

// Embed 调用 /embeddings，按输入顺序返回向量。
func (c *OpenAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if c.o.EmbeddingModel == "" {
		return nil, ErrNoEmbedding
	}
	if len(texts) == 0 {
		return nil, nil
	}
	req := struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}{c.o.EmbeddingModel, texts}
	var resp struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := c.post(ctx, "/embeddings", req, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data) != len(texts) {
		return nil, fmt.Errorf("llm: got %d embeddings for %d inputs", len(resp.Data), len(texts))
	}
	sort.Slice(resp.Data, func(i, j int) bool { return resp.Data[i].Index < resp.Data[j].Index })
	out := make([][]float32, len(resp.Data))
	for i, d := range resp.Data {
		out[i] = d.Embedding
	}
	return out, nil
}

func (c *OpenAI) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	var last error
	for attempt := 0; attempt <= c.o.MaxRetries; attempt++ {
		if attempt > 0 {
			wait := time.Duration(attempt) * time.Second
			if ra, ok := last.(*retryAfter); ok && ra.d > 0 {
				wait = min(ra.d, 30*time.Second)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		err := c.do(ctx, path, body, out)
		var ra *retryAfter
		if !errors.As(err, &ra) {
			return err
		}
		last = ra
	}
	return last.(*retryAfter).err
}

// retryAfter 包装可重试的错误（429 / 5xx / 网络错误）。
type retryAfter struct {
	err error
	d   time.Duration
}

func (r *retryAfter) Error() string { return r.err.Error() }
func (r *retryAfter) Unwrap() error { return r.err }

func (c *OpenAI) do(ctx context.Context, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.o.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.o.APIKey)
	}
	for k, v := range c.o.Headers {
		req.Header.Set(k, v)
	}
	resp, err := c.o.HTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &retryAfter{err: fmt.Errorf("llm: %w", err)}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return &retryAfter{err: fmt.Errorf("llm: read response: %w", err)}
	}
	if resp.StatusCode/100 != 2 {
		ae := &APIError{StatusCode: resp.StatusCode, Message: errorMessage(data)}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			secs, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			return &retryAfter{err: ae, d: time.Duration(secs) * time.Second}
		}
		return ae
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("llm: decode response: %w", err)
	}
	return nil
}

// errorMessage 取 {"error":{"message":...}} 中的说明，否则取响应开头。
func errorMessage(data []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	s := strings.TrimSpace(string(data))
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
