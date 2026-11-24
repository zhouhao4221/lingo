package knowledge

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo/internal/auth"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

func TestTokens(t *testing.T) {
	got := Tokens("导出报表为空, Export CSV v1.2 怎")
	want := []string{"导出", "出报", "报表", "表为", "为空", "export", "csv", "v1", "2", "怎"}
	if !slices.Equal(got, want) {
		t.Errorf("Tokens = %v", got)
	}
	if got := Tokens("导出 导出"); !slices.Equal(got, []string{"导出"}) {
		t.Errorf("dedupe = %v", got)
	}
	if got := words("导出报表，为什么是空的? a"); !slices.Equal(got, []string{"导出报表", "为什么是空的"}) {
		t.Errorf("words = %v", got)
	}
}

func TestPrepareScrubsAndValidates(t *testing.T) {
	d, err := Prepare(Draft{Question: " 联系 a.b@example.com ", Answer: "打 138 1234 5678 或 +86-13912345678"})
	if err != nil || d.Question != "联系 [邮箱]" || strings.Contains(d.Answer, "138") || strings.Contains(d.Answer, "139") {
		t.Errorf("prepare = %+v %v", d, err)
	}
	for _, d := range []Draft{{Question: "q"}, {Answer: "a"}, {Question: strings.Repeat("问", MaxQuestionRunes+1), Answer: "a"},
		{Question: "q", Answer: strings.Repeat("答", MaxAnswerRunes+1)}} {
		if _, err := Prepare(d); !errors.Is(err, ticket.ErrInvalid) {
			t.Errorf("%d/%d: %v", len(d.Question), len(d.Answer), err)
		}
	}
}

type fakeStore struct {
	Store
	hits  []Hit
	embs  map[int64][]float32
	model string
}

func (f *fakeStore) SearchKnowledgeTokens(context.Context, []string, string, []string, int) ([]Hit, error) {
	return slices.Clone(f.hits), nil
}
func (f *fakeStore) SearchKnowledgeWords(context.Context, []string, string, []string, int) ([]Hit, error) {
	return nil, nil
}
func (f *fakeStore) KnowledgeEmbeddings(_ context.Context, _ []int64, model string) (map[int64][]float32, error) {
	f.model = model
	return f.embs, nil
}

type fakeEmbedder struct {
	v    []float32
	name string
	err  error
}

func (e fakeEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return [][]float32{e.v}, e.err
}
func (e fakeEmbedder) Name() string { return e.name }

func TestSearchRerank(t *testing.T) {
	now := time.Now()
	st := &fakeStore{hits: []Hit{{Entry: &Entry{ID: 1, UpdatedAt: now}, Score: 2}, {Entry: &Entry{ID: 2, UpdatedAt: now}, Score: 2}},
		embs: map[int64][]float32{1: {0, 1}, 2: {1, 0}}}
	var em Embedder = fakeEmbedder{v: []float32{1, 0}, name: "m1"}
	s, _ := New(Options{Store: st, Embedder: func() Embedder { return em }})
	p := auth.Principal{ID: "u", Role: auth.RoleExternal, Projects: []string{"demo"}}
	hits, err := s.Search(context.Background(), p, "", "导出", nil, 5)
	if err != nil || len(hits) != 2 || hits[0].ID != 2 || st.model != "m1" {
		t.Fatalf("rerank = %+v %v model %q", hits, err, st.model)
	}
	// 模型不可用：保持切词顺序
	em = fakeEmbedder{err: errors.New("down")}
	if hits, _ := s.Search(context.Background(), p, "", "导出", nil, 5); hits[0].ID != 1 {
		t.Errorf("fallback order = %d", hits[0].ID)
	}
	// 未给出模型名：以维度作为标识
	em = fakeEmbedder{v: []float32{1, 0}}
	if _, _ = s.Search(context.Background(), p, "", "导出", nil, 5); st.model != "dim:2" {
		t.Errorf("model = %q", st.model)
	}
	// 项目不在范围内、无检索词：为空
	if hits, _ := s.Search(context.Background(), p, "other", "导出", nil, 5); hits != nil {
		t.Errorf("out of scope = %v", hits)
	}
	if hits, _ := s.Search(context.Background(), p, "", "，。", nil, 5); hits != nil {
		t.Errorf("no tokens = %v", hits)
	}
}

func TestMaintenanceRequiresStaff(t *testing.T) {
	s, _ := New(Options{Store: &fakeStore{}})
	ext := auth.Principal{ID: "u", Role: auth.RoleExternal}
	if _, err := s.Create(context.Background(), ext, Draft{ProjectKey: "demo", Question: "q", Answer: "a"}); !errors.Is(err, ticket.ErrForbidden) {
		t.Errorf("create: %v", err)
	}
	rv := auth.Principal{ID: "r", Role: auth.RoleReviewer, Projects: []string{"demo"}}
	if _, err := s.Create(context.Background(), rv, Draft{ProjectKey: "other", Question: "q", Answer: "a"}); !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("other project: %v", err)
	}
	if _, err := s.List(context.Background(), rv, ListQuery{Sort: "x"}); !errors.Is(err, ticket.ErrInvalid) {
		t.Errorf("bad sort: %v", err)
	}
}
