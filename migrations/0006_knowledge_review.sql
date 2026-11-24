-- 知识库二期（REQ-007）：工单抽取与回答反馈入库（待审核）、后台评论入库、审核、可能重复、未答上统计

ALTER TABLE lingo_knowledge DROP CONSTRAINT lingo_knowledge_status_check;
ALTER TABLE lingo_knowledge ADD CONSTRAINT lingo_knowledge_status_check
    CHECK (status IN ('active', 'disabled', 'pending', 'rejected'));
ALTER TABLE lingo_knowledge DROP CONSTRAINT lingo_knowledge_source_check;
ALTER TABLE lingo_knowledge ADD CONSTRAINT lingo_knowledge_source_check
    CHECK (source IN ('manual', 'ticket_answer', 'ticket_extract', 'assistant_feedback', 'ticket_comment'));
ALTER TABLE lingo_knowledge
    ADD COLUMN source_message_id bigint REFERENCES lingo_assistant_messages (id) ON DELETE SET NULL, -- 来源 d：被标为"有帮助"的回答
    ADD COLUMN duplicate_of      bigint REFERENCES lingo_knowledge (id) ON DELETE SET NULL,          -- 入库时检出的可能重复条目
    ADD COLUMN helpful_count     bigint NOT NULL DEFAULT 0,  -- 引用该条目的回答被标为"有帮助"的次数
    ADD COLUMN unhelpful_count   bigint NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX lingo_knowledge_source_message_idx ON lingo_knowledge (source_message_id) WHERE source_message_id IS NOT NULL;
-- 每张工单最多一条抽取候选（多实例下的兜底）
CREATE UNIQUE INDEX lingo_knowledge_extract_ticket_idx ON lingo_knowledge (source_ticket_id) WHERE source = 'ticket_extract';
CREATE INDEX lingo_knowledge_pending_idx ON lingo_knowledge (project_id, id) WHERE status = 'pending';

-- 问答回答的反馈：每条助手消息一行，以最后一次为准
CREATE TABLE lingo_assistant_feedback (
    message_id bigint      PRIMARY KEY REFERENCES lingo_assistant_messages (id) ON DELETE CASCADE,
    user_id    text        NOT NULL,
    helpful    boolean     NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- 结案工单的知识抽取：每张工单最多一行（每张工单最多抽取一次）
CREATE TABLE lingo_knowledge_extractions (
    ticket_id    bigint      PRIMARY KEY REFERENCES lingo_tickets (id) ON DELETE CASCADE,
    status       text        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'done', 'failed', 'skipped')),
    attempts     integer     NOT NULL DEFAULT 0,
    locked_until timestamptz,                     -- 租约（多实例互斥），崩溃后自然过期
    last_error   text        NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX lingo_knowledge_extractions_pending_idx ON lingo_knowledge_extractions (ticket_id) WHERE status = 'pending';
-- 升级时已结案的工单不抽取（避免一次性大量调用模型）
INSERT INTO lingo_knowledge_extractions (ticket_id, status)
SELECT id, 'skipped' FROM lingo_tickets WHERE type IN ('bug', 'feature') AND status IN ('released', 'deployed', 'closed');

-- 知识类问题没有命中的提问（脱敏），按项目分别记录、按切词重合度归并计数，保留 90 天
CREATE TABLE lingo_knowledge_misses (
    id          bigserial PRIMARY KEY,
    project_key text        NOT NULL,             -- 提问人可访问的项目（每个项目一行，只对该项目的 reviewer 可见）
    question    text        NOT NULL,
    tokens     text[]      NOT NULL DEFAULT '{}',
    hits       integer     NOT NULL DEFAULT 1,
    first_at   timestamptz NOT NULL DEFAULT now(),
    last_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX lingo_knowledge_misses_tokens_idx ON lingo_knowledge_misses USING gin (tokens);
CREATE INDEX lingo_knowledge_misses_project_idx ON lingo_knowledge_misses (project_key, last_at);
