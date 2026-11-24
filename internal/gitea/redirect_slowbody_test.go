package gitea

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// slowRedirectServer 回 3xx 后只写出声明长度的一小部分响应体就停住，直到测试结束。
// 读取响应体的客户端会一直等；只关闭响应体的客户端立刻返回。仅监听本机回环，令牌为虚构值。
func slowRedirectServer(t *testing.T, code int, location string) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop); ln.Close() })
	hits := &atomic.Int32{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				req, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, req.Body)
				hits.Add(1)
				fmt.Fprintf(conn, "HTTP/1.1 %d Redirect\r\nLocation: %s\r\nContent-Length: 1048576\r\n\r\nslow", code, location)
				select { // 不再写出剩余响应体
				case <-stop:
				case <-time.After(30 * time.Second):
				}
			}(conn)
		}
	}()
	return "http://" + ln.Addr().String(), hits
}

// bodyTracker 包住底层 Transport，记录 3xx 响应体是否被读取、是否被关闭。
type bodyTracker struct {
	base   http.RoundTripper
	mu     sync.Mutex
	bodies []*trackedBody
}

type trackedBody struct {
	io.ReadCloser
	reads, closed atomic.Int32
}

func (b *trackedBody) Read(p []byte) (int, error) { b.reads.Add(1); return b.ReadCloser.Read(p) }
func (b *trackedBody) Close() error               { b.closed.Add(1); return b.ReadCloser.Close() }

func (t *bodyTracker) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil {
		tb := &trackedBody{ReadCloser: resp.Body}
		resp.Body = tb
		t.mu.Lock()
		t.bodies = append(t.bodies, tb)
		t.mu.Unlock()
	}
	return resp, err
}

func TestRejectRedirectsSlowBodyNotRead(t *testing.T) {
	target := &recorder{}
	tsrv := httptest.NewServer(target.handler())
	defer tsrv.Close()
	locations := map[string]string{
		"cross-origin": tsrv.URL + "/landing",
		"same-origin":  "/landing",
		"unparseable":  "http://[leak-secret-701",
	}
	for lname, loc := range locations {
		for _, code := range []int{301, 302, 303, 307, 308} {
			for name, call := range calls {
				t.Run(fmt.Sprintf("%s/%d/%s", lname, code, name), func(t *testing.T) {
					base, hits := slowRedirectServer(t, code, loc)
					tr := &bodyTracker{base: &http.Transport{}}
					defer tr.base.(*http.Transport).CloseIdleConnections()
					c, err := New(Options{BaseURL: base, Token: fakeToken, HTTPClient: &http.Client{Transport: tr}, RejectRedirects: true})
					if err != nil {
						t.Fatal(err)
					}
					start := time.Now()
					err = call(c)
					elapsed := time.Since(start)
					if elapsed > 2*time.Second {
						t.Fatalf("waited %v on a stalled 3xx body", elapsed)
					}
					var re *RedirectError
					if !errors.Is(err, ErrRedirect) || !errors.As(err, &re) || re.StatusCode != code {
						t.Fatalf("err = %v, want RedirectError %d", err, code)
					}
					if IsRetryable(err) {
						t.Error("must not be retryable")
					}
					if msg := err.Error(); strings.Contains(msg, "leak-secret-701") || strings.Contains(msg, fakeToken) || strings.Contains(msg, tsrv.URL) {
						t.Errorf("error leaks Location or token: %s", msg)
					}
					if n := hits.Load(); n != 1 {
						t.Errorf("source got %d requests, want 1 (no retry, no follow)", n)
					}
					tr.mu.Lock()
					defer tr.mu.Unlock()
					if len(tr.bodies) != 1 {
						t.Fatalf("round trips = %d, want 1", len(tr.bodies))
					}
					if b := tr.bodies[0]; b.reads.Load() != 0 || b.closed.Load() == 0 {
						t.Errorf("3xx body reads=%d closed=%d, want reads=0 closed>0", b.reads.Load(), b.closed.Load())
					}
				})
			}
		}
	}
	if hits, auths := target.snapshot(); hits != 0 {
		t.Fatalf("redirect target got %d request(s): %q", hits, auths)
	}
}

// send() 的 3xx 兜底分支（底层 Transport 不经 rejectRedirectTransport 时）同样不读响应体。
func TestSendFallbackDoesNotReadBody(t *testing.T) {
	base, _ := slowRedirectServer(t, http.StatusFound, "/x")
	tr := &bodyTracker{base: &http.Transport{}}
	defer tr.base.(*http.Transport).CloseIdleConnections()
	c, err := New(Options{BaseURL: base, Token: fakeToken, RejectRedirects: true})
	if err != nil {
		t.Fatal(err)
	}
	c.cfg.Load().hc = &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start := time.Now()
	_, err = c.GetVersion(context.Background())
	if time.Since(start) > 2*time.Second || !errors.Is(err, ErrRedirect) || IsRetryable(err) {
		t.Fatalf("elapsed=%v err=%v", time.Since(start), err)
	}
	if b := tr.bodies[0]; b.reads.Load() != 0 || b.closed.Load() == 0 {
		t.Errorf("body reads=%d closed=%d", b.reads.Load(), b.closed.Load())
	}
}
