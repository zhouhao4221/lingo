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
- 后端构建：`go build -o bin/server ./cmd/server`（前端产物经 `go:embed` 打入）
- 前端构建：`cd web && pnpm install --frozen-lockfile && pnpm build`

## 发版后步骤

> 仅输出提示，不自动执行副作用操作。

- 提醒执行本版本合并后的迁移 SQL：`server migrate`（不依赖应用启动时自动迁移）
- 部署：
  - 单机：二进制 + systemd，或在管理后台用 `binary` 执行器在线升级
  - 规模化：推送容器镜像，由流水线 / 编排系统滚动发布（`pipeline` 执行器可从后台触发）
  - 部署脚本见 `deploy/`（待补充）

## 额外附件

> 在线升级依赖以下附件，名称保持稳定。

- `bin/server`（linux/amd64 二进制）
- `bin/server.sha256`（校验和，`binary` 执行器必验）
- 容器镜像：`<registry>/lingo:<version>`（规模化部署使用，镜像仓库地址待定）
