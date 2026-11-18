package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout 是未提供 HTTPClient 时单次请求的超时时间。
const DefaultTimeout = 30 * time.Second

// Options 是客户端配置。
type Options struct {
	BaseURL    string       // Gitea 根地址，如 https://git.example.com（可带子路径，不含 /api/v1）
	Token      string       // 机器人账号访问令牌
	HTTPClient *http.Client // 为空时使用带 DefaultTimeout 的默认客户端
	UserAgent  string       // 为空时为 "lingo"
}

// Client 是 Gitea API 客户端，可并发使用。
type Client struct {
	base  string // 含 /api/v1，无结尾斜杠
	token string
	hc    *http.Client
	ua    string
}

// New 校验配置并创建客户端。
func New(o Options) (*Client, error) {
	if o.BaseURL == "" {
		return nil, errors.New("gitea: BaseURL is required")
	}
	if o.Token == "" {
		return nil, errors.New("gitea: Token is required")
	}
	u, err := url.Parse(strings.TrimRight(o.BaseURL, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("gitea: invalid BaseURL %q", o.BaseURL)
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	ua := o.UserAgent
	if ua == "" {
		ua = "lingo"
	}
	return &Client{base: u.String() + "/api/v1", token: o.Token, hc: hc, ua: ua}, nil
}

// ErrNotFound 表示资源不存在（HTTP 404），可用 errors.Is 判断。
var ErrNotFound = errors.New("gitea: not found")

// APIError 是 Gitea 返回的非 2xx 响应。
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("gitea: %s %s: %d %s", e.Method, e.Path, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("gitea: %s %s: %d", e.Method, e.Path, e.StatusCode)
}

// Is 让 errors.Is(err, ErrNotFound) 对 404 成立。
func (e *APIError) Is(target error) bool {
	return target == ErrNotFound && e.StatusCode == http.StatusNotFound
}

// IsRetryable 判断错误是否值得由 outbox 重试：网络错误、超时、429 与 5xx 重试；
// 其余 4xx（参数、权限、不存在）重试无意义。
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.StatusCode == http.StatusTooManyRequests || ae.StatusCode >= 500
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF)
}

// ListOptions 是分页参数；Page 从 1 开始，Limit 为 0 时使用服务端默认值
// （上限为实例的 [api] MAX_RESPONSE_ITEMS，可经 GetAPISettings 读取）。
type ListOptions struct {
	Page  int
	Limit int
}

func (o ListOptions) apply(q url.Values) {
	if o.Page > 0 {
		q.Set("page", strconv.Itoa(o.Page))
	}
	if o.Limit > 0 {
		q.Set("limit", strconv.Itoa(o.Limit))
	}
}

// Response 携带分页信息。
type Response struct {
	StatusCode int
	NextPage   int // 0 表示没有下一页
	TotalCount int // X-Total-Count，未返回时为 -1
}

var linkNextRe = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func parseResponse(resp *http.Response) *Response {
	r := &Response{StatusCode: resp.StatusCode, TotalCount: -1}
	if v := resp.Header.Get("X-Total-Count"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			r.TotalCount = n
		}
	}
	if m := linkNextRe.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
		if u, err := url.Parse(m[1]); err == nil {
			r.NextPage, _ = strconv.Atoi(u.Query().Get("page"))
		}
	}
	return r
}

// ListAll 依次请求每一页并合并结果，直到没有下一页。
func ListAll[T any](ctx context.Context, limit int, fetch func(ctx context.Context, o ListOptions) ([]T, *Response, error)) ([]T, error) {
	var all []T
	o := ListOptions{Page: 1, Limit: limit}
	for {
		items, resp, err := fetch(ctx, o)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if resp == nil || resp.NextPage == 0 || resp.NextPage <= o.Page || len(items) == 0 {
			return all, nil
		}
		o.Page = resp.NextPage
	}
}

// esc 对单个路径段转义。
func esc(s string) string { return url.PathEscape(s) }

func repoPath(owner, repo string) string {
	return "/repos/" + esc(owner) + "/" + esc(repo)
}

func (c *Client) newRequest(ctx context.Context, method, path string, q url.Values, body io.Reader, contentType string) (*http.Request, error) {
	// path 由各方法用 esc 拼接，已是转义后的形式
	s := c.base + path
	if len(q) > 0 {
		s += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, s, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.ua)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req, nil
}

// do 发送 JSON 请求；in 非空时编码为请求体，out 非空时解码响应体。
func (c *Client) do(ctx context.Context, method, path string, q url.Values, in, out any) (*Response, error) {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("gitea: encode request: %w", err)
		}
		body = bytes.NewReader(b)
		ct = "application/json"
	}
	req, err := c.newRequest(ctx, method, path, q, body, ct)
	if err != nil {
		return nil, err
	}
	return c.send(req, out)
}

func (c *Client) send(req *http.Request, out any) (*Response, error) {
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	r := parseResponse(resp)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return r, newAPIError(req, resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return r, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return r, fmt.Errorf("gitea: decode %s %s: %w", req.Method, req.URL.Path, err)
	}
	return r, nil
}

func newAPIError(req *http.Request, resp *http.Response) error {
	e := &APIError{Method: req.Method, Path: req.URL.Path, StatusCode: resp.StatusCode}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &m) == nil && m.Message != "" {
		e.Message = m.Message
	} else {
		e.Message = strings.TrimSpace(string(b))
		if len(e.Message) > 200 {
			e.Message = e.Message[:200]
		}
	}
	return e
}
