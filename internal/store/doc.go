// Package store 持久化：基于接入方提供的 *sql.DB（PostgreSQL）实现各领域定义的仓储接口。
//
// 表结构见 migrations/，由 lingo.Migrate 执行。store 不引入数据库驱动，SQL 只用标准 PostgreSQL 语法与
// database/sql 的 $n 占位符，兼容 pgx stdlib、lib/pq 等驱动。
//
// 后台任务的领取统一用 FOR UPDATE SKIP LOCKED + 租约（locked_until）：领取后在租约内处理，
// 进程崩溃时租约到期自动回到可领取状态，多实例运行安全。
package store
