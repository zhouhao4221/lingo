# Release 配置

> 本文件由 `/rd:release` 在步骤 0 读取，注入项目特有的发版规则。
> 删除不需要的章节即可——所有章节均为可选，缺失时使用插件默认行为。

## 版本号文件

> 版本号确定后（步骤 2），自动更新并暂存以下文件中的版本号字段。

- `web/package.json` → `version`（写入本次 release 版本号，去掉 v 前缀；当前无该字段时新增）

## 发版前检查

> 检查失败时硬停止，不继续执行后续步骤。

- 静态检查：`go vet ./...`
- 运行测试：`go test ./...`
- 后端构建：`go build -o bin/server ./cmd/server`
- 前端构建：`cd web && pnpm install --frozen-lockfile && pnpm build`

## 发版后步骤

> 仅输出提示，不自动执行副作用操作。

- 提醒执行本版本合并后的迁移 SQL（`migrations/`）
- 部署：按 `deploy/` 下的部署脚本发布（待补充）

## 额外附件

- `bin/server`
