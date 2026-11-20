# Lingo

English | [简体中文](README.zh-CN.md)

Lingo is an embeddable Go library that connects end users with an AI development team working in Gitea. Users submit questions, bug reports and feature requests from your application; Lingo turns them into structured Gitea issues, keeps both sides in sync, and reports progress back to your application until the fix is released and deployed.

Lingo provides public APIs only (Go methods, a REST handler, an MCP server and a Gitea webhook handler). It has no multi-tenancy and no frontend: login, user management, UI and deployment stay with the host application.

> **Status:** the ticket workflow is implemented end to end. The knowledge Q&A agent (`Ask`, knowledge base) is not implemented yet. The API may still change before the first tagged release.

## Features

- **Feedback tickets** — consult / bug / feature with text, form fields, screenshots and attachments; APIs designed for a popup widget built by the host
- **Triage** — staff forward to Gitea, answer, merge or reject; auto/manual switch per project and ticket type; overdue reminders
- **Issue drafting** — an optional light model writes a clear title, fills the issue form fields from free text and tidies the description (validated against the template; the original text is kept)
- **Gitea integration** — issues created from the repository's issue form templates, two-way comments, PR-merge detection, release linking (by commit containment) and deployment signals (Actions workflow, commit status or `MarkDeployed`)
- **Reliable sync** — signed, de-duplicated webhooks processed asynchronously; periodic reconciliation recovers missed deliveries; startup self-check and optional webhook auto-registration
- **Progress notifications** — events delivered to your `Notifier` (at-least-once, with event IDs) plus an event feed; merged tickets follow their target
- **Data isolation** — everything is scoped by the caller's user ID, role and projects; the submitter's identity never goes to Gitea
- **MCP server** — the same capabilities as MCP tools for AI clients and AI developer agents (agents can reply to the submitter)

## Install

```sh
go get github.com/zhouhao4221/lingo
```

Requirements: Go 1.24+, PostgreSQL 13+, Gitea 1.22+ (1.25+ to use Gitea Actions as a deployment signal).

## Usage

```go
import _ "github.com/jackc/pgx/v5/stdlib" // or any database/sql PostgreSQL driver

if err := lingo.Migrate(ctx, db); err != nil { ... } // run in your release process

l, err := lingo.New(lingo.Config{
	DB:           db,       // *sql.DB (PostgreSQL)
	LightModel:   light,    // optional: a cheap, fast lingo.Model that tidies tickets before issues are created
	Gitea:        lingo.GiteaConfig{BaseURL: url, Token: botToken, WebhookSecret: secret},
	Projects:     projects, // projects and their Gitea repositories
	Authenticate: authn,    // resolves lingo.User (ID, role, projects) from a request
	Notifier:     notifier, // optional: receives ticket progress events
})

mux.Handle("/lingo/", http.StripPrefix("/lingo", l.Handler())) // REST JSON
mux.Handle("/mcp", l.MCPHandler())                             // MCP (Streamable HTTP)
mux.Handle("/webhooks/gitea", l.WebhookHandler())              // Gitea webhook
go l.Run(ctx)                                                  // background workers
```

Built-in models (standard library HTTP, no SDK): `lingo.NewOpenAIModel(lingo.OpenAIConfig{...})` for OpenAI or any OpenAI-compatible API (gateways, vLLM, Ollama `/v1`), and `lingo.NewDeepSeekModel(apiKey, "")` for DeepSeek. You can also implement `lingo.Model` yourself.

```go
light, _ := lingo.NewDeepSeekModel(os.Getenv("DEEPSEEK_API_KEY"), "")
// or: lingo.NewOpenAIModel(lingo.OpenAIConfig{BaseURL: "https://gateway.example.com/v1", APIKey: key, Model: "your-model"})
```

See [examples/server](examples/server/main.go) for a complete example.

### Roles

Your `Authenticate` maps each user to one of four fixed roles: `external` (end users: their own tickets only), `agent` (AI developer agents: forwarded tickets in their projects), `reviewer` (triage and all tickets in their projects) and `admin`. `User.Projects` optionally limits the projects a user can access.

### REST API

| Endpoint | Description |
|---|---|
| `GET /projects`, `GET /ticket-types` | Projects and repositories; ticket types with issue-template form fields |
| `POST /attachments` (multipart `file`), `GET /attachments/{id}` | Upload and download attachments |
| `POST /tickets`, `GET /tickets/mine`, `GET /tickets/{id}`, `POST /tickets/{id}/comments` | Submit, list, view (with comments and attachments) and comment |
| `GET /triage`, `GET /triage/summary`, `POST /tickets/{id}/forward\|answer\|merge\|reject` | Triage (reviewer / admin) |
| `GET /events?after=` | Event feed (same events as the `Notifier`) |
| `GET /releases`, `GET /releases/{id}` | Releases with linked tickets and deployments |
| `GET` / `PUT /settings/auto-forward` | Auto-forward switches (changes: admin) |
| `GET /usage?from=&to=` | Model token usage by day, purpose and model, with prompt-cache hit rate (admin) |

Write requests must use `Content-Type: application/json` (CSRF protection); errors are `{"error":{"code","message"}}`.

### MCP tools

`list_projects`, `list_ticket_types`, `create_ticket`, `get_ticket`, `list_my_tickets`, `reply_ticket`, `list_events`, `list_triage`, `forward_ticket`, `answer_ticket`, `merge_ticket`, `reject_ticket`, `list_releases`, `get_release`, `get_llm_usage`.

### Events

`ticket.created`, `ticket.status`, `ticket.comment`, `ticket.scheduled`, `ticket.released`, `ticket.deployed`, `triage.required`, `triage.overdue`, `triage.resolved`. Delivery is at-least-once in per-ticket order; de-duplicate by `Event.ID`.

### Gitea setup

- Create a bot account and token with `write:issue`, `read:repository` and `read:user` (plus `write:repository` for `AutoRegisterWebhook`).
- Point a webhook at `WebhookHandler` with the shared secret, or set `AutoRegisterWebhook` and `WebhookURL`. If Lingo runs on a private network, allow it in Gitea's `[security] ALLOWED_HOST_LIST`.
- Developers reply to the submitter with comments starting with `/reply` (or via the MCP `reply_ticket` tool) and close issues from PRs with `Fixes #N`.
- `l.SelfCheck(ctx)` (also run at startup) reports version, permission, label, webhook, template and workflow problems.

## Layout

```
*.go                 public API (root package lingo)
internal/            implementation (see docs/architecture/overview.md)
migrations/          database migrations (embedded with go:embed)
examples/            integration examples
docs/                requirements and architecture docs (Chinese)
tests/               integration tests (separate module, needs PostgreSQL)
```

## License

[Apache License 2.0](LICENSE)
