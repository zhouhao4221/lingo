# Lingo

English | [简体中文](README.zh-CN.md)

Lingo is an embeddable Go library for a knowledge Q&A agent. Users ask questions at any time and Lingo answers from the knowledge base whenever it can. Questions it cannot answer become structured Gitea issues for an AI development team; progress is reported back to the host application, and every resolved Q&A flows back into the knowledge base.

Lingo provides public APIs only. It has no multi-tenancy and no frontend: login, user management, UI and deployment are left to the host application.

> **Status:** early stage. The public API is being designed and most methods are not implemented yet.

## Features

- **Knowledge Q&A agent** — retrieval-augmented answers with sources; asks follow-up questions and escalates to a ticket when it cannot answer
- **Feedback tickets** — consult / bug / feature, with text, screenshots and attachments; APIs designed for a popup widget built by the host
- **Triage** — staff confirm before forwarding to Gitea, or answer, merge or reject; auto/manual switch per project and ticket type
- **Gitea integration** — issues created from the repository's issue form templates, two-way comments, PR-merge detection, release linking and deployment signals; signed, de-duplicated webhooks with periodic reconciliation
- **Progress notifications** — events delivered to your `Notifier` (at-least-once, with event IDs), plus query methods and an event feed
- **Data isolation** — tickets, conversations and attachments are scoped by the submitter's ID, role and project; personal data never goes to Gitea
- **MCP server** — the same capabilities exposed as MCP tools for AI clients and AI developer agents
- **Business data tools** — register Go functions that fetch data on behalf of the user, under that user's own permissions

## Install

```sh
go get github.com/zhouhao4221/lingo
```

## Usage

```go
if err := lingo.Migrate(ctx, db); err != nil { ... } // run once in your release process

l, err := lingo.New(lingo.Config{
	DB:           db,       // *sql.DB (PostgreSQL, bring your own driver)
	Model:        model,    // implements lingo.Model: Chat + Embed
	Gitea:        lingo.GiteaConfig{BaseURL: url, Token: token, WebhookSecret: secret},
	Projects:     projects, // projects and their Gitea repositories
	Authenticate: authn,    // resolves lingo.User from a request
	Notifier:     notifier, // optional: receives ticket progress events
	Tools:        tools,    // optional: fetch business data on behalf of users
})

mux.Handle("/lingo/", http.StripPrefix("/lingo", l.Handler())) // REST JSON
mux.Handle("/mcp", l.MCPHandler())                             // MCP
mux.Handle("/webhooks/gitea", l.WebhookHandler())              // Gitea webhook
go l.Run(ctx)                                                  // background workers
```

See [examples/server](examples/server/main.go) for a complete example.

Requirements: Go 1.24+, PostgreSQL, Gitea 1.22+ (1.25+ to use Gitea Actions as a deployment signal).

## Layout

```
*.go                 public API (root package lingo)
internal/            implementation (see docs/architecture/overview.md)
migrations/          database migrations (embedded with go:embed)
examples/            integration examples
docs/                requirements and architecture docs (Chinese)
tests/               integration tests
```

## License

[Apache License 2.0](LICENSE)
