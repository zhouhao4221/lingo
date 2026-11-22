package selfcheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo/internal/gitea"
)

// 自检用真实 gitea.Client（RejectRedirects）对接一个把所有请求都跳走的本地服务：跳转目标不能收到任何请求，
// 各项检查报告被拒绝的重定向。令牌为虚构值，不访问真实 Gitea。
func TestRunRejectsRedirects(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(200)
	}))
	defer target.Close()
	var srcHits atomic.Int32
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srcHits.Add(1)
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer src.Close()
	c, err := gitea.New(gitea.Options{BaseURL: src.URL, Token: "fake-token-701", RejectRedirects: true})
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{pings: map[string]time.Time{}}
	rs := Run(context.Background(), Options{Gitea: c, Pings: f, LabelPrefix: "lingo", WebhookURL: "https://app/wh", WebhookSecret: "s",
		AutoRegister: true, PingWait: 100 * time.Millisecond,
		Repos: []Repo{{Name: "demo/api", Templates: map[string]string{"bug": "bug.yaml"}, DeployWorkflow: "deploy.yml"}}})
	if n := targetHits.Load(); n != 0 {
		t.Fatalf("redirect target received %d request(s)", n)
	}
	if srcHits.Load() == 0 {
		t.Fatal("self-check sent no request")
	}
	m := byItem(rs)
	for _, k := range []string{"|version", "demo/api|repo"} {
		r := m[k]
		if r.Level != Error || !strings.Contains(r.Message, "redirect rejected (HTTP 307") {
			t.Errorf("%s = %+v, want error with redirect rejected", k, r)
		}
		if strings.Contains(r.Message, target.URL) {
			t.Errorf("%s leaks redirect target: %s", k, r.Message)
		}
	}
}
