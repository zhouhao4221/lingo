-- 转研发后驳回（REQ-002，待办 #698）：补充评论外发与驳回的仲裁
-- sending_until：worker 发送补充评论（含其附件）前，在锁定工单的短事务中写入的发送租约（数据库时间）；驳回在同一把工单锁下
--   检查它，二者串行：租约有效期内驳回不提交（等待或冲突），驳回先提交时 worker 不再发送
-- withheld_at：驳回后排队中、未发到 Gitea 的补充评论；与“转出前的补充”（forwarded_at）区分，审计 comment.withheld 只记一次
ALTER TABLE lingo_ticket_comments
    ADD COLUMN sending_until timestamptz,
    ADD COLUMN withheld_at   timestamptz;
