# Lingo

<p align="center"><img src="assets/logo/lingo-icon-color.svg" width="96" alt="Lingo logo"></p>

English | [简体中文](README.zh-CN.md)

Lingo is an embeddable Go library that connects end users with an AI development team working in Gitea. Users submit questions, bug reports and feature requests from your application; Lingo turns them into structured Gitea issues, keeps both sides in sync, and reports progress back to your application until the fix is released and deployed.

Lingo provides public APIs only (Go methods, a REST handler, an MCP server and a Gitea webhook handler). It has no multi-tenancy and no frontend: login, user management, UI and deployment stay with the host application.

> **Status:** the ticket workflow is implemented end to end. Knowledge Q&A (questions about tickets and releases) and the knowledge base (entries from answers and comments, extraction from closed tickets, answer feedback, review, search) are implemented. The API may still change before the first tagged release.

## Features

- **Feedback tickets** — consult / bug / feature with text, form fields, screenshots and attachments (lingo stores references only; files live in the host's own attachment system and are uploaded to Gitea via the optional `Config.FetchAttachment`, which receives the submitter's ID so the host can check ownership); APIs designed for a popup widget built by the host
- **Triage** — staff forward to Gitea, answer, merge or reject; auto/manual switch per project and ticket type; overdue reminders
- **Issue drafting** — an optional light model writes a clear title, fills the issue form fields from free text and tidies the description (validated against the template; the original text is kept)
- **Gitea integration** — issues created from the repository's issue form templates, two-way comments, PR-merge detection, release linking (by commit containment) and deployment signals (Actions workflow, commit status or `MarkDeployed`)
- **Reliable sync** — signed, de-duplicated webhooks processed asynchronously; periodic reconciliation recovers missed deliveries; startup self-check and optional webhook auto-registration
- **Progress notifications** — events delivered to your `Notifier` (at-least-once, with event IDs) plus an event feed; merged tickets follow their target
- **Data isolation** — everything is scoped by the caller's user ID, role and projects; the submitter's identity never goes to Gitea
- **Knowledge Q&A** — ask about ticket progress, fix versions and recent releases in plain language, with cited tickets and releases; commenting from the chat requires confirmation; conversations are private to their owner and answers can stream (see "Knowledge Q&A" below)
- **Business data tools** — register read-only tools; the Q&A assistant and MCP query business data as the asker, with scope checks, masking and an audit trail (see "Business data tools")
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

