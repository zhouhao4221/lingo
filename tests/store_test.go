package tests

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo/internal/store"
	"github.com/zhouhao4221/lingo/internal/webhook"
)

// nulPayload 含 JSON 的 NUL 转义：jsonb 会拒收，bytea 必须原样保存。
const nulPayload = `{"a":"\u0000"}`

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.New(testDB(t, true))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func rec(id, key, repo string, issue int64) webhook.Record {
	return webhook.Record{DeliveryID: id, Event: "issues", EventType: "issues", Kind: webhook.IssueOpened,
		DedupKey: key, Repo: repo, IssueNumber: issue, Payload: []byte(nulPayload)}
}

func TestSaveDeliveryDedup(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, c := range []struct {
		r    webhook.Record
		want bool
	}{
		{rec("d1", "k1", "Demo/API", 1), true},
		{rec("d1", "k2", "demo/api", 1), false}, // 同一 delivery
		{rec("d2", "k1", "demo/api", 1), false}, // 手动重投：新 delivery、相同业务键
		{rec("d3", "k3", "demo/api", 0), true},  // 无 Issue 的事件（如 Release）
	} {
		got, err := s.SaveDelivery(ctx, c.r)
		if err != nil || got != c.want {
			t.Errorf("SaveDelivery(%s,%s) = %v, %v; want %v", c.r.DeliveryID, c.r.DedupKey, got, err, c.want)
		}
	}
}

