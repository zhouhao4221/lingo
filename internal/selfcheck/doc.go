// Package selfcheck 启动自检与初始化：检查 Gitea 版本与附件设置、各对接仓库可访问与 Issue 启用、机器人写权限、
// 补齐灵问标签、Webhook 存在与事件订阅完整（AutoRegisterWebhook 时自动创建或修正）并确认测试投递到达、
// 配置的 Issue 模板与部署工作流存在。结果写日志、不阻断启动（PRD 3.2.2、gitea-integration.md 12.3）。
package selfcheck
