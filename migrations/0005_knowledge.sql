-- 知识库一期（REQ-006）：后台答复时勾选入库或手动新建的问答条目，供知识问答检索作答
-- 检索不依赖数据库扩展：search_tokens 是 Go 端切好的词（中文按二元切词），用 GIN 数组索引按重合度排序；
-- 向量（可选）存 real[]，由灵工对候选条目计算相似度。

CREATE TABLE lingo_knowledge (
    id                 bigserial PRIMARY KEY,
    project_id         bigint      NOT NULL REFERENCES lingo_projects (id) ON DELETE CASCADE,
    repo_id            bigint      REFERENCES lingo_repos (id) ON DELETE SET NULL,
    question           text        NOT NULL,
    answer             text        NOT NULL,
    source             text        NOT NULL CHECK (source IN ('manual', 'ticket_answer')),
    source_ticket_id   bigint      REFERENCES lingo_tickets (id) ON DELETE SET NULL,
    status             text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    cite_count         bigint      NOT NULL DEFAULT 0,  -- 被问答引用的次数
    search_tokens      text[]      NOT NULL DEFAULT '{}',
    embedding          real[],                           -- 为空表示待生成（新建、编辑后）
    embed_model        text,                             -- 生成向量的模型标识；与当前模型不一致的向量不参与重排
    embed_locked_until timestamptz,                      -- 生成向量的租约（多实例互斥），崩溃后自然过期
    created_by         text        NOT NULL DEFAULT '',
    updated_by         text        NOT NULL DEFAULT '',
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX lingo_knowledge_tokens_idx ON lingo_knowledge USING gin (search_tokens) WHERE status = 'active';
CREATE INDEX lingo_knowledge_project_idx ON lingo_knowledge (project_id, status, updated_at DESC);
CREATE INDEX lingo_knowledge_source_idx ON lingo_knowledge (source_ticket_id) WHERE source_ticket_id IS NOT NULL;
CREATE INDEX lingo_knowledge_embed_idx ON lingo_knowledge (id) WHERE status = 'active' AND embedding IS NULL;
