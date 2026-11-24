-- 业务数据 Tool 调用审计（REQ-008）：每次调用一行，先以 pending 写入再调用；不含结果与错误原文，保留 180 天

CREATE TABLE lingo_data_requests (
    id              bigserial   PRIMARY KEY,
    user_id         text        NOT NULL,
    user_role       text        NOT NULL,
    project_key     text,                         -- 归属项目：只取工具结果的声明（ToolOutput.Project），未声明为空
    tool            text        NOT NULL,
    channel         text        NOT NULL CHECK (channel IN ('assistant', 'mcp', 'api')),
    conversation_id bigint,                       -- 问答入口的会话；不设外键，会话删除后审计保留
    arg_names       text[]      NOT NULL DEFAULT '{}',
    args            jsonb,                        -- 参数名 → 截断、脱敏后的值；拒绝记录为空
    status          text        NOT NULL
                    CHECK (status IN ('pending', 'ok', 'denied', 'error', 'unavailable', 'interrupted')),
    reason          text        NOT NULL DEFAULT '',
    duration_ms     integer     NOT NULL DEFAULT 0,
    truncated       boolean     NOT NULL DEFAULT false,
    lease_until     timestamptz,                  -- pending 记录的租约（数据库时间），过期后由 Run 标为中断
    created_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz
);
CREATE INDEX lingo_data_requests_user_idx ON lingo_data_requests (user_id, id DESC);
CREATE INDEX lingo_data_requests_project_idx ON lingo_data_requests (project_key, id DESC) WHERE project_key IS NOT NULL;
CREATE INDEX lingo_data_requests_created_idx ON lingo_data_requests (created_at);
CREATE INDEX lingo_data_requests_pending_idx ON lingo_data_requests (lease_until) WHERE status = 'pending';
