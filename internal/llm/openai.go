package llm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
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
	// ErrStreamIncomplete 表示流式回答没有收到结束标记（[DONE] 或 finish_reason）就断开了。
	ErrStreamIncomplete = errors.New("llm: stream ended before completion")
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

// Usage 是一次调用消耗的 token（接口未返回时为 0）。Cached 是输入中命中服务端前缀缓存的部分
// （OpenAI 的 prompt_tokens_details.cached_tokens、DeepSeek 的 prompt_cache_hit_tokens），用来观察提示词前缀是否稳定。
type Usage struct {
	Model  string
	Input  int
	Output int
	Cached int
}

// Chat 调用 /chat/completions，返回第一个候选的文本。
func (c *OpenAI) Chat(ctx context.Context, messages []Message) (string, error) {
	s, _, err := c.ChatUsage(ctx, messages)
	return s, err
}

// ChatUsage 同 Chat，另返回 token 用量（出错时为已知部分）。
func (c *OpenAI) ChatUsage(ctx context.Context, messages []Message) (string, Usage, error) {
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
	var resp completion
	u := Usage{Model: c.o.Model}
	if err := c.post(ctx, "/chat/completions", req, &resp); err != nil {
		return "", u, err
	}
	return resp.result(u)
}

// completion 是 /chat/completions 的非流式响应。
type completion struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"` // DeepSeek
	} `json:"usage"`
}

// result 取第一个候选的文本与用量（u 为已知的默认值）。
func (resp *completion) result(u Usage) (string, Usage, error) {
	if resp.Model != "" {
		u.Model = resp.Model
	}
	if x := resp.Usage; x != nil {
		u.Input, u.Output, u.Cached = x.PromptTokens, x.CompletionTokens, x.PromptCacheHitTokens
		if d := x.PromptTokensDetails; d != nil && d.CachedTokens > 0 {
			u.Cached = d.CachedTokens
		}
	}
	if len(resp.Choices) == 0 {
		return "", u, errors.New("llm: empty choices")
	}
	ch := resp.Choices[0]
	switch {
	case ch.FinishReason == "length":
		return "", u, ErrTruncated
	case strings.TrimSpace(ch.Message.Content) == "":
		return "", u, ErrEmptyContent
	}
	return ch.Message.Content, u, nil
}

// ChatStream 以流式调用 /chat/completions（stream=true），逐片段调用 onDelta，返回全文与 token 用量。
// 只在收到响应之前重试（429 / 5xx / 网络错误）；开始输出后出错直接返回，已回调的片段不撤回。
func (c *OpenAI) ChatStream(ctx context.Context, messages []Message, onDelta func(string)) (string, Usage, error) {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	req := struct {
		Model         string   `json:"model"`
		Messages      []msg    `json:"messages"`
		Temperature   *float64 `json:"temperature,omitempty"`
		Stream        bool     `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}{Model: c.o.Model, Temperature: c.o.Temperature, Stream: true}
	req.StreamOptions.IncludeUsage = true
	for _, m := range messages {
		req.Messages = append(req.Messages, msg{m.Role, m.Content})
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", Usage{}, err
	}
	// 不认识 stream_options 的兼容接口会返回 400：去掉它再试一次（只是拿不到用量）
	plain, err := json.Marshal(struct {
		Model       string   `json:"model"`
		Messages    []msg    `json:"messages"`
		Temperature *float64 `json:"temperature,omitempty"`
		Stream      bool     `json:"stream"`
	}{req.Model, req.Messages, req.Temperature, true})
	if err != nil {
		return "", Usage{}, err
	}
	u := Usage{Model: c.o.Model}
	var resp *http.Response
	for attempt := 0; ; attempt++ {
		resp, err = c.open(ctx, "/chat/completions", body, "text/event-stream")
		var ae *APIError
		if errors.As(err, &ae) && ae.StatusCode == http.StatusBadRequest && !bytes.Equal(body, plain) {
			body = plain
			resp, err = c.open(ctx, "/chat/completions", body, "text/event-stream")
		}
		var ra *retryAfter
		if !errors.As(err, &ra) {
			break
		}
		wait := time.Duration(attempt+1) * time.Second
		if ra.d > 0 {
			wait = ra.d
		}
		if attempt >= c.retries || wait > maxRetryWait {
			return "", u, ra.err
		}
		select {
		case <-ctx.Done():
			return "", u, ctx.Err()
		case <-time.After(wait):
		}
	}
	if err != nil {
		return "", u, err
	}
	defer resp.Body.Close()
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "" && mt != "text/event-stream" {
		// 网关或兼容服务忽略了 stream，返回普通响应：按整段回答处理
		data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		if err != nil {
			return "", u, fmt.Errorf("llm: read response: %w", err)
		}
		var cr completion
		if err := json.Unmarshal(data, &cr); err != nil {
			return "", u, fmt.Errorf("llm: decode response: %w", err)
		}
		out, u, err := cr.result(u)
		if err == nil && onDelta != nil {
			onDelta(out)
		}
		return out, u, err
	}
	var out strings.Builder
	finish, done := "", false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			done = true
			break
		}
		var chunk struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens        int `json:"prompt_tokens"`
				CompletionTokens    int `json:"completion_tokens"`
				PromptTokensDetails *struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
				PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return out.String(), u, fmt.Errorf("llm: decode stream chunk: %w", err)
		}
		if chunk.Model != "" {
			u.Model = chunk.Model
		}
		if x := chunk.Usage; x != nil {
			u.Input, u.Output, u.Cached = x.PromptTokens, x.CompletionTokens, x.PromptCacheHitTokens
			if d := x.PromptTokensDetails; d != nil && d.CachedTokens > 0 {
				u.Cached = d.CachedTokens
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		ch := chunk.Choices[0]
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}
		if d := ch.Delta.Content; d != "" {
			out.WriteString(d)
			if onDelta != nil {
				onDelta(d)
			}
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return out.String(), u, ctx.Err()
		}
		return out.String(), u, fmt.Errorf("llm: read stream: %w", err)
	}
	switch {
	case finish == "length":
		return out.String(), u, ErrTruncated
	case !done && finish == "":
		// 没有收到结束标记就断开（代理或上游中途关闭连接）：回答不完整
		return out.String(), u, ErrStreamIncomplete
	case strings.TrimSpace(out.String()) == "":
		return "", u, ErrEmptyContent
	}
	return out.String(), u, nil
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
	resp, err := c.open(ctx, path, body, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &retryAfter{err: fmt.Errorf("llm: read response: %w", err)}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("llm: decode response: %w", err)
	}
	return nil
}

// open 发出请求并返回 2xx 响应（调用方关闭 Body）；可重试的错误包装为 *retryAfter。
func (c *OpenAI) open(ctx context.Context, path string, body []byte, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.o.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	if c.o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.o.APIKey)
	}
	for k, v := range c.o.Headers {
		req.Header.Set(k, v)
	}
	resp, err := c.o.HTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !transient(err) {
			return nil, fmt.Errorf("llm: %w", err) // 证书、域名不存在等配置问题，重试无用
		}
		return nil, &retryAfter{err: fmt.Errorf("llm: %w", err)}
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		ae := &APIError{StatusCode: resp.StatusCode, Message: errorMessage(data)}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return nil, &retryAfter{err: ae, d: retryAfterHeader(resp.Header.Get("Retry-After"))}
		}
		return nil, ae
	}
	return resp, nil
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
