package gitea

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// 所有令牌都是虚构的；所有服务都是本地 httptest，不访问任何真实 Gitea。
const fakeToken = "fake-token-701"

// recorder 记录跳转目标收到的请求与其中的令牌。
type recorder struct {
	mu    sync.Mutex
	hits  int
	auths []string
}

func (rc *recorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rc.mu.Lock()
		rc.hits++
		rc.auths = append(rc.auths, r.Header.Get("Authorization"))
		rc.mu.Unlock()
		writeJSON(w, 200, map[string]any{"version": "1.25.0", "id": 1})
	}
}

func (rc *recorder) snapshot() (int, []string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.hits, append([]string(nil), rc.auths...)
}

// fakeDNSClient 把 *.test 主机名解析到 127.0.0.1（保留端口），以便在本地模拟同域、子域与跨域跳转。
func fakeDNSClient() *http.Client {
	d := &net.Dialer{}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			return d.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // 仅测试：httptest 证书不含 gitea.test
	}
	return &http.Client{Transport: tr}
}

func port(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

type scenario struct {
	name     string
	tls      bool                  // 源服务是否为 https
	location func(p string) string // p 为跳转目标服务的端口
	kind     string
}

var scenarios = []scenario{
	{"same-origin", false, func(p string) string { return "http://gitea.test:" + p + "/landing" }, "same-origin"},
	{"subdomain", false, func(p string) string { return "http://evil.gitea.test:" + p + "/landing" }, "subdomain"},
	{"cross-origin", false, func(p string) string { return "http://attacker.test:" + p + "/landing" }, "cross-origin"},
	{"scheme-downgrade", true, func(p string) string { return "http://gitea.test:" + p + "/landing" }, "scheme-downgrade"},
}

// calls 覆盖各类请求：GET、带 JSON 体的 POST / PATCH、DELETE、流式 multipart 上传、分页列表。
var calls = map[string]func(c *Client) error{
	"GET version": func(c *Client) error { _, err := c.GetVersion(context.Background()); return err },
	"GET user":    func(c *Client) error { _, err := c.GetCurrentUser(context.Background()); return err },
	"POST issue": func(c *Client) error {
		_, err := c.CreateIssue(context.Background(), "o", "r", CreateIssueOption{Title: "t", Body: "secret body"})
		return err
	},
	"POST comment": func(c *Client) error { _, err := c.CreateComment(context.Background(), "o", "r", 1, "b"); return err },
	"PATCH issue": func(c *Client) error {
		s := "closed"
		_, err := c.EditIssue(context.Background(), "o", "r", 1, EditIssueOption{State: &s})
		return err
	},
	"DELETE label": func(c *Client) error { return c.RemoveIssueLabel(context.Background(), "o", "r", 1, 2) },
	"POST upload": func(c *Client) error {
		_, err := c.CreateIssueAttachment(context.Background(), "o", "r", 1, "a.png", strings.NewReader("png"))
		return err
	},
	"GET list all": func(c *Client) error {
		_, err := ListAll(context.Background(), 50, func(ctx context.Context, o ListOptions) ([]*Label, *Response, error) {
			return c.ListLabels(ctx, "o", "r", o)
		})
		return err
	},
}

func TestRejectRedirectsNeverFollows(t *testing.T) {
	for _, sc := range scenarios {
		for _, code := range []int{301, 302, 303, 307, 308} {
			for name, call := range calls {
				t.Run(sc.name+"/"+http.StatusText(code)+"/"+name, func(t *testing.T) {
					target := &recorder{}
					tsrv := httptest.NewServer(target.handler())
					defer tsrv.Close()
					var srcHits int
					src := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						srcHits++
						if r.URL.Path == "/landing" { // 同源跳转的目标就在源服务上
							target.handler()(w, r)
							return
						}
						w.Header().Set("Location", sc.location(port(t, tsrv)))
						w.WriteHeader(code)
					})
					var ssrv *httptest.Server
					if sc.tls {
						ssrv = httptest.NewTLSServer(src)
					} else {
						ssrv = httptest.NewServer(src)
					}
					defer ssrv.Close()
					if sc.name == "same-origin" { // 同源：跳回源服务自己
						tsrv.Close()
						tsrv = ssrv
					}
					scheme := "http"
					if sc.tls {
						scheme = "https"
					}
					c, err := New(Options{BaseURL: scheme + "://gitea.test:" + port(t, ssrv), Token: fakeToken,
						HTTPClient: fakeDNSClient(), RejectRedirects: true})
					if err != nil {
						t.Fatal(err)
					}
					err = call(c)
					if !errors.Is(err, ErrRedirect) {
						t.Fatalf("err = %v, want ErrRedirect", err)
					}
					var re *RedirectError
					if !errors.As(err, &re) || re.StatusCode != code {
						t.Fatalf("RedirectError = %+v, want status %d", re, code)
					}
					if re.Kind != sc.kind {
						t.Errorf("Kind = %q, want %q", re.Kind, sc.kind)
					}
					if IsRetryable(err) {
						t.Error("redirect must not be retryable")
					}
					if strings.Contains(err.Error(), "landing") || strings.Contains(err.Error(), fakeToken) {
						t.Errorf("error leaks redirect target or token: %v", err)
					}
					if hits, auths := target.snapshot(); hits != 0 {
						t.Fatalf("redirect target got %d request(s), auth headers %q", hits, auths)
					}
					if srcHits != 1 {
						t.Errorf("source got %d requests, want 1", srcHits)
					}
				})
			}
		}
	}
}

