-- 0001 初始表结构（未发布前可修改；发布后结构变更一律新增迁移文件，见 PRD 5.4）
-- 无租户：lingo 嵌入单个业务项目使用；user_id / actor_id 为接入方自己的用户 ID（文本），lingo 不保存用户表
-- 时间列一律 timestamptz；可选文本列用 '' 默认值而非 NULL，可选关联用 NULL

-- 项目与仓库：由 Config.Projects 同步
CREATE TABLE projects (
    id              bigserial PRIMARY KEY,
    key             text        NOT NULL UNIQUE,
    name            text        NOT NULL,
    description     text        NOT NULL DEFAULT '',
    default_repo_id bigint,
    enabled         boolean     NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE repos (
    id         bigserial PRIMARY KEY,
    project_id bigint      NOT NULL REFERENCES projects (id),
    owner      text        NOT NULL,
    name       text        NOT NULL,
    kind       text        NOT NULL DEFAULT 'other'
               CHECK (kind IN ('frontend', 'backend', 'mobile', 'docs', 'other')),
    enabled    boolean     NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
-- Gitea 的 owner / 仓库名不区分大小写；一个仓库只属于一个项目
CREATE UNIQUE INDEX repos_owner_name_key ON repos (lower(owner), lower(name));
CREATE INDEX repos_project_id_idx ON repos (project_id);

ALTER TABLE projects
    ADD CONSTRAINT projects_default_repo_id_fkey FOREIGN KEY (default_repo_id) REFERENCES repos (id) ON DELETE SET NULL;

-- 运行时设置：自动转出开关的覆盖值（优先于 Project.AutoForward）
CREATE TABLE project_settings (
    project_id   bigint      NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    ticket_type  text        NOT NULL CHECK (ticket_type IN ('consult', 'bug', 'feature')),
    auto_forward boolean     NOT NULL,
    updated_by   text        NOT NULL DEFAULT '',
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, ticket_type)
);

-- 问答会话
CREATE TABLE conversations (
    id         bigserial PRIMARY KEY,
    user_id    text        NOT NULL,
    project_id bigint      NOT NULL REFERENCES projects (id),
    channel    text        NOT NULL CHECK (channel IN ('api', 'mcp', 'go')),
    status     text        NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved', 'escalated')),
    ticket_id  bigint,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX conversations_user_idx ON conversations (user_id, created_at DESC);

CREATE TABLE messages (
    id              bigserial PRIMARY KEY,
    conversation_id bigint      NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    role            text        NOT NULL CHECK (role IN ('user', 'assistant')),
    content         text        NOT NULL,
    sources         jsonb       NOT NULL DEFAULT '[]',
    feedback        smallint    CHECK (feedback IN (-1, 1)),
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX messages_conversation_idx ON messages (conversation_id, id);

-- 版本记录（同步自 Gitea Release）
CREATE TABLE releases (
    id               bigserial PRIMARY KEY,
    repo_id          bigint      NOT NULL REFERENCES repos (id),
    tag              text        NOT NULL,
    name             text        NOT NULL DEFAULT '',
    body             text        NOT NULL DEFAULT '',
    prerelease       boolean     NOT NULL DEFAULT false,
    published_at     timestamptz,
    gitea_release_id bigint,
    commit_sha       text        NOT NULL DEFAULT '', -- tag 所指提交，用于按提交识别上线
    html_url         text        NOT NULL DEFAULT '',
    synced_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (repo_id, tag)
);
CREATE UNIQUE INDEX releases_gitea_release_id_key ON releases (gitea_release_id) WHERE gitea_release_id IS NOT NULL;

-- 工单
CREATE TABLE tickets (
    id                    bigserial PRIMARY KEY,
    user_id               text        NOT NULL,
    user_name             text        NOT NULL DEFAULT '', -- 提交人快照，仅展示与通知，不用于权限判断
    user_email            text        NOT NULL DEFAULT '',
    conversation_id       bigint      REFERENCES conversations (id) ON DELETE SET NULL,
    project_id            bigint      NOT NULL REFERENCES projects (id),
    repo_id               bigint      REFERENCES repos (id), -- 转出前可为空
    type                  text        NOT NULL CHECK (type IN ('consult', 'bug', 'feature')),
    title                 text        NOT NULL,
    body                  text        NOT NULL DEFAULT '',
    status                text        NOT NULL CHECK (status IN (
                              'pending_triage', 'answered', 'merged', 'rejected',
                              'submitted', 'open', 'in_progress', 'need_info',
                              'fixed', 'released', 'deployed', 'closed', 'abnormal')),
    merged_into_ticket_id bigint      REFERENCES tickets (id) ON DELETE SET NULL,
    context               jsonb       NOT NULL DEFAULT '{}', -- 页面 URL、浏览器、版本等
    fields                jsonb       NOT NULL DEFAULT '{}', -- Issue 表单模板字段取值（字段 ID → 值列表）
    gitea_issue_number    bigint,
    gitea_updated_at      timestamptz, -- Webhook 乱序判断
    reported_version      text        NOT NULL DEFAULT '',
    milestone             text        NOT NULL DEFAULT '',
    due_date              date,
    fixed_pr              bigint,
    fixed_commit          text        NOT NULL DEFAULT '',
    fixed_repo            text        NOT NULL DEFAULT '', -- 修复 PR 所在仓库（小写 owner/name），可能与工单仓库不同
    fixed_release_id      bigint      REFERENCES releases (id) ON DELETE SET NULL,
    deployed_at           timestamptz,
    triage_since          timestamptz, -- 进入待确认的时间
    triage_reminded_at    timestamptz, -- 最近一次待确认提醒
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CHECK (gitea_issue_number IS NULL OR repo_id IS NOT NULL)
);
CREATE UNIQUE INDEX tickets_issue_key ON tickets (repo_id, gitea_issue_number) WHERE gitea_issue_number IS NOT NULL;
CREATE INDEX tickets_user_idx ON tickets (user_id, created_at DESC);
CREATE INDEX tickets_project_status_idx ON tickets (project_id, status);
CREATE INDEX tickets_triage_idx ON tickets (triage_since) WHERE status = 'pending_triage';

ALTER TABLE conversations
    ADD CONSTRAINT conversations_ticket_id_fkey FOREIGN KEY (ticket_id) REFERENCES tickets (id) ON DELETE SET NULL;

-- 工单拆到多个仓库时的附加 Issue（主 Issue 在 tickets 上）；全部修复发布 / 上线后工单才算 released / deployed
CREATE TABLE ticket_issues (
    id                 bigserial PRIMARY KEY,
    ticket_id          bigint      NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    repo_id            bigint      NOT NULL REFERENCES repos (id),
    gitea_issue_number bigint      NOT NULL,
    state              text        NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'closed')),
    fixed_pr           bigint,
    fixed_commit       text        NOT NULL DEFAULT '',
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (repo_id, gitea_issue_number)
);
CREATE INDEX ticket_issues_ticket_idx ON ticket_issues (ticket_id);

-- 工单评论：用户补充、后台答复、研发对外回复（来自 Gitea）
CREATE TABLE ticket_comments (
    id               bigserial PRIMARY KEY,
    ticket_id        bigint      NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    author_id        text        NOT NULL DEFAULT '', -- 接入方用户 ID；来自 Gitea 时为空
    author_name      text        NOT NULL DEFAULT '', -- Gitea 评论为研发账号 login
    source           text        NOT NULL CHECK (source IN ('user', 'staff', 'agent', 'gitea')),
    body             text        NOT NULL,
    public           boolean     NOT NULL DEFAULT true, -- 对提交人可见
    gitea_comment_id bigint      UNIQUE,                -- 已同步到 / 来自 Gitea 的评论
    gitea_posted_id  bigint      UNIQUE, -- 灵问补充由机器人发到 Gitea 后的评论 ID（与 gitea_comment_id 分开，研发编辑机器人评论不会改写原文）
    forwarded_at     timestamptz, -- 已随 Issue 正文转出（建单前的补充），不再单独发评论
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    deleted_at       timestamptz
);
CREATE INDEX ticket_comments_ticket_idx ON ticket_comments (ticket_id, id);

-- 附件：存数据库，不依赖对象存储；访问按 user_id / 工单归属校验
CREATE TABLE attachments (
    id                    bigserial PRIMARY KEY,
    user_id               text        NOT NULL,
    ticket_id             bigint      REFERENCES tickets (id) ON DELETE CASCADE,
    comment_id            bigint      REFERENCES ticket_comments (id) ON DELETE CASCADE,
    filename              text        NOT NULL,
    content_type          text        NOT NULL,
    size                  bigint      NOT NULL CHECK (size >= 0),
    sha256                bytea       NOT NULL,
    data                  bytea       NOT NULL,
    forwarded             boolean     NOT NULL DEFAULT false, -- 已上传到 Gitea
    gitea_attachment_uuid text        NOT NULL DEFAULT '',
    gitea_url             text        NOT NULL DEFAULT '', -- Gitea 中的下载地址，用于在 Issue / 评论正文引用
    forward_error         text        NOT NULL DEFAULT '', -- 上传被 Gitea 拒绝（类型、大小限制）的原因，不再重试
    created_at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX attachments_ticket_idx ON attachments (ticket_id);
-- 未关联工单的附件 24 小时后清理
CREATE INDEX attachments_orphan_idx ON attachments (created_at) WHERE ticket_id IS NULL;

-- 后台确认记录
CREATE TABLE triage_actions (
    id               bigserial PRIMARY KEY,
    ticket_id        bigint      NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    actor_id         text        NOT NULL,
    action           text        NOT NULL CHECK (action IN ('forward', 'answer', 'merge', 'reject')),
    note             text        NOT NULL DEFAULT '',
    target_ticket_id bigint      REFERENCES tickets (id) ON DELETE SET NULL,
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX triage_actions_ticket_idx ON triage_actions (ticket_id);

-- 需求评审
CREATE TABLE reviews (
    id          bigserial PRIMARY KEY,
    ticket_id   bigint      NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    reviewer_id text        NOT NULL,
    verdict     text        NOT NULL CHECK (verdict IN ('approve', 'reject', 'need_info')),
    comment     text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX reviews_ticket_idx ON reviews (ticket_id);

-- 版本关联工单：以 tag 区间提交匹配 fixed_commit / fixed_pr 为主，版本说明中的 Issue 引用补漏
CREATE TABLE release_tickets (
    release_id bigint      NOT NULL REFERENCES releases (id) ON DELETE CASCADE,
    ticket_id  bigint      NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    ref        text        NOT NULL DEFAULT '', -- 'commit' 或版本说明中的引用原文
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (release_id, ticket_id)
);
CREATE INDEX release_tickets_ticket_idx ON release_tickets (ticket_id);

-- 上线记录
CREATE TABLE deployments (
    id          bigserial PRIMARY KEY,
    release_id  bigint      NOT NULL REFERENCES releases (id) ON DELETE CASCADE,
    env         text        NOT NULL DEFAULT 'production',
    source      text        NOT NULL CHECK (source IN ('workflow', 'status', 'manual')),
    ref         text        NOT NULL DEFAULT '',
    deployed_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (release_id, env)
);

-- 业务数据请求审计：不保存业务数据原文
CREATE TABLE data_requests (
    id              bigserial PRIMARY KEY,
    user_id         text        NOT NULL,
    conversation_id bigint      REFERENCES conversations (id) ON DELETE SET NULL,
    tool_name       text        NOT NULL,
    params_digest   text        NOT NULL DEFAULT '',
    status          text        NOT NULL CHECK (status IN ('ok', 'denied', 'failed')),
    latency_ms      integer     NOT NULL DEFAULT 0,
    idempotency_key text,
    created_at      timestamptz NOT NULL DEFAULT now()
);
-- write 类工具防重复提交
CREATE UNIQUE INDEX data_requests_idempotency_key ON data_requests (user_id, tool_name, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX data_requests_user_idx ON data_requests (user_id, created_at DESC);

-- 工单事件（审计，含 Gitea 回流的全部变化）
CREATE TABLE ticket_events (
    id         bigserial PRIMARY KEY,
    ticket_id  bigint      NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    actor_id   text        NOT NULL DEFAULT '',
    source     text        NOT NULL CHECK (source IN ('api', 'mcp', 'go', 'gitea', 'system')),
    kind       text        NOT NULL,
    payload    jsonb       NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ticket_events_ticket_idx ON ticket_events (ticket_id, id);

-- 对外事件：Notifier 推送与 ListEvents 变更流共用，id 即 lingo.Event.ID
CREATE TABLE events (
    id          bigserial PRIMARY KEY,
    kind        text        NOT NULL,
    project_id  bigint      REFERENCES projects (id),
    ticket_id   bigint      REFERENCES tickets (id) ON DELETE SET NULL,
    user_id     text        NOT NULL DEFAULT '',
    actor_id    text        NOT NULL DEFAULT '',
    status      text        NOT NULL DEFAULT '',
    prev_status text        NOT NULL DEFAULT '',
    action      text        NOT NULL DEFAULT '',
    payload     jsonb       NOT NULL DEFAULT '{}',
    -- 写入事务的 ID：变更流只返回早于所有进行中事务的事件，ID 先分配后提交的事件不会被游标跳过
    txid        xid8        NOT NULL DEFAULT pg_current_xact_id(),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX events_ticket_idx ON events (ticket_id, id);
CREATE INDEX events_user_idx ON events (user_id, id);
CREATE INDEX events_project_idx ON events (project_id, id);
-- 评论被删除、改为内部或编辑时按评论 ID 更新对应事件
CREATE INDEX events_comment_idx ON events ((payload->>'comment_id')) WHERE kind = 'ticket.comment';

-- 异步任务：Gitea 写操作与 Notifier 投递；worker 用 FOR UPDATE SKIP LOCKED + 租约领取
CREATE TABLE outbox (
    id              bigserial PRIMARY KEY,
    kind            text        NOT NULL CHECK (kind IN ('gitea', 'notify')),
    key             text        UNIQUE, -- 幂等键，如 'create_issue:ticket:42'，同一键只入队一次
    event_id        bigint      REFERENCES events (id) ON DELETE CASCADE,
    ticket_id       bigint      REFERENCES tickets (id) ON DELETE CASCADE, -- gitea 任务所属工单，同一工单按序执行
    payload         jsonb       NOT NULL DEFAULT '{}',
    status          text        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'done', 'failed')),
    attempts        integer     NOT NULL DEFAULT 0, -- 领取次数，兼作租约令牌
    failures        integer     NOT NULL DEFAULT 0, -- 失败次数（推迟、释放不计），与重试上限比较
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    locked_until    timestamptz, -- 租约：处理中的任务在此之前不会被其他实例领取
    last_error      text        NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX outbox_due_idx ON outbox (next_attempt_at) WHERE status = 'pending';
-- 通知按工单顺序领取时检查同一工单是否有更早的未完成通知
CREATE INDEX outbox_notify_pending_idx ON outbox (event_id) WHERE kind = 'notify' AND status = 'pending';
CREATE INDEX outbox_gitea_pending_idx ON outbox (ticket_id, id) WHERE kind = 'gitea' AND status = 'pending';

-- Webhook 投递：验签后落库，worker 处理；去重见 internal/webhook
CREATE TABLE webhook_deliveries (
    id              bigserial PRIMARY KEY,
    delivery_id     text        NOT NULL UNIQUE,
    event           text        NOT NULL DEFAULT '',
    event_type      text        NOT NULL,
    kind            text        NOT NULL,
    dedup_key       text        NOT NULL UNIQUE,
    repo            text        NOT NULL DEFAULT '',
    issue_number    bigint,
    from_bot        boolean     NOT NULL DEFAULT false,
    payload         bytea       NOT NULL, -- 原始载荷字节；不用 jsonb，因其拒收 NUL 字符转义
    parse_error     text        NOT NULL DEFAULT '', -- kind = 'invalid' 时的解析错误
    received_at     timestamptz NOT NULL DEFAULT now(),
    processed_at    timestamptz,
    attempts        integer     NOT NULL DEFAULT 0, -- 领取次数，兼作租约令牌
    failures        integer     NOT NULL DEFAULT 0, -- 失败次数（推迟、释放不计），与重试上限比较
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    locked_until    timestamptz,
    last_error      text        NOT NULL DEFAULT ''
);
CREATE INDEX webhook_deliveries_pending_idx ON webhook_deliveries (next_attempt_at, id) WHERE processed_at IS NULL;
-- 同一 Issue 的投递按顺序处理：领取时检查是否有更早的未完成投递
CREATE INDEX webhook_deliveries_issue_pending_idx ON webhook_deliveries (repo, issue_number, id) WHERE processed_at IS NULL;
-- Issue 关闭时检查是否还有未处理的 PR 合并投递
CREATE INDEX webhook_deliveries_merge_pending_idx ON webhook_deliveries (received_at) WHERE processed_at IS NULL AND kind = 'pull.merged';

-- 连通记录：push / 测试投递只更新这里，不进投递表
CREATE TABLE webhook_pings (
    repo         text PRIMARY KEY, -- "owner/name"，小写
    last_ping_at timestamptz NOT NULL
);

-- 对账游标
CREATE TABLE sync_cursors (
    repo_id    bigint      NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    kind       text        NOT NULL CHECK (kind IN ('issues', 'comments', 'releases', 'deployments')),
    since      timestamptz NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repo_id, kind)
);

-- 知识库问答；embedding 先用 real[]，接入 pgvector 后另行迁移
CREATE TABLE kb_entries (
    id                     bigserial PRIMARY KEY,
    project_id             bigint      NOT NULL REFERENCES projects (id),
    repo_id                bigint      REFERENCES repos (id),
    question               text        NOT NULL,
    answer                 text        NOT NULL,
    source_ticket_id       bigint      REFERENCES tickets (id) ON DELETE SET NULL,
    source_conversation_id bigint      REFERENCES conversations (id) ON DELETE SET NULL,
    status                 text        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected')),
    tags                   text[]      NOT NULL DEFAULT '{}',
    embedding              real[],
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX kb_entries_project_status_idx ON kb_entries (project_id, status);
