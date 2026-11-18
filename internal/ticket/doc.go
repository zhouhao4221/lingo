// Package ticket 工单领域：咨询 / Bug / 需求三类工单的状态机，以及与 Gitea Issue 的双向关联。
//
// Sync 把 Gitea 侧的变化（Issue、评论、PR）同步到工单：在一个事务内锁定工单、推进状态、写 ticket_events
// 审计并写对外事件（notify.Event）。Webhook 只是触发器，状态判断以调用方传入的 Issue 当前快照为准；
// 旧于已采纳 updated_at 的乱序事件只留痕不改状态。仓储接口 Store / Tx 由 internal/store 实现。
package ticket
