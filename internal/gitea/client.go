package gitea

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
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// DefaultTimeout 是未提供 HTTPClient 时单次请求的超时时间。
const DefaultTimeout = 30 * time.Second

// Options 是客户端配置。
type Options struct {
	BaseURL    string       // Gitea 根地址，如 https://git.example.com（可带子路径，不含 /api/v1）
	Token      string       // 机器人账号访问令牌
	HTTPClient *http.Client // 为空时使用带 DefaultTimeout 的默认客户端
	UserAgent  string       // 为空时为 "lingo"
	// RejectRedirects 为真时所有请求都不跟随 HTTP 重定向：不向跳转目标发出请求，返回 *RedirectError。
	// 设置在 HTTPClient 的副本上，不修改传入的客户端或 http.DefaultClient。
	RejectRedirects bool
}

// Client 是 Gitea API 客户端，可并发使用。
type Client struct {
	base  string // 含 /api/v1，无结尾斜杠
	token string
	hc    *http.Client
	ua    string
	// noRedirect 对应 Options.RejectRedirects：Go 不自动跟随的 3xx（如流式请求体遇 307/308）也按 RedirectError 报告
	noRedirect bool
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
	if o.RejectRedirects {
		cp := *hc
		cp.CheckRedirect = rejectRedirect
		// Go 在调用 CheckRedirect 之前先解析 Location，解析失败时返回含原始 Location 的错误；
		// 在 Transport 层拦下所有 3xx，http.Client 就不会解析或跟随 Location
		cp.Transport = &rejectRedirectTransport{base: hc.Transport}
		hc = &cp
	}
	ua := o.UserAgent
	if ua == "" {
		ua = "lingo"
	}
	return &Client{base: u.String() + "/api/v1", token: o.Token, hc: hc, ua: ua, noRedirect: o.RejectRedirects}, nil
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

// ErrRedirect 表示 Gitea 返回了重定向而客户端按 Options.RejectRedirects 拒绝跟随，可用 errors.Is 判断。
var ErrRedirect = errors.New("gitea: redirect rejected")

// RedirectError 是被拒绝跟随的重定向。跳转目标没有收到任何请求（也就没收到令牌）；属于配置错误，不重试。
// 为免泄露，不记录跳转地址，只记录类别。
type RedirectError struct {
	Method     string
	Path       string // 发出请求的路径（含 /api/v1）
	StatusCode int    // 301 / 302 / 303 / 307 / 308
	Kind       string // same-origin | subdomain | scheme-downgrade | cross-origin | unknown（Location 缺失或无法解析）
}

func (e *RedirectError) Error() string {
	return fmt.Sprintf("gitea: %s %s: redirect rejected (HTTP %d, %s)", e.Method, e.Path, e.StatusCode, e.Kind)
}

// Is 让 errors.Is(err, ErrRedirect) 成立。
func (e *RedirectError) Is(target error) bool { return target == ErrRedirect }

// rejectRedirect 是 http.Client.CheckRedirect：在发出跳转请求之前调用，返回错误即不跟随。
func rejectRedirect(req *http.Request, via []*http.Request) error {
	prev := via[len(via)-1]
	e := &RedirectError{Method: prev.Method, Path: prev.URL.Path, Kind: redirectKind(prev.URL, req.URL)}
	if req.Response != nil {
		e.StatusCode = req.Response.StatusCode
	}
	return e
}

// rejectRedirectTransport 把任何 3xx 响应转成 *RedirectError（关闭响应体），http.Client 看不到 3xx，
// 也就不会解析 Location 或发出跳转请求。base 为空时用 http.DefaultTransport（只读取，不修改）。
type rejectRedirectTransport struct{ base http.RoundTripper }

func (t *rejectRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil || resp.StatusCode < 300 || resp.StatusCode > 399 {
		return resp, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	kind := "unknown" // Location 缺失或无法解析：不回显原值
	if loc, err := resp.Location(); err == nil {
		kind = redirectKind(req.URL, loc)
	}
	return nil, &RedirectError{Method: req.Method, Path: req.URL.Path, StatusCode: resp.StatusCode, Kind: kind}
}

// redirectKind 给跳转分类，便于排查 BaseURL 配置。
func redirectKind(from, to *url.URL) string {
	fh, th := strings.ToLower(from.Hostname()), strings.ToLower(to.Hostname())
	switch {
	case strings.EqualFold(from.Scheme, "https") && strings.EqualFold(to.Scheme, "http"):
		return "scheme-downgrade"
	case strings.EqualFold(from.Scheme, to.Scheme) && strings.EqualFold(from.Host, to.Host):
		return "same-origin"
	case fh != th && (strings.HasSuffix(th, "."+fh) || strings.HasSuffix(fh, "."+th)):
		return "subdomain"
	}
	return "cross-origin"
}

// DecodeError 表示请求已成功（2xx）但响应体无法解析。写操作此时多半已在 Gitea 生效，不应重试。
type DecodeError struct {
	Method, Path string
	Err          error
}

func (e *DecodeError) Error() string {
	return fmt.Sprintf("gitea: decode %s %s: %v", e.Method, e.Path, e.Err)
}

func (e *DecodeError) Unwrap() error { return e.Err }

// IsRetryable 判断错误是否值得由 outbox 重试：
//   - 429、5xx、连接失败、连接中断、超时：重试
//   - 其余 4xx、证书错误、域名不存在、配置错误、被拒绝的重定向（RedirectError）、2xx 响应解析失败（DecodeError）：不重试
//
// 注意：连接中断或超时发生在请求已发出之后时，写操作可能已经生效。对非幂等写操作
// （CreateIssue、CreateComment 等）重试前，调用方必须先按隐藏标记查重，见 PRD 3.2.2。
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.StatusCode == http.StatusTooManyRequests || ae.StatusCode >= 500
	}
	var de *DecodeError
	if errors.As(err, &de) || errors.Is(err, ErrRedirect) || errors.Is(err, context.Canceled) {
		return false
	}
	// 证书问题与域名不存在属于配置错误，重试也不会成功
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
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// 只认真正的网络操作错误（连接被拒、重置等）与超时；*url.Error 本身也实现 net.Error，不能据此判断
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
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

// esc 对单个路径段转义（"/" 也会被转义）。
func esc(s string) string { return url.PathEscape(s) }

// escRef 转义可能含 "/" 的分支或 tag 名，保留 "/"，用于 Gitea 的通配路由（compare/*、commits/<ref:*>/statuses）。
func escRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// truncateUTF8 把 s 截到最多 n 字节且不切断多字节字符，并替换无效的 UTF-8 序列。
func truncateUTF8(s string, n int) string {
	s = strings.ToValidUTF8(s, string(utf8.RuneError))
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

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
		// 拒绝的重定向不带 *url.Error 包装返回：后者含请求地址，脱敏说明只需要方法、路径与状态码
		var re *RedirectError
		if errors.As(err, &re) {
			return nil, re
		}
		return nil, err
	}
	defer resp.Body.Close()
	r := parseResponse(resp)
	if c.noRedirect && resp.StatusCode >= 300 && resp.StatusCode <= 399 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		kind := "unknown"
		if loc, err := resp.Location(); err == nil {
			kind = redirectKind(req.URL, loc)
		}
		return r, &RedirectError{Method: req.Method, Path: req.URL.Path, StatusCode: resp.StatusCode, Kind: kind}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return r, newAPIError(req, resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return r, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return r, &DecodeError{Method: req.Method, Path: req.URL.Path, Err: err}
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
		e.Message = truncateUTF8(strings.TrimSpace(string(b)), 200)
	}
	return e
}
