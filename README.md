# Lingo

<p align="center"><img src="assets/logo/lingo-icon-color.svg" width="96" alt="Lingo logo"></p>

English | [简体中文](README.zh-CN.md)

Lingo is an embeddable Go library that connects end users with an AI development team working in Gitea. Users submit questions, bug reports and feature requests from your application; Lingo turns them into structured Gitea issues, keeps both sides in sync, and reports progress back to your application until the fix is released and deployed.

Lingo provides public APIs only (Go methods, a REST handler, an MCP server and a Gitea webhook handler). It has no multi-tenancy and no frontend: login, user management, UI and deployment stay with the host application.

> **Status:** the ticket workflow is implemented end to end. The first stage of knowledge Q&A (questions about tickets and releases) is implemented; knowledge-base search is not yet. The API may still change before the first tagged release.

## Features

- **Feedback tickets** — consult / bug / feature with text, form fields, screenshots and attachments (lingo stores references only; files live in the host's own attachment system and are uploaded to Gitea via the optional `Config.FetchAttachment`, which receives the submitter's ID so the host can check ownership); APIs designed for a popup widget built by the host
- **Triage** — staff forward to Gitea, answer, merge or reject; auto/manual switch per project and ticket type; overdue reminders
- **Issue drafting** — an optional light model writes a clear title, fills the issue form fields from free text and tidies the description (validated against the template; the original text is kept)
- **Gitea integration** — issues created from the repository's issue form templates, two-way comments, PR-merge detection, release linking (by commit containment) and deployment signals (Actions workflow, commit status or `MarkDeployed`)
- **Reliable sync** — signed, de-duplicated webhooks processed asynchronously; periodic reconciliation recovers missed deliveries; startup self-check and optional webhook auto-registration
- **Progress notifications** — events delivered to your `Notifier` (at-least-once, with event IDs) plus an event feed; merged tickets follow their target
- **Data isolation** — everything is scoped by the caller's user ID, role and projects; the submitter's identity never goes to Gitea
- **Knowledge Q&A** — ask about ticket progress, fix versions and recent releases in plain language, with cited tickets and releases; commenting from the chat requires confirmation; conversations are private to their owner and answers can stream (see "Knowledge Q&A" below)
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
	Model:        model,    // optional: writes knowledge Q&A answers (Q&A is unavailable without it)
	LightModel:   light,    // optional: a cheap, fast lingo.Model that tidies tickets and parses Q&A intents (falls back to Model)
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

### Knowledge Q&A (chat window)

Users can ask about their tickets and releases in plain language, for example "has the login bug I filed last week been fixed?", "what was released recently?" or "how many tickets in demo are waiting for triage?". They can also say "add to #12: reproduced on Chrome 120". We recommend building it as a separate chat window, with the conversation list on the left and the chat on the right.