func TestClaimDeliveriesOrderAndLease(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, r := range []webhook.Record{rec("d1", "k1", "demo/api", 12), rec("d2", "k2", "demo/api", 12), rec("d3", "k3", "demo/web", 12), rec("d4", "k4", "demo/api", 0)} {
		if _, err := s.SaveDelivery(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(ds []store.Delivery) string {
		var out []string
		for _, d := range ds {
			out = append(out, d.DeliveryID)
		}
		return fmt.Sprint(out)
	}
	first, err := s.ClaimDeliveries(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// d2 与 d1 属于同一 Issue，须等 d1 完成
	if got := ids(first); got != "[d1 d3 d4]" {
		t.Fatalf("first claim = %s", got)
	}
	if first[0].Attempts != 1 || first[0].Repo != "demo/api" || string(first[0].Payload) != nulPayload {
		t.Errorf("claimed = %+v", first[0])
	}
	// 租约内不会被重复领取
	if again, _ := s.ClaimDeliveries(ctx, 10, time.Minute); len(again) != 0 {
		t.Errorf("claimed again within lease: %s", ids(again))
	}
	if err := s.CompleteDelivery(ctx, first[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.RetryDelivery(ctx, first[1], "gitea down", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.AbandonDelivery(ctx, first[2], "bad payload"); err != nil {
		t.Fatal(err)
	}
	second, err := s.ClaimDeliveries(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// d1 完成后 d2 可领取；d3 推迟到一小时后；d4 已放弃
	if got := ids(second); got != "[d2]" {
		t.Errorf("second claim = %s", got)
	}
	if err := s.CompleteDelivery(ctx, store.Delivery{ID: 999999, Attempts: 1}); !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("completing unknown delivery: %v", err)
	}
}

func TestClaimDeliveriesConcurrentNoDuplicates(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const n = 60
	for i := range n {
		if _, err := s.SaveDelivery(ctx, rec(fmt.Sprintf("d%d", i), fmt.Sprintf("k%d", i), "demo/api", int64(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				ds, err := s.ClaimDeliveries(ctx, 7, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if len(ds) == 0 {
					return
				}
				mu.Lock()
				for _, d := range ds {
					seen[d.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Errorf("claimed %d distinct deliveries, want %d", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("delivery %d claimed %d times", id, c)
		}
	}
}

func TestRecordPingOnlyMovesForward(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	t1 := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	if _, ok, _ := s.LastPing(ctx, "demo/api"); ok {
		t.Fatal("ping exists before any delivery")
	}
	for _, at := range []time.Time{t1, t1.Add(-time.Hour)} {
		if err := s.RecordPing(ctx, "Demo/API", at); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := s.LastPing(ctx, "demo/api")
	if err != nil || !ok || !got.Equal(t1) {
		t.Errorf("LastPing = %v %v %v", got, ok, err)
	}
}

func TestLeaseFencing(t *testing.T) {
	db := testDB(t, true)
	s, _ := store.New(db)
	ctx := context.Background()
	if _, err := s.SaveDelivery(ctx, rec("d1", "k1", "demo/api", 1)); err != nil {
		t.Fatal(err)
	}
	a, _ := s.ClaimDeliveries(ctx, 1, time.Minute)
	if len(a) != 1 {
		t.Fatalf("claim A = %d", len(a))
	}
	expireLeases(t, db)
	b, _ := s.ClaimDeliveries(ctx, 1, time.Minute)
	if len(b) != 1 || b[0].Attempts != 2 {
		t.Fatalf("claim B = %+v", b)
	}
	// A 的租约已失效：它的收尾不能影响 B 正在处理的投递
	for name, err := range map[string]error{
		"complete": s.CompleteDelivery(ctx, a[0]),
		"retry":    s.RetryDelivery(ctx, a[0], "late", time.Now()),
		"abandon":  s.AbandonDelivery(ctx, a[0], "late"),
	} {
		if !errors.Is(err, store.ErrLeaseLost) {
			t.Errorf("stale %s: err = %v", name, err)
		}
	}
	if again, _ := s.ClaimDeliveries(ctx, 1, time.Minute); len(again) != 0 {
		t.Error("stale retry released B's lease")
	}
	if err := s.CompleteDelivery(ctx, b[0]); err != nil {
		t.Errorf("B complete: %v", err)
	}
	if err := s.CompleteDelivery(ctx, b[0]); !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("double complete: %v", err)
	}
}

func TestReleaseDoesNotCountFailure(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	r := rec("d1", "k1", "demo/api", 1)
	r.Kind = webhook.PullMerged
	if _, err := s.SaveDelivery(ctx, r); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.HasPendingMerges(ctx, "Demo/API", time.Now().Add(time.Minute)); err != nil || !ok {
		t.Fatalf("pending merges = %v %v", ok, err)
	}
	// 其他项目的仓库不受影响
	if ok, err := s.HasPendingMerges(ctx, "other/web", time.Now().Add(time.Minute)); err != nil || ok {
		t.Fatalf("other repo pending merges = %v %v", ok, err)
	}
	a, _ := s.ClaimDeliveries(ctx, 1, time.Minute)
	if err := s.ReleaseDelivery(ctx, a[0], "deferred", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	b, _ := s.ClaimDeliveries(ctx, 1, time.Minute)
	if len(b) != 1 || b[0].Attempts != 2 || b[0].Failures != 0 {
		t.Fatalf("after release = %+v", b)
	}
	if err := s.RetryDelivery(ctx, b[0], "boom", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	// 失败过的合并投递不再拖住关闭判定
	if ok, _ := s.HasPendingMerges(ctx, "demo/api", time.Now().Add(time.Minute)); ok {
		t.Error("failed merge still counted as pending")
	}
	c, _ := s.ClaimDeliveries(ctx, 1, time.Minute)
	if len(c) != 1 || c[0].Attempts != 3 || c[0].Failures != 1 {
		t.Fatalf("after retry = %+v", c)
	}
}

func TestPendingMergesScopedToProject(t *testing.T) {
	db := testDB(t, true)
	seedTicket(t, db, "open") // 项目 demo 含 demo/api
	if _, err := db.Exec(`INSERT INTO repos (project_id, owner, name) SELECT id, 'demo', 'web' FROM projects WHERE key = 'demo'`); err != nil {
		t.Fatal(err)
	}
	s, _ := store.New(db)
	ctx := context.Background()
	r := rec("d1", "k1", "demo/web", 3)
	r.Kind = webhook.PullMerged
	if _, err := s.SaveDelivery(ctx, r); err != nil {
		t.Fatal(err)
	}
	// 同项目另一仓库的 PR 可能修复本仓库的 Issue
	if ok, err := s.HasPendingMerges(ctx, "Demo/Api", time.Now().Add(time.Minute)); err != nil || !ok {
		t.Errorf("same project = %v %v", ok, err)
	}
	if ok, _ := s.HasPendingMerges(ctx, "demo/api", time.Now().Add(-time.Hour)); ok {
		t.Error("merge received after the deadline counted")
	}
}

func TestClaimZeroLeaseUsesDefault(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.SaveDelivery(ctx, rec("d1", "k1", "demo/api", 1)); err != nil {
		t.Fatal(err)
	}
	if ds, _ := s.ClaimDeliveries(ctx, 1, 0); len(ds) != 1 {
		t.Fatalf("claim = %d", len(ds))
	}
	if ds, _ := s.ClaimDeliveries(ctx, 1, 0); len(ds) != 0 {
		t.Error("zero lease allowed immediate re-claim")
	}
}

// expireLeases 让所有租约立即过期，模拟 worker 卡住超过租约时长。
func expireLeases(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("UPDATE webhook_deliveries SET locked_until = now() - interval '1 second' WHERE locked_until IS NOT NULL"); err != nil {
		t.Fatal(err)
	}
}
