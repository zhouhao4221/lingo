package worker

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

// 处理中途 Gitea 连接被清空：转为推迟（不计失败、不放弃），即使已被包装为永久错误。
func TestDeferUnconfigured(t *testing.T) {
	now := time.Now()
	for _, err := range []error{gitea.ErrNotConfigured, giteaErr("create issue", gitea.ErrNotConfigured), fmt.Errorf("x: %w", permanent(gitea.ErrNotConfigured))} {
		var de *ticket.DeferError
		if got := deferUnconfigured(now, err); !errors.As(got, &de) || !de.Until.After(now) {
			t.Errorf("%v -> %v", err, got)
		}
	}
	if other := errors.New("boom"); deferUnconfigured(now, other) != other {
		t.Error("other errors must pass through")
	}
}
