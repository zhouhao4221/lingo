package gitea

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient 启动一个模拟 Gitea，h 处理 /api/v1 之后的请求。
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(http.StripPrefix("/sub/api/v1", h))
	t.Cleanup(srv.Close)
	c, err := New(Options{BaseURL: srv.URL + "/sub/", Token: "tkn"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func TestNewValidates(t *testing.T) {
	for _, o := range []Options{{Token: "x"}, {BaseURL: "https://g.example"}, {BaseURL: "not a url", Token: "x"}} {
		if _, err := New(o); err == nil {
			t.Errorf("New(%+v) succeeded, want error", o)
		}
	}
}

func TestRequestBasics(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token tkn" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "lingo" {
			t.Errorf("User-Agent = %q", got)
		}
		// owner 中的空格与 / 必须按路径段转义
		if got := r.URL.EscapedPath(); got != "/repos/my%20org/a%2Fb" {
			t.Errorf("path = %q", got)
		}
		writeJSON(w, 200, Repository{Name: "a/b", HasIssues: true, InternalTracker: &InternalTracker{EnableIssueDependencies: true}})
	})
	r, err := c.GetRepo(context.Background(), "my org", "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if !r.HasIssues || !r.InternalTracker.EnableIssueDependencies {
		t.Errorf("decoded repo = %+v", r)
	}
}

func TestAPIErrors(t *testing.T) {
	codes := map[string]int{"/nf": 404, "/bad": 422, "/boom": 502, "/slow": 429}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, codes[r.URL.Path], map[string]string{"message": "msg " + r.URL.Path})
	})
	ctx := context.Background()
	for path, code := range codes {
		_, err := c.do(ctx, "GET", path, nil, nil, nil)
		var ae *APIError
		if !errors.As(err, &ae) || ae.StatusCode != code || ae.Message != "msg "+path {
			t.Fatalf("%s: err = %v", path, err)
		}
		if got, want := errors.Is(err, ErrNotFound), code == 404; got != want {
			t.Errorf("%s: Is(ErrNotFound) = %v", path, got)
		}
		if got, want := IsRetryable(err), code >= 500 || code == 429; got != want {
			t.Errorf("%s: IsRetryable = %v, want %v", path, got, want)
		}
	}
}

func TestIsRetryableTransport(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // 连接被拒绝
	c, _ := New(Options{BaseURL: url, Token: "t"})
	_, err := c.GetVersion(context.Background())
	if err == nil || !IsRetryable(err) {
		t.Errorf("connection refused: err = %v, retryable = %v", err, IsRetryable(err))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.GetVersion(ctx)
	if IsRetryable(err) {
		t.Errorf("canceled context should not be retryable: %v", err)
	}
	if IsRetryable(nil) {
		t.Error("nil error retryable")
	}
}

func TestListAllFollowsLinkHeader(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if r.URL.Query().Get("limit") != "2" {
			t.Errorf("limit = %q", r.URL.Query().Get("limit"))
		}
		w.Header().Set("X-Total-Count", "5")
		labels := map[string][]Label{
			"1": {{ID: 1}, {ID: 2}}, "2": {{ID: 3}, {ID: 4}}, "3": {{ID: 5}},
		}[page]
		if page != "3" {
			var next int
			fmt.Sscan(page, &next)
			w.Header().Set("Link", fmt.Sprintf(`<http://x/api/v1/repos/o/r/labels?limit=2&page=%d>; rel="next", <http://x/api/v1/repos/o/r/labels?page=3>; rel="last"`, next+1))
		}
		writeJSON(w, 200, labels)
	})
	all, err := ListAll(context.Background(), 2, func(ctx context.Context, o ListOptions) ([]*Label, *Response, error) {
		ls, resp, err := c.ListLabels(ctx, "o", "r", o)
		if err == nil && resp.TotalCount != 5 {
			t.Errorf("TotalCount = %d", resp.TotalCount)
		}
		return ls, resp, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 || all[4].ID != 5 {
		t.Errorf("got %d labels", len(all))
	}
}

func TestCreateIssueRequestBody(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/repos/o/r/issues" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		if m["title"] != "t" || fmt.Sprint(m["labels"]) != "[3 7]" || fmt.Sprint(m["assignees"]) != "[dev-agent]" {
			t.Errorf("body = %v", m)
		}
		for _, k := range []string{"milestone", "due_date", "ref"} {
			if _, ok := m[k]; ok {
				t.Errorf("empty field %q should be omitted", k)
			}
		}
		writeJSON(w, 201, Issue{Number: 12, HTMLURL: "http://g/o/r/issues/12"})
	})
	is, err := c.CreateIssue(context.Background(), "o", "r", CreateIssueOption{Title: "t", Labels: []int64{3, 7}, Assignees: []string{"dev-agent"}})
	if err != nil || is.Number != 12 {
		t.Fatalf("issue = %+v, err = %v", is, err)
	}
}

