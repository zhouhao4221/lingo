-- 模型内容策略（REQ-012，待办 #793）：模型准入与用户可见（public）分开记录，供 Config.ModelContent = ModelContentMarked 时
-- 统一过滤送给模型的内容。只加可空列、替换一个部分唯一索引；不改写任何已有行，旧版本照常读写（新列为 NULL = 未知）。

-- Gitea 评论按原始正文的标记（/reply 开头或带 <!-- lingo:public -->）判定是否属于 ReplyMarked 内容范围，与 ReplyMode 无关；
-- 同步（Webhook、定时对账、历史评论重新同步）时随正文写入，乱序旧版本与已删除的不覆盖。NULL：升级前同步、尚未重新判定（不准入）。
-- 只对 source = 'gitea' 有意义：灵工本地的评论（提交人补充、后台与 MCP 回复）按其显式的 public 判定。
ALTER TABLE lingo_ticket_comments ADD COLUMN model_marked boolean;

-- 助手消息生成时送给模型的工单评论、知识条目与历史回答是否都在 ReplyMarked 内容范围内（可信来源链）。
-- true：都在范围内；false：含范围外的内容（如 reviewer 可见的内部评论）；NULL：升级前的消息（未知，不作为模型输入）。
ALTER TABLE lingo_assistant_messages ADD COLUMN model_ok boolean;

-- 派生知识条目（ticket_extract、assistant_feedback）的来源是否可信：true 为按新规则从可信内容生成；false 为来源含范围外内容；
-- NULL 为升级前生成（未知，不作为模型输入）。manual、ticket_answer、ticket_comment 由人工在灵工中填写，不看此列。
ALTER TABLE lingo_knowledge ADD COLUMN model_ok boolean;

-- 每张工单原先最多一条抽取候选；按新规则重新抽取（ReextractTicketKnowledge）时保留旧候选、另入库一条可信候选：
-- 来源未知 / 不可信的与可信的各最多一条。旧版本写入的 model_ok 为 NULL，仍受第一个索引约束。
DROP INDEX lingo_knowledge_extract_ticket_idx;
CREATE UNIQUE INDEX lingo_knowledge_extract_ticket_idx ON lingo_knowledge (source_ticket_id)
    WHERE source = 'ticket_extract' AND model_ok IS NOT TRUE;
CREATE UNIQUE INDEX lingo_knowledge_extract_trusted_idx ON lingo_knowledge (source_ticket_id)
    WHERE source = 'ticket_extract' AND model_ok;
