package gitea

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestVerifySignatureRejectsEmptySecret(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	forged := fmt.Sprintf("%x", Sign("", body)) // 空密钥时任何人都能算出的签名
	if err := VerifySignature("", body, forged); !errors.Is(err, ErrEmptySecret) {
		t.Fatalf("err = %v, want ErrEmptySecret", err)
	}
}

func TestIsRetryableClassification(t *testing.T) {
	wrap := func(err error) error { return &url.Error{Op: "Get", URL: "https://g", Err: err} }
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"decode error on 2xx", &DecodeError{Method: "POST", Path: "/issues", Err: errors.New("unexpected EOF")}, false},
		{"unknown host", wrap(&net.DNSError{Err: "no such host", Name: "g", IsNotFound: true}), false},
		{"dns temporary", wrap(&net.DNSError{Err: "server misbehaving", Name: "g", IsTemporary: true}), true},
		{"bad certificate", wrap(x509.UnknownAuthorityError{}), false},
		{"hostname mismatch", wrap(x509.HostnameError{Host: "g"}), false},
		{"connection refused", wrap(&net.OpError{Op: "dial", Err: errors.New("connection refused")}), true},
		{"unsupported scheme", wrap(errors.New("unsupported protocol scheme")), false},
		{"timeout", wrap(context.DeadlineExceeded), true},
	}
	for _, c := range cases {
		if got := IsRetryable(c.err); got != c.want {
			t.Errorf("%s: IsRetryable = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDecodeErrorFromClient(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"number": 1, "tit`)) // 被截断的响应
	})
	_, err := c.CreateIssue(context.Background(), "o", "r", CreateIssueOption{Title: "t"})
	var de *DecodeError
	if !errors.As(err, &de) || IsRetryable(err) {
		t.Fatalf("err = %v, retryable = %v", err, IsRetryable(err))
	}
}

func TestRefsKeepSlashes(t *testing.T) {
	var paths []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		if strings.Contains(r.URL.Path, "statuses") {
			writeJSON(w, 200, []CommitStatus{})
			return
		}
		writeJSON(w, 200, Compare{})
	})
	ctx := context.Background()
	_, _ = c.Compare(ctx, "o", "r", "release/v1.0", "release/v1 1")
	_, _, _ = c.ListCommitStatuses(ctx, "o", "r", "feature/x", ListOptions{})
	want := []string{"/repos/o/r/compare/release/v1.0...release/v1%201", "/repos/o/r/commits/feature/x/statuses"}
	if fmt.Sprint(paths) != fmt.Sprint(want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}
}

func TestHookSubscribesPullRequestComment(t *testing.T) {
	if !contains(LingoHookEvents, EventPullComment) {
		t.Errorf("LingoHookEvents = %v, missing %s", LingoHookEvents, EventPullComment)
	}
}

func TestEditIssueClearAssignees(t *testing.T) {
	empty := []string{}
	b, _ := json.Marshal(EditIssueOption{Assignees: &empty})
	if string(b) != `{"assignees":[]}` {
		t.Errorf("clear assignees = %s", b)
	}
	b, _ = json.Marshal(EditIssueOption{})
	if string(b) != `{}` {
		t.Errorf("unset = %s", b)
	}
}

func TestTruncateUTF8(t *testing.T) {
	s := strings.Repeat("错", 100) // 每个 3 字节
	got := truncateUTF8(s, 200)
	if !utf8.ValidString(got) || len(got) > 200 || len(got) != 198 {
		t.Errorf("len = %d, valid = %v", len(got), utf8.ValidString(got))
	}
	if got := truncateUTF8("ok\xff", 200); !utf8.ValidString(got) {
		t.Errorf("invalid input not sanitized: %q", got)
	}
}

func TestIssueIndexFromURLTrailingSlash(t *testing.T) {
	if n := IssueIndexFromURL("https://g.example/o/r/issues/12/"); n != 12 {
		t.Errorf("IssueIndexFromURL = %d", n)
	}
}

func TestMinQuotesIndentedFence(t *testing.T) {
	for value, want := range map[string]string{
		"plain":                  "```",
		"log:\n  ```\nrest":      "````",
		"````go\nx":              "`````",
		"    ```\nindented code": "```", // 4 个空格是缩进代码块，不会关闭围栏
	} {
		if got := minQuotes(value); got != want {
			t.Errorf("minQuotes(%q) = %q, want %q", value, got, want)
		}
	}
}