Your `Authenticate` maps each user to one of four fixed roles: `external` (end users: their own tickets only, optionally widened read-only to teammates' with `TicketAudience`; see below), `agent` (AI developer agents: forwarded tickets in their projects), `reviewer` (triage and all tickets in their projects) and `admin`. `User.Projects` optionally limits the projects a user can access. `User.Perspective` is an optional requirement perspective unrelated to permissions; see below.

### Teammate visibility (TicketAudience)

By default an `external` user sees only their own tickets. Set `Config.TicketAudience` to let them also see tickets from certain submitters, by your own rules (same department, same team):

```go
TicketAudience: func(ctx context.Context, u lingo.User) (lingo.TicketAudience, error) {
	return lingo.TicketAudience{UserIDs: directory.Teammates(u.ID)}, nil // your org data
},
```

- **Read-only, within `User.Projects`**: viewing tickets and public comments, `ListVisibleTickets` (REST `GET /tickets/visible`, MCP `list_visible_tickets`), Q&A lookups and summaries, and a release's tickets follow it. Tickets carry `mine` to tell the caller's own apart.
- **Not covered**: comments, attachments added later, perspectives and confirmed comment actions (teammates get `not_found`); attachments, `ListEvents` / `Notifier` (submitter only); requirement perspectives (when `AuthorizePerspective` is unset).
- **Merged tickets** go by submitter: a teammate's ticket merged into someone else's does not make the target visible.
- **Q&A history**: reopening a conversation re-checks cited tickets against the current audience; ones no longer visible keep only their number. Answer text already generated is left as is.
- Called only for `external` users with an ID, at most once per read request, never cached; IDs are de-duplicated and capped at 1,000; on error Lingo falls back to own tickets only and logs a warning.
- This is a privacy change: tell users before enabling it, e.g. "Members of your department can see the tickets you submit (title, description, progress and public replies). Attachments stay visible only to you and the people handling them."

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
- What the model receives: the question and the last 10 turns; each ticket's number, project, repository, type, title, status, timestamps, fix version, milestone and a body excerpt; the 5 most recent comment bodies of a single ticket (within the asker's visibility, so internal developer discussion for reviewers and admins); release names, note excerpts, deployments and fixed tickets; for how-to questions, the matching knowledge entries (question and an answer excerpt). Names, emails and user IDs of submitters and comment authors are never sent (exception: with `TicketAudience`, a teammate's ticket carries the submitter's name; see Teammate visibility), nor are form fields, submission context or ticket attachments. You supply the model, so choose one that meets your compliance needs.
- Image questions (REQ-009): `MessageInput.Attachments` (REST `attachments`, MCP `ask`) carries up to 4 PNG / JPEG / WebP references (ID, name and type only, no URL; image-only questions are allowed). lingo reads them through `Config.FetchAttachment` with `Attachment.Purpose == "question"` and the asker as `userID` — check that the asker uploaded the file and that it may be sent to the model service. After checking the file header, size (5 MB) and dimensions, lingo sends the images of the current question to intent parsing and answering in the OpenAI-compatible multimodal format; image content is not stored, forwarded to Gitea or added to the knowledge base. The model must declare image support (implement `lingo.ImageModel`, set `OpenAIConfig.Images`, or tick `images` in connection settings); otherwise image questions fail with `ErrImagesUnsupported` (REST 501 `images_unsupported`) instead of silently dropping the images. **Images go to the configured model service; confirm what may leave and which provider is acceptable before enabling.**
- Writes need confirmation: when the user asks to comment, the assistant message carries a `pending_action`. Show confirm / cancel buttons (call confirm / cancel), or let the user simply reply "confirm". It is valid for 30 minutes and is dropped on cancel, on an unrelated message or on timeout.
- Conversations are visible only to their owner (not even to admins). Users can continue or delete them. Lingo never purges them; retention is up to you.
- Requires `Config.Model` (intent parsing uses `LightModel`, falling back to `Model`). Without it, sending returns `ErrNoModel` (REST 501). Models implementing `lingo.StreamModel` stream the answer; the built-in models do (note that `OpenAIConfig.HTTPClient`'s timeout bounds the whole answer, 90 s by default).
- Answers questions about tickets, releases and how-to / why / what-to-do questions (see Knowledge base below), and questions about the asker's own business data through registered tools (see Business data tools).

```go
c, _ := l.CreateConversation(ctx, u, "")
m, err := l.SendMessageStream(ctx, u, c.ID, lingo.MessageInput{Content: "Has my login bug been fixed?", TimeZone: "Asia/Shanghai"},
	func(delta string) { fmt.Print(delta) }) // streamed text; m.Content is authoritative
if m.PendingAction != nil { /* show a confirm button that calls l.ConfirmPendingAction(ctx, u, c.ID, m.PendingAction.ID) */ }
```

REST streaming: `POST /conversations/{id}/messages` with `{"content":"…","time_zone":"Asia/Shanghai","stream":true}` returns SSE: any number of `event: delta` (`{"text"}`) followed by exactly one `event: message` carrying the full assistant message, whose `content` is authoritative. Errors after the stream starts arrive as `event: error` (`{"code","message"}`); errors before it (404, 409, 501) are regular JSON errors. A message whose model call failed has `status: "failed"` and can be retried. Read the stream with `fetch` (`EventSource` cannot POST).

### Knowledge base

How-to questions ("why is the exported report empty", "how do I set up SSO") are answered from the knowledge base: Lingo searches active entries in the asker's projects (up to 5) plus similar tickets the asker can see, and the model answers from them. Answers list the entries in `citations.knowledge` (ID and question; the source ticket only when the asker can view it), and each cited entry's cite count goes up. With no match, Lingo says the knowledge base has nothing on it yet and suggests a consult ticket.

- **Adding entries**: reviewers and admins can add their answer to the knowledge base when answering a pending ticket (`AnswerTicketWith`, REST `POST /tickets/{id}/answer` with `knowledge`, MCP `answer_ticket` with `add_to_knowledge`); it is off by default. They can also create entries by hand (`CreateKnowledge`). Entries go live at once and anyone with access to the project may find them, so **rewrite the question and answer in general terms and drop anything specific to the submitter**. Emails and phone numbers are replaced with placeholders.
- **Maintenance** (reviewers / admins, within `User.Projects`): `ListKnowledge` (sortable by cite count), `UpdateKnowledge`, `DisableKnowledge` / `EnableKnowledge`, `DeleteKnowledge`, `DisableKnowledgeByTicket` (disables entries derived from a ticket, e.g. when its submitter asks for their data to be removed).
- **Search** (all roles): `SearchKnowledge` (REST `GET /knowledge/search`, MCP `search_knowledge`). No database extension needed: Chinese text is split into bigrams and ranked by overlap with a GIN array index, with substring matching as a fallback.
- **Vectors (optional)**: when the main model's `Embed` works, `Run` generates vectors in the background (for new or edited entries, or after the embedding model changes), and search re-ranks the top 50 by similarity. Vectors are told apart by model name when the model implements `lingo.EmbeddingModelNamer` (the built-in adapters do; DeepSeek and an OpenAI adapter without `EmbeddingModel` produce none), otherwise by dimension. Each search then makes one extra `Embed` call and falls back to bigram ranking on error.
- **Growth and review** (REQ-007):
  - Reviewers and admins can also add a public ticket comment to the knowledge base (`AddTicketCommentWith`, REST `POST /tickets/{id}/comments` with `knowledge`, MCP `reply_ticket` with `add_to_knowledge`); it goes live at once.
  - When a bug or feature ticket first becomes released, deployed or closed (not fixed), `Run` uses the light model to extract one Q&A as **pending review**. Each ticket is extracted at most once, and nothing is extracted without a model. **Tickets already closed when you upgrade to this version are not extracted.**
  - Users rate answers as helpful or not (`RateMessage`, REST `POST /conversations/{id}/messages/{mid}/feedback`, MCP `rate_message`). Cited entries get the counts. The first time an answer to a how-to question is rated helpful, the question and answer (with ticket and knowledge numbers removed, scrubbed) become a **pending** candidate.
  - Review (reviewers / admins): list pending entries with `ListKnowledge` (`Status: KnowledgePending`), then `ApproveKnowledge` (optionally with edits) or `RejectKnowledge`. New entries overlapping an active one by 60% or more carry `duplicate_of`, as a hint only. Pending and rejected entries are never searched or used in answers.
  - Unanswered questions: how-to questions with no match are recorded (scrubbed) once per project the asker can access, similar ones in the same project within 90 days are merged and counted, and `ListUnansweredQuestions` (REST `GET /knowledge/unanswered`) lists them by count so you know what to add.
- Search only returns entries whose overlap is at least 20% of the shorter of the query and the entry, so common words such as "怎么" alone do not pull in unrelated entries while long questions still find short entries.
- Usage: answers are recorded as `knowledge_answer`, embeddings as `knowledge_embed` (call counts only), extraction as `knowledge_extract`. Requires migrations `0005_knowledge.sql` and `0006_knowledge_review.sql`.

### Business data tools

Register read-only business data tools and Lingo can answer questions such as "why was order 1024 rejected" or "how many leave days do I have left": the Q&A assistant picks one tool when needed, calls it **as the asker**, and answers from the result (`citations.tools` carries the tool name only); MCP clients see the tools they may call in `tools/list`; your own UI can call `CallTool`.

```go
orderTool := lingo.Tool{
	Name:        "get_order",
	Description: "Look up an order's status and approval history by order number",
	InputSchema: json.RawMessage(`{"type":"object","required":["order_id"],
		"properties":{"order_id":{"type":"string","description":"order number"}}}`),
	Scope:     "order:read",               // only users whose Scopes contain it can call it
	Mode:      lingo.ToolRead,             // only read tools are called for now
	Sensitive: []string{"customer.phone"}, // replaced with *** in results and audit arguments
	Timeout:   5 * time.Second,            // 0 means 10s; at most 60s
	Call: func(ctx context.Context, u lingo.User, in json.RawMessage) (any, error) {
		var p struct {
			OrderID string `json:"order_id"`
		}
		if err := json.Unmarshal(in, &p); err != nil {
			return nil, err
		}
		o, err := orders.Get(ctx, p.OrderID) // honour ctx cancellation
		switch {
		case errors.Is(err, orders.ErrNotExist):
			return nil, lingo.ErrNotFound // no data: a normal business outcome, not counted by the breaker
		case err != nil:
			return nil, errors.New("order lookup failed") // technical failure; keep business data out of errors
		}
		if o.OwnerID != u.ID && !canViewAllOrders(u) { // you must authorize the given user again
			return nil, lingo.ErrForbidden // business denial: a normal outcome, not counted by the breaker
		}
		// Optional: declare which project the data belongs to, so that project's reviewers / admins see the call in the audit
		return lingo.ToolOutput{Project: o.ProjectKey, Data: o}, nil
	},
}

leaveTool := lingo.Tool{
	Name: "get_leave_balance", Description: "The caller's remaining annual leave and time off in lieu",
	InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	Scope: "leave:read", Mode: lingo.ToolRead,
	Call: func(ctx context.Context, u lingo.User, _ json.RawMessage) (any, error) {
		return leave.Balance(ctx, u.ID) // the caller's own data only
	},
}

l, err := lingo.New(lingo.Config{ /* … */ Tools: []lingo.Tool{orderTool, leaveTool}})
```

- **Permissions**: Lingo is only the first line of defence — a tool is visible and callable only when `User.Scopes` contains `Tool.Scope` (the model never sees tools the asker may not use). **`Call` must authorize and filter by the `lingo.User` it receives**; Lingo never uses a privileged account. Missing permission, an unknown tool and a declared project the caller cannot access all return the same `ErrNotFound` (`unknown tool` over MCP).
- **Registration checks**: `Name` uses letters, digits and underscores (at most 64), `Mode` is `ToolRead`, `Scope` is non-empty without whitespace or control characters (including leading / trailing spaces), `InputSchema` is a JSON object, `Call` is set and `Timeout` is between 0 and 60 seconds. Tools that fail are **ignored with a Warn log** (`New` does not fail); a tool named like a built-in MCP tool or an earlier tool (case-insensitive) is ignored and never replaces it. After upgrading, check the startup log for `Config.Tools entry ignored`.
- **Arguments**: the input must be one complete JSON object (anything after it is an argument error and `Call` is not invoked); only `required` and the basic `properties` types are checked, and `integer` follows Go's standard integer decoding (plain decimal integers only — `3.0` and `1e3` are rejected because `json.Unmarshal` into an `int` would fail in your `Call`), with no type conversion; the assistant asks for a missing value using the property `description` and says plainly when a value has the wrong format; MCP / Go return an argument error naming the parameter and expected type only.
- **Results** are masked by `Sensitive` paths (`phone`, `customer.phone`, `items.*.price`; key names match case-insensitively) and truncated beyond 8 KB (`truncated`). **Raw results are never stored in conversations, the knowledge base, the audit or logs**; the answer text is kept like any other message. Answers based on business data never become knowledge candidates.
- **Timeouts, concurrency and circuit breaking**: 10 seconds by default; a `Call` still running after its timeout keeps its slot (8 per tool, 64 in total) and `ErrToolUnavailable` is returned when slots run out; **a `Call` returning `lingo.ErrNotFound` (no data) or `lingo.ErrForbidden` (denied by your business rules) is a normal business outcome**: it is audited, not counted by the breaker, and the caller gets the same result as for an unknown tool; any other error is a technical failure and counts; five consecutive failures within five minutes pause a tool for one minute (per instance); calls that started before a pause and return late never close or reopen it. A result that arrives after the deadline is discarded as a timeout. Encoding or masking failures count as failures and are recorded without their error text.
- **Audit**: every call (including MCP calls the caller may not make) first writes a record (no record, no call) with the user, tool, channel, argument summary (only parameters declared in `InputSchema`: names plus truncated, masked values; denied calls keep names only; undeclared fields are not recorded), status, reason and duration — never the result or error text — kept for 180 days (purged by the database clock). `ListDataRequests` (REST `GET /data-requests`): everyone sees their own records; reviewers / admins also see records whose project is **explicitly listed** in their `User.Projects`. An empty `User.Projects` grants **no** cross-user audit access, and records without a declared project are visible to the caller only.
- **Compliance**: business data is sent to the model you configure to compose the answer, so pick a model service that meets your data rules and mask fields that must not leave with `Sensitive`. Tool results are treated as data, not instructions, but avoid returning untrusted long text.
- Errors: `ErrToolUnavailable` (REST 503 `tool_unavailable`) and `ErrToolFailed` (timeout, `Call` error or panic; REST 502 `tool_failed`), never with the underlying error text.
- Requires migration `0007_data_requests.sql`. `Run` marks expired pending records as interrupted and purges records older than 180 days even when no tools are registered. Read-only for now; write tools and attaching business data to tickets come later.
- **Behaviour changes when upgrading**:
  1. Tools already listed in `Config.Tools` (previously never called) are now **really called** by Q&A, MCP and `CallTool` — real business queries and audit records — once the asker's `User.Scopes` contains their `Scope`.
  2. Tools that fail the registration checks (including `write` tools, an empty `Scope` or `Timeout` above 60 seconds) are **ignored with a Warn log** instead of being kept; `New` still succeeds. Check the startup log for `Config.Tools entry ignored`.

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
| `GET /knowledge/search?q=&project=&limit=`; `GET` / `POST /knowledge`, `PUT` / `DELETE /knowledge/{id}`, `POST /knowledge/{id}/disable\|enable`, `POST /knowledge/disable-by-ticket`, `POST /knowledge/{id}/approve\|reject`, `GET /knowledge/unanswered` | Knowledge base: search (all roles); maintenance and review (reviewers / admins) |
| `GET /data-requests?tool=&user_id=&status=&since=&until=&before=&limit=` | Business data tool call audit: your own, plus projects reviewers / admins are explicitly responsible for |
| `GET /tickets/visible?project=&status=&before=` | Tickets the caller can view: their own plus teammates' via `TicketAudience` (read-only) |
| `GET /tickets?project=&repo=&type=&status=a,b&q=&mine=&shared=&time_field=&from=&to=&before=&limit=` | Feedback management list (Go `ListTickets`): returns `{tickets, total, next_before}`; externals see only their own by default (`shared=true` adds teammates'), agents only forwarded tickets, reviewers / admins everything within `User.Projects` (all of `Config.Projects` when `User.Projects` is empty). `status` is comma-separated (any of); `q` is split on whitespace (at most 10 words, 100 chars each); `mine` / `shared` accept only `true`\|`false`\|`1`\|`0`; `time_field` defaults to `created`, or `updated`\|`forwarded`; `from` / `to` are RFC 3339, `to` exclusive; `before` is the previous page's `next_before`; `limit` defaults to 50, max 200; invalid values return 400 |
| `POST` / `GET /tickets/{id}/perspectives` | Requirement perspectives: add, list grouped by perspective (see above) |
| `GET /triage`, `GET /triage/summary`, `POST /tickets/{id}/forward\|answer\|merge\|reject` | Triage (reviewer / admin) |
| `POST /tickets/{id}/comments/resync` | Re-sync a ticket's main-issue Gitea comments under the current `ReplyMode` (Go `ResyncTicketComments`, reviewer / admin within `User.Projects`), e.g. to backfill comments stored as internal before switching to `ReplyAll`; same handling as webhooks (bot comments skipped, dedup, edits, locally deleted stay deleted), silent by default (body `{"notify":true}` notifies as usual), idempotent, reads Gitea only; returns counts |
| `POST /tickets/{id}/reject-forwarded`, `GET /tickets/{id}/issue-close`, `POST /tickets/{id}/issue-close/retry` | Reject a forwarded ticket (closes only its linked issue, async and retryable), close progress / retry (reviewer / admin) |
| `GET /events?after=` | Event feed (same events as the `Notifier`) |
| `GET /releases`, `GET /releases/{id}` | Releases with linked tickets and deployments |
| `GET` / `PUT /settings/auto-forward` | Auto-forward switches (changes: admin) |
| `GET /usage?from=&to=` | Model token usage by day, purpose and model, with prompt-cache hit rate (admin) |
| `POST` / `GET /conversations`, `DELETE /conversations/{id}` | Q&A conversations: create, list (by recent activity; pass the previous page's `cursor` as `before`), delete |
| `GET` / `POST /conversations/{id}/messages`, `POST /conversations/{id}/messages/{mid}/retry`, `POST /conversations/{id}/messages/{mid}/feedback` | History; ask (SSE when `stream` is true); retry a failed answer; rate an answer |
| `POST /conversations/{id}/actions/{aid}/confirm\|cancel` | Confirm / cancel a pending action (such as a comment) |
| `GET` / `PUT /settings/connections`, `POST /settings/connections/test` | Connection settings (model and Gitea): view (secrets masked), update by field, test (access per `Config.AuthorizeSettings`) |

Write requests must use `Content-Type: application/json` (CSRF protection); errors are `{"error":{"code","message"}}`.

### MCP tools

`list_projects`, `list_ticket_types`, `create_ticket`, `get_ticket`, `list_my_tickets`, `list_visible_tickets`, `reply_ticket`, `add_perspective`, `list_perspectives`, `list_events`, `list_triage`, `forward_ticket`, `answer_ticket`, `merge_ticket`, `reject_ticket`, `reject_forwarded_ticket`, `get_issue_close`, `retry_issue_close`, `list_releases`, `get_release`, `get_llm_usage`, `ask`, `list_conversations`, `list_conversation_messages`, `confirm_action`, `cancel_action`, `rate_message`, `search_knowledge`; plus the business data tools the caller may call (`Config.Tools`, filtered by `User.Scopes`).

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
