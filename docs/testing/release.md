---
status: active
created: 2026-08-10
summary: 发布构建与安装的测试方案（派生自 specs/release.md）
---

# 发布测试方案

派生自 [`../specs/release.md`](../specs/release.md)。覆盖 M8 §8.1（Issue #903）的四个验收面：归档产出、安装布局、版本握手可见性、四组合构建不回归。

## 1. 测试金字塔

| 层 | 载体 | 内容 |
|----|------|------|
| 单元 | `internal/version` | 默认值 canonical SemVer；版本语法（含路径分隔符拒绝） |
| 单元 | `internal/install` | manifest closed 解码、sha256 校验、探测一致、traversal/symlink 拒绝、同版本重装拒绝、staging 清理、`current` 原子切换 |
| 单元 | `internal/controlplane` doctor | `version` / `version:wrapper` 检查：一致 ok、不一致 error、缺失/不可执行 warning |
| 单元 | `cmd/sift` | `--version` 输出；`install` 参数纪律；`sift install` 端到端布局 |
| 集成 | `tools/release verify` + CI `release-smoke` | 四归档 ×（两二进制 + manifest）+ checksums 三方一致；linux/amd64 原生 install 冒烟 |
| 回归 | 既有 CI `build-matrix` / `check` | 四组合 `CGO_ENABLED=0 go build ./...` 与全测试套件不回归 |

## 2. 关键断言

1. **归档内两二进制同 release 版本**：manifest 单一 `release_version`；install 探测两二进制 `--version` 必须都等于它（`TestInstallRejectsVersionProbeMismatch`）。
2. **原子 current**：`current` 是相对 symlink；切换经 temp+rename（`TestSwitchCurrentReplacesExistingLinkAtomically`），失败不留 temp link（`TestSwitchCurrentLeavesNoTempLinkOnError`）。
3. **不逐文件覆盖**：同版本重装报错且版本目录字节不变（`TestInstallRepeatedSameVersionDoesNotOverwriteFiles`）。
4. **握手不变量不放宽**：daemon 启动拒绝路径（`runtime.ResolveWrapper` 既有测试）保持不变；doctor 对不一致可见且为 error（`TestWrapperVersionChecksMismatchIsError`）。
5. **V14 不回归**：goreleaser `before.hooks` 保留 `go generate ./...`；CI `schema-drift` job 继续 diff 生成物。

## 3. 运行

```bash
go test ./internal/version/... ./internal/install/... ./internal/controlplane/... ./cmd/sift/...
./scripts/release-snapshot.sh        # goreleaser release --snapshot --clean + verify
go run ./tools/release verify --dist dist
```

CI `release-smoke` job 在 ubuntu runner 上跑完整快照管线并对 linux/amd64 归档做原生安装冒烟。
