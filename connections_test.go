package lingo

import (
	"testing"
	"time"

	"github.com/zhouhao4221/lingo/internal/settings"
)

// 新旧按版本号判断；修改时间（事务开始时间）只在版本号变小（配置行补建）时使用。
func TestNewerSettings(t *testing.T) {
	t0 := time.Now()
	t1 := t0.Add(time.Second)
	for _, c := range []struct {
		m, cur settings.Meta
		want   bool
	}{
		{settings.Meta{Version: 6, UpdatedAt: &t0}, settings.Meta{Version: 5, UpdatedAt: &t1}, true},  // 版本更高：较早开始的事务也算更新
		{settings.Meta{Version: 1, UpdatedAt: &t1}, settings.Meta{Version: 6, UpdatedAt: &t0}, true},  // 补建行（版本回到 1）且时间更晚
		{settings.Meta{Version: 5, UpdatedAt: &t1}, settings.Meta{Version: 6, UpdatedAt: &t0}, false}, // 并发保存：较旧版本、较晚开始的事务
		{settings.Meta{Version: 5, UpdatedAt: &t0}, settings.Meta{Version: 6, UpdatedAt: &t1}, false}, // 旧读
		{settings.Meta{Version: 6, UpdatedAt: &t1}, settings.Meta{Version: 6, UpdatedAt: &t0}, false}, // 同版本
		{settings.Meta{Version: 1, UpdatedAt: &t1}, settings.Meta{Version: 7}, true},
	} {
		if got := newer(c.m, c.cur); got != c.want {
			t.Errorf("newer(%d, %d) = %v, want %v", c.m.Version, c.cur.Version, got, c.want)
		}
	}
}
