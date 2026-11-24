-- 知识问答 Agent：工单与版本问答（REQ-003）
-- 会话只有创建者本人可访问（含 admin）；不带项目，项目范围每次按 User.Projects 现算。软删除：统计取自消息行，删除会话后仍保留。

-- activity_seq：会话每有新消息取一次 nextval，用作"最近活动"倒序与分页游标（单整数游标，翻页不重不漏）
CREATE SEQUENCE lingo_assistant_activity_seq;

CREATE TABLE lingo_assistant_conversations (
    id             bigserial PRIMARY KEY,
    user_id        text        NOT NULL,              -- 创建者（接入方用户 ID）
    title          text        NOT NULL DEFAULT '',   -- 为空时由首条提问生成
    activity_seq   bigint      NOT NULL DEFAULT nextval('lingo_assistant_activity_seq'),
    last_active_at timestamptz NOT NULL DEFAULT now(),
    turn_until     timestamptz,                       -- 回答生成租约（数据库时间）：同一会话同时只处理一条提问，崩溃后自然过期
    created_at     timestamptz NOT NULL DEFAULT now(),
    deleted_at     timestamptz
);
CREATE INDEX lingo_assistant_conversations_user_idx
    ON lingo_assistant_conversations (user_id, activity_seq DESC) WHERE deleted_at IS NULL;

CREATE TABLE lingo_assistant_messages (
    id              bigserial PRIMARY KEY,
    conversation_id bigint      NOT NULL REFERENCES lingo_assistant_conversations (id) ON DELETE CASCADE,
    role            text        NOT NULL CHECK (role IN ('user', 'assistant')),
    content         text        NOT NULL DEFAULT '',
    status          text        NOT NULL DEFAULT 'done' CHECK (status IN ('done', 'failed')),
    error           text        NOT NULL DEFAULT '',  -- 失败类别：model_error | timeout | canceled | internal_error（不存模型原文）
    reply_to        bigint      REFERENCES lingo_assistant_messages (id) ON DELETE CASCADE, -- 助手消息所回答的用户消息
    intent          text        NOT NULL DEFAULT '',  -- 助手消息：解析出的意图（统计用）
    outcome         text        NOT NULL DEFAULT '',  -- 助手消息：结果分类（统计用），如 answered / not_found / action_executed
    citations       jsonb       NOT NULL DEFAULT '{}', -- 回答当时的引用快照 {"tickets":[…],"releases":[…]}
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX lingo_assistant_messages_conversation_idx ON lingo_assistant_messages (conversation_id, id);

-- 待确认操作：通用结构（kind + payload），本期只有 ticket_comment，后续 Tool 写操作复用
CREATE TABLE lingo_assistant_actions (
    id              bigserial PRIMARY KEY,
    conversation_id bigint      NOT NULL REFERENCES lingo_assistant_conversations (id) ON DELETE CASCADE,
    message_id      bigint      NOT NULL REFERENCES lingo_assistant_messages (id) ON DELETE CASCADE, -- 提出该操作的助手消息
    user_id         text        NOT NULL,
    kind            text        NOT NULL,
    payload         jsonb       NOT NULL DEFAULT '{}',
    summary         text        NOT NULL DEFAULT '',   -- 给用户看的操作描述
    status          text        NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'executed', 'cancelled', 'expired', 'failed')),
    result          jsonb       NOT NULL DEFAULT '{}',
    expires_at      timestamptz NOT NULL,
    resolved_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now()
);
-- 每个会话同时最多一个待确认操作
CREATE UNIQUE INDEX lingo_assistant_actions_pending_key ON lingo_assistant_actions (conversation_id) WHERE status = 'pending';
CREATE INDEX lingo_assistant_actions_message_idx ON lingo_assistant_actions (message_id);
