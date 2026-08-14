# Issue #953 独立复审（round 1）— unify Sift module and repository identity under xsift

- 日期：2026-08-14
- 基线：`origin/main` b594bb0 → `origin/refactor/issue-953-unify-sift-module-and-repository-identity-under` 03b66f3（2 commits：8fabe77 迁移 + 03b66f3 修复 launchd label）
- 载体：Issue #953 评论 + 本文件（第 1 轮）
- 模式：无历史关闭包 → 完整关闭包

## 验收核验（全部 YES）

| # | 验收项 | 证据 | 结果 |
|---|--------|------|------|
| A1 | `go list -m` | `go list -m` → `github.com/xsift/sift`；`tools/schema` → `github.com/xsift/sift/tools/schema` | YES |
| A2 | 无旧模块路径 | 全树 grep `miaoxiaoyong/sift` / `hexai-cn`：零命中于 `.go`/`go.mod`/`.sh`/`.yml`/`.rb`（仅剩下「允许保留」集合，见 A7） | YES |
| A3 | 链接全 xsift | README（install curl、bluff）、scripts/install.sh、cmd/sift/update.go（releaseAPI/DownloadBase）、internal/hosting（systemd Documentation + Homebrew formula 模板）、packaging/homebrew/sift.rb、docs/guides/*、docs/specs/hosting.md、docs/PRD.md | YES |
| A4 | 测试/静态/构建 | `go test -count=1 ./...` 28 pkg ok（1 处 pre-existing flake 见 DEFER-1）；`tools/schema` build+vet+test ok；`go vet ./...` 0；`CGO_ENABLED=0 go build ./...` 0 | YES |
| A5 | ldflags 注入 | `.goreleaser.yml` 双 build（sift / sift-agent-wrapper）均 `-X github.com/xsift/sift/internal/version.Release`；实建注入 0.5.4：`sift version`=0.5.4、wrapper `--version`=0.5.4 | YES |
| A6 | 端点 200 | `api.github.com/repos/xsift/sift/releases/latest`=200；raw install.sh=200；v0.5.4 资产（-L 跟随后）=200，release API 确认 tag v0.5.4 含 4 资产 | YES |
| A7 | scoped grep | 含旧 owner 共 7 文件：6×docs/reviews（历史允许重定向）+ `internal/hosting/hosting_test.go`（负向断言 + LegacyLabel 钉住） | YES |

## Preserve-intentionally 核验

- `com.miaoxiaoyong.sift` LegacyLabel 保留（hosting.go:58），`TestLaunchdIdentityKeepsCurrentAndLegacyLabels` 钉住当前+旧 label 且互异。
- 现行 launchd label `cn.hexai.sift`（v0.5.4 已装句柄）保留：8fabe77 曾误改 `com.xsift.sift`，03b66f3 已回退；docs/specs/hosting.md §3.2 已注明「仓库身份迁至 xsift 不改变运行时 service identity」。
- `docs/reviews/` 零改动（`git diff main...HEAD -- docs/reviews/ CHANGELOG.md` 为空）；CHANGELOG 无旧引用。
- `cmd/sift/setup_test.go` 用户示例（GitHub 账户名 miaoxiaoyong）未动（person-name 例外）。

## Findings

- `[P0]` 无。
- `[P1]` 无。

- `[P2-1]` 中间提交越界与回退：8fabe77 曾把现行 launchd label 改成 `com.xsift.sift`（会破坏已装服务迁移），由 fix-up 提交 03b66f3 回退。
  - 标尺：`git show 8fabe77:internal/hosting/hosting.go | grep -E 'Label ='` → 含 `com.xsift.sift`（历史事实）；`grep -E 'Label = ' internal/hosting/hosting.go`（HEAD）→ `cn.hexai.sift` + `com.miaoxiaoyong.sift`。YES。
  - 证据缺口：无（双向已核验）。
  - fixer=same（记录；已闭合，合并须携带两提交或 squash）。

- `[DEFER-1]` `internal/agents` `TestProbeVersion` 3s exec-timeout 环境 flake：全量套件并发/负载下偶发 `ProbeVersion(...)=""`，隔离 5/5 通过；该包本分支零改动（最后触碰 commit e10cbec，main），可能间歇性打红 CI。
  - 标尺：`go test -count=1 ./internal/agents/ -run TestProbeVersion` 连跑 5 次全 PASS；根因是 CommandContext 3s 超时在负载下打不满。
  - 证据缺口：未能稳定复现（环境相关）。
  - fixer=same（backlog，另开 Issue，非本 MR 范围）。

## Scope summary

| 级别 | 数量 | 本轮是否实施 |
|---|---|---|
| P0 | 0 | 是 |
| P1 | 0 | 是 |
| P2 | 1 | 否（记录） |
| DEFER | 1 | 否（backlog） |

## Verdict

**PASS** — 验收 A1–A7 全 YES，Preserve-intentionally 四项全核验通过；P0/P1 为 0，可进入合并流程（建议 squash 或整体合并两提交）。
