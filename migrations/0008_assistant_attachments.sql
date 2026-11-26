-- 知识问答带图提问（REQ-009）：用户消息附带的图片引用 [{"id","name","content_type","size"}]。
-- 图片由接入方的附件系统保存，这里只存引用（不含地址与内容）；助手消息与旧消息为空数组。只加列，旧版本照常读写。
ALTER TABLE lingo_assistant_messages ADD COLUMN attachments jsonb NOT NULL DEFAULT '[]';