- How it works: the model only turns the question into a query intent. Lingo runs the query with the caller's role and projects (same visibility as `GetTicket`), and the model writes the answer from the results. Answers carry structured `citations` (ticket ID, title, status, issue URL; release tag and publish time) that you can render as cards or links.
- What the model receives: the question and the last 10 turns; each ticket's number, project, repository, type, title, status, timestamps, fix version, milestone and a body excerpt; the 5 most recent comment bodies of a single ticket (within the asker's visibility, so internal developer discussion for reviewers and admins); release names, note excerpts, deployments and fixed tickets. Names, emails and user IDs of submitters and comment authors are never sent, nor are form fields, submission context or attachments. You supply the model, so choose one that meets your compliance needs.
- Writes need confirmation: when the user asks to comment, the assistant message carries a `pending_action`. Show confirm / cancel buttons (call confirm / cancel), or let the user simply reply "confirm". It is valid for 30 minutes and is dropped on cancel, on an unrelated message or on timeout.
- Conversations are visible only to their owner (not even to admins). Users can continue or delete them. Lingo never purges them; retention is up to you.
- Requires `Config.Model` (intent parsing uses `LightModel`, falling back to `Model`). Without it, sending returns `ErrNoModel` (REST 501). Models implementing `lingo.StreamModel` stream the answer; the built-in models do (note that `OpenAIConfig.HTTPClient`'s timeout bounds the whole answer, 90 s by default).
- This first version answers ticket and release questions only. Knowledge-base search and business-data `Tool`s come later.

```go
c, _ := l.CreateConversation(ctx, u, "")
m, err := l.SendMessageStream(ctx, u, c.ID, lingo.MessageInput{Content: "Has my login bug been fixed?", TimeZone: "Asia/Shanghai"},
	func(delta string) { fmt.Print(delta) }) // streamed text; m.Content is authoritative
if m.PendingAction != nil { /* show a confirm button that calls l.ConfirmPendingAction(ctx, u, c.ID, m.PendingAction.ID) */ }
```

REST streaming: `POST /conversations/{id}/messages` with `{"content":"…","time_zone":"Asia/Shanghai","stream":true}` returns SSE: any number of `event: delta` (`{"text"}`) followed by exactly one `event: message` carrying the full assistant message, whose `content` is authoritative. Errors after the stream starts arrive as `event: error` (`{"code","message"}`); errors before it (404, 409, 501) are regular JSON errors. A message whose model call failed has `status: "failed"` and can be retried. Read the stream with `fetch` (`EventSource` cannot POST).

### Connection settings (stored in the database)

Model and Gitea connection settings (addresses, tokens / API keys, webhook secret, webhook auto-registration, redirect rejection) can live in the database instead of code, editable from the host's settings page **without a restart**:

- `ConnectionSettings` / `UpdateConnectionSettings` / `TestConnectionSettings` (REST `GET` / `PUT /settings/connections`, `POST /settings/connections/test`); not exposed over MCP. Access is decided by `Config.AuthorizeSettings`; without it only admins may view or change them.
- **Precedence**: values given in `Config` win, show source `config` and return 409 if changed; the rest come from the database. Models are decided per slot (if `Config.Model` / `LightModel` is set, that slot comes from code). Booleans are locked by code only when `true`. `LabelPrefix` and `ReplyMode` are code-only.
- **Propagation**: the saving instance switches immediately; other instances within 30 seconds (`Run` polls; Handler-only processes check on incoming requests). In-flight requests keep the settings they started with.
- **Secrets are stored in plain text** in `lingo_settings`: anyone who can read that table (including backups and replicas) can read them, so restrict database and backup access. APIs only return masks (e.g. `sk-****abcd`); logs record only who changed which fields.
- **Changing an address requires re-entering the secret**: if the Gitea or model host changes, the token / API key must be submitted too, so a stored secret is never sent to a new host. Pointing at a different Gitea instance breaks existing ticket–issue links, and queued tasks or reconciliation may act on unrelated issues with the same numbers on the new instance — don't do it (address changes are meant for the same instance, e.g. a new domain or scheme).
- **Unconfigured state**: lingo starts without a Gitea address or token: webhooks return 503, Gitea tasks wait (without counting failures), and the self-check reports a `config` warning; everything resumes once configured. Webhooks Gitea failed to deliver meanwhile are not retried by Gitea and are recovered by reconciliation, which is impossible if reconciliation is disabled (`ReconcileInterval < 0`).
- **Changing the webhook secret**: lingo verifies with the new secret immediately. With auto-registration, a self-check re-runs and updates the secret in Gitea; otherwise (or with `SkipSelfCheck`) update it in Gitea by hand.
- Database-configured models support the built-in OpenAI-compatible (`openai`) and DeepSeek (`deepseek`) providers, without embeddings; custom `lingo.Model` implementations still go through `Config`. Requires migration 0004.

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
| `POST` / `GET /conversations`, `DELETE /conversations/{id}` | Q&A conversations: create, list (by recent activity; pass the previous page's `cursor` as `before`), delete |
| `GET` / `POST /conversations/{id}/messages`, `POST /conversations/{id}/messages/{mid}/retry` | History; ask (SSE when `stream` is true); retry a failed answer |
| `POST /conversations/{id}/actions/{aid}/confirm\|cancel` | Confirm / cancel a pending action (such as a comment) |
| `GET` / `PUT /settings/connections`, `POST /settings/connections/test` | Connection settings (model and Gitea): view (secrets masked), update by field, test (access per `Config.AuthorizeSettings`) |

Write requests must use `Content-Type: application/json` (CSRF protection); errors are `{"error":{"code","message"}}`.

### MCP tools

`list_projects`, `list_ticket_types`, `create_ticket`, `get_ticket`, `list_my_tickets`, `reply_ticket`, `add_perspective`, `list_perspectives`, `list_events`, `list_triage`, `forward_ticket`, `answer_ticket`, `merge_ticket`, `reject_ticket`, `reject_forwarded_ticket`, `get_issue_close`, `retry_issue_close`, `list_releases`, `get_release`, `get_llm_usage`, `ask`, `list_conversations`, `list_conversation_messages`, `confirm_action`, `cancel_action`.

### Events

`ticket.created`, `ticket.status`, `ticket.comment`, `ticket.scheduled`, `ticket.released`, `ticket.deployed`, `triage.required`, `triage.overdue`, `triage.resolved`, `ticket.perspective_added`, `ticket.perspective_missing`. Delivery is at-least-once in per-ticket order; de-duplicate by `Event.ID`.

### Gitea setup

- Create a bot account and token with `write:issue`, `read:repository` and `read:user` (plus `write:repository` for `AutoRegisterWebhook`).
- Point a webhook at `WebhookHandler` with the shared secret, or set `AutoRegisterWebhook` and `WebhookURL`. If Lingo runs on a private network, allow it in Gitea's `[security] ALLOWED_HOST_LIST`.
- Developers reply to the submitter with comments starting with `/reply` (or via the MCP `reply_ticket` tool) and close issues from PRs with `Fixes #N`.
- `l.SelfCheck(ctx)` (also run at startup) reports version, permission, label, webhook, template and workflow problems.
- Keep the token on `BaseURL`: with `GiteaConfig.RejectRedirects: true`, no Gitea request follows an HTTP redirect (same host, subdomain, cross-host and https→http downgrade are all refused) and the redirect is reported as a non-retryable configuration error. Off by default; behavior is unchanged.

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
