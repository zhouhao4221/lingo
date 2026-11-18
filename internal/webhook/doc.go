// Package webhook 接收 Gitea Webhook：验签、把各类载荷归一为 Event、计算去重键并落库，随即返回 2xx。
//
// 业务处理（更新工单状态、写 ticket_events、发通知）不在请求内进行，由 worker 读取已落库的投递，
// 用 Parse 还原 Event 后处理；这样 Gitea 的 5 秒投递超时不受业务耗时影响。
//
// 去重两层（PRD 3.2.2）：X-Gitea-Delivery 挡住同一次投递的重复请求；DedupKey 是业务键，
// 手动重投（新 delivery ID）与对账拉回的同一变化得到相同的键。
package webhook
