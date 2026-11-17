# Release 配置

> 本文件由 `/rd:release` 在步骤 0 读取，注入项目特有的发版规则。
> 删除不需要的章节即可——所有章节均为可选，缺失时使用插件默认行为。

## 版本号

- lingo 以 Go module semver tag（`vX.Y.Z`）发布，无版本号文件需要更新
- 根包公开 API 有破坏性变更时升主版本；v2 起 module 路径加 `/v2` 后缀

## 发版前检查

> 检查失败时硬停止，不继续执行后续步骤。

- 静态检查：`go vet ./...`
- 运行测试：`go test ./...`
- 构建：`go build ./...`（含 `examples/`）

## 发版后步骤

> 仅输出提示，不自动执行副作用操作。

- 版本说明中注明本版本新增的迁移文件，提醒接入方在发布流程中调用 `lingo.Migrate`
- 提醒接入方 `go get github.com/zhouhao4221/lingo@<version>` 升级
