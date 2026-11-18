// Package tests 是 lingo 的集成测试，依赖真实 PostgreSQL。
//
// 独立 Go module：PostgreSQL 驱动（pgx）只作为测试依赖出现在这里，不进入 lingo 主 module 的依赖。
// 运行：
//
//	cd tests && LINGO_TEST_DATABASE_URL=postgres://user:pass@127.0.0.1:5432/db?sslmode=disable go test ./...
//
// 每个测试在独立 schema 中运行，结束后删除；未设置 LINGO_TEST_DATABASE_URL 时跳过。
package tests
