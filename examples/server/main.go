// Command server 是最小接入示例：在业务项目的 HTTP 服务中挂载 lingo。
// 运行：LINGO_DATABASE_URL=... LINGO_GITEA_URL=... LINGO_GITEA_TOKEN=... go run ./examples/server
// 示例需自行引入 PostgreSQL 驱动（如 github.com/jackc/pgx/v5/stdlib）并实现 Model。
package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"

	"github.com/zhouhao4221/lingo"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	db, err := sql.Open("pgx", os.Getenv("LINGO_DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	if err := lingo.Migrate(ctx, db); err != nil {
		log.Fatal(err)
	}

	l, err := lingo.New(lingo.Config{
		DB:    db,
		Model: nil, // 替换为自己的 lingo.Model 实现
		Gitea: lingo.GiteaConfig{
			BaseURL:       os.Getenv("LINGO_GITEA_URL"),
			Token:         os.Getenv("LINGO_GITEA_TOKEN"),
			WebhookSecret: os.Getenv("LINGO_WEBHOOK_SECRET"),
		},
		Projects: []lingo.Project{{
			Key:  "demo",
			Name: "示例项目",
			Repos: []lingo.Repo{
				{Owner: "demo", Name: "web", Kind: lingo.RepoFrontend},
				{Owner: "demo", Name: "api", Kind: lingo.RepoBackend},
			},
			DefaultRepo: "demo/api",
		}},
		Authenticate: func(r *http.Request) (lingo.User, error) {
			// 替换为业务项目自己的登录态解析
			id := r.Header.Get("X-User-ID")
			if id == "" {
				return lingo.User{}, errors.New("unauthenticated")
			}
			return lingo.User{ID: id, Role: lingo.RoleExternal}, nil
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/lingo/", http.StripPrefix("/lingo", l.Handler()))
	mux.Handle("/mcp", l.MCPHandler())
	mux.Handle("/webhooks/gitea", l.WebhookHandler())
	go func() { _ = l.Run(ctx) }()

	srv := &http.Server{Addr: ":8080", Handler: mux}
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
	log.Print("listening on :8080")
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
