# AGENTS.md

本文件约定本仓库的协作与交付方式。

## 交付方式

- 本项目不走 Merge Request / Pull Request。改动直接推送到 `main`，不创建分支后再开 PR 或 MR，也不使用 `gh pr merge` 等合并流程。
- 推送前在本地运行与 CI 一致的检查：`go vet ./...` 和 `go test -race ./...`。两者都通过后再推送。
- 推送后跟进 CI（`.github/workflows/verify.yml`）。如果失败，先定位原因，属于本次改动可修复的问题则直接修复并再次推送；无法修复时向用户报告失败的检查项和关键日志。
- 不得为了通过检查而绕过 hook、跳过测试或使用 `--no-verify`。

## 提交

- 提交信息使用中文 Conventional Commits：`type(scope): 中文主题`，主题简洁，正文说明动机和设计。
- 按业务边界分组提交，不把无关改动混在一个提交里。
- 不提交 `.env`、节点凭据、订阅地址或其他私密配置。`.env.template` 可以提交。

## 本地开发

- Go 版本见 `go.mod`；常用任务见 `mise.toml`。
- 前端资源位于 `internal/manager/web/`，相关交互测试在 `internal/manager/testdata/log_view_test.cjs`，由 Go 测试执行。
- 涉及远端部署的测试和脚本，改动后应在 `tests/deployment` 中验证，不要直接在生产主机上试验。
