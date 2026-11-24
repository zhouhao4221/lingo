package lingo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/zhouhao4221/lingo/internal/gitea"
)

// GiteaConfig.RejectRedirects 经 giteaOptions 传到 lingo 唯一的 Gitea 客户端；默认值保持跟随。令牌为虚构值。
func TestGiteaConfigRejectRedirects(t *testing.T) {
	var landed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/landing" {
			landed.Add(1)
			_, _ = w.Write([]byte(`{"version":"1.25.0"}`))
			return
		}
		http.Redirect(w, r, "/landing", http.StatusFound)
	}))
	defer srv.Close()

	c, err := gitea.New(giteaOptions(GiteaConfig{BaseURL: srv.URL, Token: "fake-token-701", RejectRedirects: true}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetVersion(context.Background()); !errors.Is(err, gitea.ErrRedirect) || gitea.IsRetryable(err) {
		t.Fatalf("RejectRedirects: err = %v, want non-retryable ErrRedirect", err)
	}
	if landed.Load() != 0 {
		t.Fatal("redirect followed with RejectRedirects")
	}

	d, err := gitea.New(giteaOptions(GiteaConfig{BaseURL: srv.URL, Token: "fake-token-701"}))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := d.GetVersion(context.Background()); err != nil || v != "1.25.0" || landed.Load() != 1 {
		t.Fatalf("default: v=%q err=%v landed=%d, want redirect followed as before", v, err, landed.Load())
	}
}
