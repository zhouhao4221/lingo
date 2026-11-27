-- 模型内容策略（REQ-012，待办 #793）：模型准入与用户可见（public）分开记录，供 Config.ModelContent = ModelContentMarked 时
-- 统一过滤送给模型的内容。只加可空列、替换一个部分唯一索引；不改写任何已有行，旧版本照常读写（新列为 NULL = 未知）。
--
-- 评论与助手消息的判定与写入时的版本绑定（*_at = updated_at 且 *_md5 = md5(正文)），读取时不一致即按未知处理：旧版本实例
-- （滚动升级期间或回退后）改写同一行的正文时不会更新这些列，旧判定随之失效，不会被当作可信。

-- Gitea 评论按原始正文的标记（/reply 开头或带 <!-- lingo:public -->）判定是否属于 ReplyMarked 内容范围，与 ReplyMode 无关；
-- 同步（Webhook、定时对账、历史评论重新同步）时随正文写入，乱序旧版本与已删除的不覆盖。NULL：升级前同步、尚未重新判定（不准入）。
-- 只对 source = 'gitea' 有意义：灵工本地的评论（提交人补充、后台与 MCP 回复）按其显式的 public 判定。
ALTER TABLE lingo_ticket_comments
    ADD COLUMN model_marked     boolean,
    ADD COLUMN model_marked_at  timestamptz, -- 判定时的 updated_at
    ADD COLUMN model_marked_md5 text;        -- 判定时的 md5(body)

-- 助手消息生成时送给模型的内容范围：marked（都在 ReplyMarked 范围内且来源可信）| authorized（另含提问人角色已授权的
-- 内部评论，无其他不可信内容）| untrusted（含来源未知或不可信的知识、历史，或 external 范围外的评论）；NULL：升级前的消息、
-- 失败的回答、用户消息（未知，不作为模型输入）。
ALTER TABLE lingo_assistant_messages
    ADD COLUMN model_scope     text CHECK (model_scope IN ('marked', 'authorized', 'untrusted')),
    ADD COLUMN model_scope_at  timestamptz, -- 判定时的 updated_at
    ADD COLUMN model_scope_md5 text;        -- 判定时的 md5(content)

-- 派生知识条目（ticket_extract、assistant_feedback）的来源是否可信：true 为按新规则从可信内容生成；false 为来源含范围外内容；
-- NULL 为升级前生成（未知，不作为模型输入）。manual、ticket_answer、ticket_comment 由人工在灵工中填写，不看此列。
-- 不与版本绑定：它描述的是条目的来源；旧版本对已有条目的改写只有人工编辑与审核（新版本同样保留来源判定），不会写入派生内容。
ALTER TABLE lingo_knowledge ADD COLUMN model_ok boolean;

-- 每张工单原先最多一条抽取候选；按新规则重新抽取（ReextractTicketKnowledge）时保留旧候选、另入库一条可信候选：
-- 来源未知 / 不可信的与可信的各最多一条。旧版本写入的 model_ok 为 NULL，仍受第一个索引约束。
-- 注意：非 CONCURRENTLY 重建，执行期间短暂锁 lingo_knowledge 的写入（表通常很小）。
DROP INDEX lingo_knowledge_extract_ticket_idx;
CREATE UNIQUE INDEX lingo_knowledge_extract_ticket_idx ON lingo_knowledge (source_ticket_id)
    WHERE source = 'ticket_extract' AND model_ok IS NOT TRUE;
CREATE UNIQUE INDEX lingo_knowledge_extract_trusted_idx ON lingo_knowledge (source_ticket_id)
    WHERE source = 'ticket_extract' AND model_ok;