func TestListIssuesQuery(t *testing.T) {
	since := time.Date(2026, 11, 18, 8, 0, 0, 0, time.FixedZone("CST", 8*3600))
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		want := map[string]string{"state": "all", "labels": "lingo,type/bug", "since": "2026-11-18T00:00:00Z", "type": "issues", "page": "2"}
		for k, v := range want {
			if q.Get(k) != v {
				t.Errorf("%s = %q, want %q", k, q.Get(k), v)
			}
		}
		writeJSON(w, 200, []Issue{})
	})
	_, _, err := c.ListIssues(context.Background(), "o", "r", ListIssuesOptions{
		ListOptions: ListOptions{Page: 2}, State: StateAll, Labels: []string{"lingo", "type/bug"}, Since: since,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEditIssueOmitsNil(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if string(b) != `{"state":"closed"}` {
			t.Errorf("body = %s", b)
		}
		writeJSON(w, 201, Issue{State: StateClosed})
	})
	st := StateClosed
	if _, err := c.EditIssue(context.Background(), "o", "r", 1, EditIssueOption{State: &st}); err != nil {
		t.Fatal(err)
	}
}

func TestUploadAttachment(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/issues/5/assets" || r.URL.Query().Get("name") != "shot.png" {
			t.Errorf("%s ?%s", r.URL.Path, r.URL.RawQuery)
		}
		f, hdr, err := r.FormFile("attachment")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(f)
		if hdr.Filename != "shot.png" || string(b) != "PNGDATA" {
			t.Errorf("file %q = %q", hdr.Filename, b)
		}
		writeJSON(w, 201, Attachment{ID: 9, Name: hdr.Filename, BrowserDownloadURL: "http://g/attachments/u"})
	})
	a, err := c.CreateIssueAttachment(context.Background(), "o", "r", 5, "shot.png", strings.NewReader("PNGDATA"))
	if err != nil || a.ID != 9 {
		t.Fatalf("attachment = %+v, err = %v", a, err)
	}
}

func TestUploadErrorIsAPIError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		writeJSON(w, 413, map[string]string{"message": "too big"})
	})
	_, err := c.CreateCommentAttachment(context.Background(), "o", "r", 1, "a.log", strings.NewReader("x"))
	var ae *APIError
	if !errors.As(err, &ae) || ae.StatusCode != 413 || IsRetryable(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestFindHook(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, []Hook{
			{ID: 1, Config: map[string]string{"url": "https://other/hook"}},
			{ID: 2, Config: map[string]string{"url": "https://app/webhooks/gitea"}},
		})
	})
	h, err := c.FindHook(context.Background(), "o", "r", "https://app/webhooks/gitea")
	if err != nil || h == nil || h.ID != 2 {
		t.Fatalf("hook = %+v, err = %v", h, err)
	}
	h, err = c.FindHook(context.Background(), "o", "r", "https://missing")
	if err != nil || h != nil {
		t.Fatalf("missing hook = %+v, err = %v", h, err)
	}
}

func TestNewLingoHook(t *testing.T) {
	h := NewLingoHook("https://app/wh", "s3cret", "")
	b, _ := json.Marshal(h)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	cfg := m["config"].(map[string]any)
	if m["type"] != "gitea" || cfg["content_type"] != "json" || cfg["secret"] != "s3cret" || m["active"] != true {
		t.Errorf("hook = %s", b)
	}
	if _, ok := m["authorization_header"]; ok {
		t.Error("empty authorization_header should be omitted")
	}
}

func TestCompareEscapesRefs(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.EscapedPath(); got != "/repos/o/r/compare/v1.0.0...v1.1.0" {
			t.Errorf("path = %q", got)
		}
		writeJSON(w, 200, Compare{TotalCommits: 1, Commits: []*Commit{{SHA: "abc"}}})
	})
	cp, err := c.Compare(context.Background(), "o", "r", "v1.0.0", "v1.1.0")
	if err != nil || cp.Commits[0].SHA != "abc" {
		t.Fatalf("compare = %+v, err = %v", cp, err)
	}
}

func TestListReleasesExcludesDrafts(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("draft") != "false" || r.URL.Query().Has("pre-release") {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		writeJSON(w, 200, []Release{{TagName: "v1.0.0"}})
	})
	rs, _, err := c.ListReleases(context.Background(), "o", "r", ListReleasesOptions{})
	if err != nil || rs[0].TagName != "v1.0.0" {
		t.Fatalf("releases = %v, err = %v", rs, err)
	}
}

func TestHelpers(t *testing.T) {
	if n := IssueIndexFromURL("https://g.example/api/v1/repos/o/r/issues/42"); n != 42 {
		t.Errorf("IssueIndexFromURL = %d", n)
	}
	if n := IssueIndexFromURL("https://g.example/o/r/issues/abc"); n != 0 {
		t.Errorf("IssueIndexFromURL(bad) = %d", n)
	}
	run := &WorkflowRun{Path: ".gitea/workflows/deploy-prod.yml@refs/heads/main"}
	if got := run.WorkflowFile(); got != "deploy-prod.yml" {
		t.Errorf("WorkflowFile = %q", got)
	}
	for perm, want := range map[string]bool{"read": false, "write": true, "admin": true, "owner": true, "none": false} {
		if got := (&CollaboratorPermission{Permission: perm}).CanWrite(); got != want {
			t.Errorf("CanWrite(%s) = %v", perm, got)
		}
	}
}
