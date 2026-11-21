# Lingo

<p align="center"><img src="assets/logo/lingo-icon-color.svg" width="96" alt="Lingo logo"></p>

English | [简体中文](README.zh-CN.md)

Lingo is an embeddable Go library that connects end users with an AI development team working in Gitea. Users submit questions, bug reports and feature requests from your application; Lingo turns them into structured Gitea issues, keeps both sides in sync, and reports progress back to your application until the fix is released and deployed.

Lingo provides public APIs only (Go methods, a REST handler, an MCP server and a Gitea webhook handler). It has no multi-tenancy and no frontend: login, user management, UI and deployment stay with the host application.

> **Status:** the ticket workflow is implemented end to end. The knowledge Q&A agent (`Ask`, knowledge base) is not implemented yet. The API may still change before the first tagged release.

## Features

- **Feedback tickets** — consult / bug / feature with text, form fields, screenshots and attachments (lingo stores references only; files live in the host's own attachment system and are uploaded to Gitea via the optional `Config.FetchAttachment`, which receives the submitter's ID so the host can check ownership); APIs designed for a popup widget built by the host
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

// Read-only readiness check (creates nothing, writes nothing), e.g. to disable only the feedback entry:
st, err := lingo.CheckMigrations(ctx, db) // err != nil: check failed, state unknown; err == nil && !st.Ready: not migrated (st.Pending / st.MissingTables …)

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

Your `Authenticate` maps each user to one of four fixed roles: `external` (end users: their own tickets only), `agent` (AI developer agents: forwarded tickets in their projects), `reviewer` (triage and all tickets in their projects) and `admin`. `User.Projects` optionally limits the projects a user can access. `User.Perspective` is an optional requirement perspective unrelated to permissions; see below.

### Requirement perspectives

A requirement (`feature` ticket) can be supplemented by people in different positions: the user (`user`: symptoms and needs), the business side (`business`: scope and impact), experts (`expert`: proposed solutions) and decision makers (`leader`: go/no-go and priority). Once forwarded, the issue body shows the supplements grouped by perspective (each with name and time) and gets `<prefix>/视角/<name>` labels; later supplements make the bot rewrite that block and post a short notice comment. Perspectives only classify content and grant no permissions.

Integration takes four steps (full examples in [example_perspective_test.go](example_perspective_test.go) and [examples/server](examples/server/main.go)):

```go
lingo.Config{
	// 1. Map the perspective from your own user model in Authenticate (empty means user)
	Authenticate: func(r *http.Request) (lingo.User, error) {
		u := currentUser(r)
		return lingo.User{ID: u.ID, Name: u.Name, Role: lingo.RoleExternal, Perspective: perspectiveOf(u)}, nil
	},
	// 2. Who may add to (write) or read which requirement. If unset, normal ticket visibility applies: external
	//    users only their own, agents only forwarded ones, reviewer / admin everything in their projects.
	//    Allow business people and others to join other users' requirements here
	AuthorizePerspective: func(ctx context.Context, u lingo.User, t lingo.Ticket, write bool) error {
		if u.Perspective != "" || t.UserID == u.ID {
			return nil
		}
		return lingo.ErrNotFound // or lingo.ErrForbidden
	},
	// 3. Required perspectives (optional): forwarding without them emits ticket.perspective_missing and the issue
	//    block notes what is missing; Project.RequiredPerspectives overrides, an empty slice disables the check
	RequiredPerspectives: []lingo.Perspective{lingo.PerspectiveBusiness, lingo.PerspectiveLeader},
	// 4. In your Notifier, use Event.Perspectives to decide whom to notify
}
```

Calls (Go, REST and MCP share the same checks):

| Go | REST | MCP |
|---|---|---|
| `l.AddPerspective(ctx, u, id, content)` | `POST /tickets/{id}/perspectives` `{"content":"…"}` → 201 | `add_perspective {id, content}` |
| `l.ListPerspectives(ctx, u, id)` | `GET /tickets/{id}/perspectives` → 200 | `list_perspectives {id}` |

```json
// POST returns the supplement
{"id": 7, "perspective": "business", "author_name": "Zhang San", "content": "Affects 30 stores", "at": "2026-11-21T09:30:00Z"}
// GET returns supplements grouped by perspective (fixed order, non-empty groups only) and missing required perspectives;
// the first entry is the original submission (original, id 0)
{"ticket_id": 42,
 "groups": [{"perspective": "user", "title": "使用者反馈", "entries": [{"id": 0, "perspective": "user", "author_name": "Wang Wu", "content": "Export to Excel", "at": "…", "original": true}]},
            {"perspective": "business", "title": "业务补充", "entries": [{"id": 7, "…": "…"}]}],
 "missing": ["leader"]}
```

Errors: `invalid_input` (400: not a feature ticket, invalid perspective, empty content or over 10,000 characters), `forbidden` (403), `not_found` (404: missing, outside the user's projects or not visible) and `conflict` (409: ticket answered, merged, rejected or closed).

Notes:

- Supplements are append-only; fixed, released and deployed requirements still accept them.
- Contributor and submitter names (`User.Name`) are written to the Gitea issue; emails and user IDs are not.
- `ticket.perspective_added` (`Perspectives` holds the contributor's perspective, `Body` a notice without the content) and `ticket.perspective_missing` (`Perspectives` holds the missing ones) follow the visibility of other ticket events.
- The bot token must be able to create labels (`write:issue`); perspective labels are created by the startup self-check.

### REST API

| Endpoint | Description |
|---|---|
| `GET /projects`, `GET /ticket-types` | Projects and repositories; ticket types with issue-template form fields |
| `POST /tickets`, `GET /tickets/mine`, `GET /tickets/{id}`, `POST /tickets/{id}/comments` | Submit, list, view (with comments and attachments) and comment |
| `POST` / `GET /tickets/{id}/perspectives` | Requirement perspectives: add, list grouped by perspective (see above) |
| `GET /triage`, `GET /triage/summary`, `POST /tickets/{id}/forward\|answer\|merge\|reject` | Triage (reviewer / admin) |
| `POST /tickets/{id}/reject-forwarded`, `GET /tickets/{id}/issue-close`, `POST /tickets/{id}/issue-close/retry` | Reject a forwarded ticket (closes only its linked issue, async and retryable), close progress / retry (reviewer / admin) |
| `GET /events?after=` | Event feed (same events as the `Notifier`) |
| `GET /releases`, `GET /releases/{id}` | Releases with linked tickets and deployments |
| `GET` / `PUT /settings/auto-forward` | Auto-forward switches (changes: admin) |
| `GET /usage?from=&to=` | Model token usage by day, purpose and model, with prompt-cache hit rate (admin) |

Write requests must use `Content-Type: application/json` (CSRF protection); errors are `{"error":{"code","message"}}`.

### MCP tools

`list_projects`, `list_ticket_types`, `create_ticket`, `get_ticket`, `list_my_tickets`, `reply_ticket`, `add_perspective`, `list_perspectives`, `list_events`, `list_triage`, `forward_ticket`, `answer_ticket`, `merge_ticket`, `reject_ticket`, `reject_forwarded_ticket`, `get_issue_close`, `retry_issue_close`, `list_releases`, `get_release`, `get_llm_usage`.

### Events

`ticket.created`, `ticket.status`, `ticket.comment`, `ticket.scheduled`, `ticket.released`, `ticket.deployed`, `triage.required`, `triage.overdue`, `triage.resolved`, `ticket.perspective_added`, `ticket.perspective_missing`. Delivery is at-least-once in per-ticket order; de-duplicate by `Event.ID`.

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