// 默认（RejectRedirects 为假）行为与 v0.2.0 相同：跟随重定向；同源跳转仍带令牌，跨域时由 Go 去掉令牌。
func TestDefaultStillFollowsRedirects(t *testing.T) {
	target := &recorder{}
	tsrv := httptest.NewServer(target.handler())
	defer tsrv.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/landing" {
			target.handler()(w, r)
			return
		}
		loc := "http://other.test:" + port(t, tsrv) + "/landing" // 不同主机
		if r.URL.Query().Get("same") == "1" || strings.HasSuffix(r.URL.Path, "/version") {
			loc = "/landing"
		}
		http.Redirect(w, r, loc, http.StatusFound)
	}))
	defer src.Close()
	c, err := New(Options{BaseURL: "http://gitea.test:" + port(t, src), Token: fakeToken, HTTPClient: fakeDNSClient()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetVersion(context.Background()); err != nil { // 同源
		t.Fatalf("same-origin redirect not followed by default: %v", err)
	}
	if _, err := c.GetCurrentUser(context.Background()); err != nil { // 不同主机
		t.Fatalf("cross-origin redirect not followed by default: %v", err)
	}
	hits, auths := target.snapshot()
	if hits != 2 || auths[0] != "token "+fakeToken {
		t.Fatalf("default behavior changed: hits=%d auths=%q", hits, auths)
	}
	if auths[1] != "" {
		t.Errorf("Go stopped stripping Authorization on cross-origin redirect: %q", auths[1])
	}
}

func TestRejectRedirectsDoesNotMutateClients(t *testing.T) {
	hc := &http.Client{}
	c, err := New(Options{BaseURL: "https://g.example", Token: fakeToken, HTTPClient: hc, RejectRedirects: true})
	if err != nil {
		t.Fatal(err)
	}
	if hc.CheckRedirect != nil || hc.Transport != nil || http.DefaultClient.CheckRedirect != nil || http.DefaultClient.Transport != nil {
		t.Fatal("caller's client or http.DefaultClient was modified")
	}
	if c.hc == hc || c.hc.CheckRedirect == nil {
		t.Fatal("RejectRedirects not applied to a private copy")
	}
	d, _ := New(Options{BaseURL: "https://g.example", Token: fakeToken, RejectRedirects: true})
	if d.hc.Timeout != DefaultTimeout || d.hc.CheckRedirect == nil {
		t.Errorf("default client: timeout=%v checkRedirect=%v", d.hc.Timeout, d.hc.CheckRedirect != nil)
	}
	e, _ := New(Options{BaseURL: "https://g.example", Token: fakeToken})
	if e.hc.CheckRedirect != nil || e.hc.Timeout != DefaultTimeout {
		t.Error("default (RejectRedirects=false) client changed")
	}
}

func TestRedirectKind(t *testing.T) {
	mu := func(s string) *url.URL { u, _ := url.Parse(s); return u }
	for _, tc := range []struct{ from, to, want string }{
		{"https://git.example.com/api", "https://git.example.com/x", "same-origin"},
		{"https://git.example.com/api", "http://git.example.com/x", "scheme-downgrade"},
		{"https://git.example.com/api", "https://a.git.example.com/x", "subdomain"},
		{"https://git.example.com/api", "https://example.com/x", "subdomain"},
		{"https://git.example.com/api", "https://evil.com/x", "cross-origin"},
		{"https://git.example.com/api", "https://git.example.com:8443/x", "cross-origin"},
		{"http://git.example.com/api", "https://git.example.com/x", "cross-origin"},
	} {
		if got := redirectKind(mu(tc.from), mu(tc.to)); got != tc.want {
			t.Errorf("%s -> %s = %q, want %q", tc.from, tc.to, got, tc.want)
		}
	}
}
