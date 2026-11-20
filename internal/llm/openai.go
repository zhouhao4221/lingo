package llm

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/textutil"
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
	MaxRetries     int               // 429 / 5xx / 网络错误的重试次数：0 取默认 2，负数不重试
}

// ErrNoEmbedding 表示未配置向量模型。
var ErrNoEmbedding = errors.New("llm: embedding model not configured")

// OpenAI 是 OpenAI 兼容接口的客户端，可并发使用。
type OpenAI struct {
	o       OpenAIOptions
	retries int
}

// maxRetryWait 是愿意等待的 Retry-After 上限，服务端要求更久时不再重试。
const maxRetryWait = 30 * time.Second

// ErrTruncated 表示模型因长度上限截断了回答；ErrEmptyContent 表示回答没有文本（如被内容过滤或返回工具调用）。
var (
	ErrTruncated    = errors.New("llm: response truncated (finish_reason=length)")
	ErrEmptyContent = errors.New("llm: response has no text content")
)

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
	retries := o.MaxRetries
	switch {
	case retries == 0:
		retries = 2
	case retries < 0:
		retries = 0
	}
	return &OpenAI{o: o, retries: retries}, nil
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
	ch := resp.Choices[0]
	switch {
	case ch.FinishReason == "length":
		return "", ErrTruncated
	case strings.TrimSpace(ch.Message.Content) == "":
		return "", ErrEmptyContent
	}
	return ch.Message.Content, nil
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
	// index 须恰好是 0..n-1：缺失或重复时无法确定向量对应哪段输入
	out := make([][]float32, len(texts))
	for _, d := range resp.Data {
		if d.Index < 0 || d.Index >= len(out) || out[d.Index] != nil || len(d.Embedding) == 0 {
			return nil, fmt.Errorf("llm: invalid embedding index %d", d.Index)
		}
		out[d.Index] = d.Embedding
	}
	return out, nil
}

func (c *OpenAI) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		err := c.do(ctx, path, body, out)
		var ra *retryAfter
		if !errors.As(err, &ra) {
			return err
		}
		wait := time.Duration(attempt+1) * time.Second
		if ra.d > 0 {
			wait = ra.d
		}
		if attempt >= c.retries || wait > maxRetryWait {
			return ra.err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
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
		if !transient(err) {
			return fmt.Errorf("llm: %w", err) // 证书、域名不存在等配置问题，重试无用
		}
		return &retryAfter{err: fmt.Errorf("llm: %w", err)}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &retryAfter{err: fmt.Errorf("llm: read response: %w", err)}
	}
	if resp.StatusCode/100 != 2 {
		ae := &APIError{StatusCode: resp.StatusCode, Message: errorMessage(data)}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return &retryAfter{err: ae, d: retryAfterHeader(resp.Header.Get("Retry-After"))}
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
	return textutil.Clip(strings.TrimSpace(string(data)), 300)
}

// retryAfterHeader 解析 Retry-After：秒数或 HTTP 日期。
func retryAfterHeader(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0)
	}
	return 0
}

// transient 报告传输错误是否值得重试：证书错误、域名不存在属于配置问题，不重试。
func transient(err error) bool {
	var (
		unknownCA x509.UnknownAuthorityError
		hostErr   x509.HostnameError
		certErr   x509.CertificateInvalidError
		verifyErr *tls.CertificateVerificationError
		dnsErr    *net.DNSError
	)
	switch {
	case errors.As(err, &unknownCA), errors.As(err, &hostErr), errors.As(err, &certErr), errors.As(err, &verifyErr):
		return false
	case errors.As(err, &dnsErr):
		return !dnsErr.IsNotFound
	}
	return true
}
