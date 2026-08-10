---
status: active
created: 2026-08-10
summary: 发布版本、GoReleaser 归档、安装布局与握手不变量契约
---

# 发布规格

本文定义 M8 §8.1（Issue #903）的发布契约：release 版本方案、GoReleaser 归档与 manifest、`~/.sift/bin/<version>/` 安装布局与原子 `current` 切换、以及不得放宽的版本握手不变量。实现为 `.goreleaser.yml`、`internal/version`、`internal/install`、`tools/release`；测试基准见 [`../testing/release.md`](../testing/release.md)。

需求与结构来源：[WBS M8 §8.1](../WBS.md)、[DESIGN §8.4 升级段落与 §8.10](../DESIGN.md)、[`control-plane.md` §3.4](control-plane.md)、[`runtime.md` §7](runtime.md)。

## 1. 版本三元组（互不混淆）

| 版本 | 位置 | 值 | 变更权 |
|------|------|----|--------|
| **release 版本** | `internal/version.Release`（ldflags `-X` 注入，默认 `0.1.0-dev`） | canonical SemVer，如 `0.1.0`、`0.1.0-dev` | 发布管线；每个归档内两个二进制必须同值 |
| **wire 协议版本** | `controlplane.ProtocolMajor/Minor` | `1/0` | 协议破坏性变更才动；本 Issue 不改 |
| **config 协议版本** | `config.Version` | `1` | config schema 破坏性变更才动；本 Issue 不改 |

release 版本由 GoReleaser 从 git tag（`vX.Y.Z`，去 `v` 前缀）注入；`--snapshot` 与 dev 构建恒为 `0.1.0-dev`（`.goreleaser.yml` 的 `snapshot.version_template` 与代码默认值必须始终一致）。

## 2. 归档契约

每个组合（darwin/linux × amd64/arm64）一个归档，命名 `sift_<release>_<goos>_<goarch>.tar.gz`，根目录含：

- `sift`（daemon + CLI）
- `sift-agent-wrapper`
- `manifest.json`（closed contract，见下）

归档旁生成 `checksums.txt`（GoReleaser 对每个归档的 sha256）。

### 2.1 manifest.json

```json
{
  "schema_version": 1,
  "release_version": "0.1.0-dev",
  "artifacts": [
    {"goos": "darwin", "goarch": "amd64", "name": "sift", "sha256": "…"},
    {"goos": "darwin", "goarch": "amd64", "name": "sift-agent-wrapper", "sha256": "…"}
  ]
}
```

- `schema_version` 必为 `1`；未知字段 fail closed（经全系统 decode gateway 的 `closed` 策略）。
- `release_version` 必须为 canonical SemVer；它是安装目录名，必须不含路径分隔符。
- `artifacts` 覆盖四个组合 × 两个二进制；sha256 为 64 位小写十六进制。
- 生成：goreleaser 每 build 的 post hook 调 `go run ./tools/release manifest --dist dist --version {{.Version}} --allow-partial`，从已构建二进制计算哈希；归档步骤在全部 build+hook 完成后才开始，因此最终 manifest 恒为完整 8 项。
- 校验：`go run ./tools/release verify --dist dist` 断言 8 项齐全、归档内容与 manifest/checksums 三方一致。

## 3. 安装布局与原子切换

```
~/.sift/bin/<release-version>/sift
~/.sift/bin/<release-version>/sift-agent-wrapper
~/.sift/bin/<release-version>/manifest.json
~/.sift/bin/current -> <release-version>   (相对 symlink)
```

- `sift install <archive.tar.gz>` 全有或全无：解压到 `bin/.staging-*` → 校验 manifest（closed、schema、release SemVer）→ 校验当前平台两个二进制的 sha256 → 探测两二进制 `--version` == manifest release → 目录重命名激活 → 原子切换 `current`。
- 原子切换是 **temp+rename**：先建 `bin/.current-tmp` symlink 指向新版本，再 `rename` 覆盖 `current`；禁止逐文件覆盖正在使用的安装。
- **重复安装同一版本拒绝**（报「already installed」），绝不改写版本目录内文件；升级安装新版本时旧版本目录保持原样。
- 解压安全：只接受 regular file + clean 相对路径；symlink/hardlink/`..`/绝对路径拒绝。
- `sift daemon` 只从自身二进制所在目录解析同版本 wrapper（`runtime.ResolveWrapper`，从不查 PATH；DESIGN §8.4）；`current` 的 symlink 解析后即落在真实版本目录，daemon 与其 wrapper 同目录同版本。

## 4. 握手不变量（不放宽）

- CLI↔daemon：RPC envelope 的 `client_version`/`server_version` 均为 release 版本；binary major 不同拒绝（`unsupported_binary`，`control-plane.md` §3.2）。
- daemon↔wrapper（文件级）：wrapper 与 daemon 同目录安装；启动时 `ResolveWrapper` 探测 wrapper `--version`，与 release 版本不等即拒绝启动。
- wrapper↔daemon（bootstrap 级）：`bootstrap.json` v2 的 `daemon_version`/`wrapper_version` 与 wrapper 自身 release 版本完全相等校验（`runtime.md` §7）。
- **doctor 可见性**：`sift doctor` 报告 `version`（release 版本）与 `version:wrapper` 两项；wrapper 缺失/不可执行/探测失败为 warning，**wrapper release 版本与运行中二进制不一致为 error（exit 2）**。

## 5. 非范围

- A10 干净机安装证据（Human Gate，M7 通过后）。
- live 跨版本 DB 升级不丢数据（留 M7 真实数据；只做契约/单元级回归）。
- Homebrew tap 实际发布（Issue C）。
- 既有协议握手语义与 `config.Version` 一律保持。

## 6. 验收映射

| Issue #903 验收 | 判据 |
|-----------------|------|
| goreleaser 干跑产出 4 个单归档，各含两二进制 + manifest + sha256 | `scripts/release-snapshot.sh`（= `goreleaser release --snapshot --clean`）+ `tools/release verify`；CI `release-smoke` job |
| 安装逻辑产出版本目录 + 原子 `current` symlink；重复安装不逐文件覆盖 | `internal/install` 测试（含同版本重装拒绝、staging 清理） |
| `sift --version` 报 release 版本；握手不一致仍被拒且 doctor 可见 | `cmd/sift` 与 `controlplane` doctor 测试；daemon 启动路径既有拒绝保持 |
| 四组合 `CGO_ENABLED=0 go build ./...` 通过 | 既有 CI `build-matrix` job 不回归 |
