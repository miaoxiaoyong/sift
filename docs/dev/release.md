---
status: active
created: 2026-08-11
summary: 发布归档、升级与干净机验收操作手册
---

# 发布 Sift

本文面向发布维护者，说明仓库中已经接通的 GoReleaser、归档校验、原子安装和用户级托管流程。发布契约以 [`../specs/release.md`](../specs/release.md) 为准；需求和验收边界只引用 [WBS M8](../WBS.md)、[DESIGN §11 / V15](../DESIGN.md) 与 [PRD §9.3 / §10.1](../PRD.md)，本文不复制它们。

> 本文中的“干净机流程”是执行清单，不是 A10 Human Gate 证据。只有实际机器的日志和验收记录齐全后，才能声明发布门禁通过。

## 1. 发布前门禁

发布机需要 Go（当前稳定版）、GoReleaser v2、Git 和 GitHub 发布凭据。目标机不需要 Go。安装 GoReleaser v2：

```bash
go install github.com/goreleaser/goreleaser/v2@latest
```

在干净 checkout 中执行：

```bash
go generate ./...
git diff --exit-code -- internal/schema/artifacts/ internal/brain/prompts/
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test ./...

for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64; do
  GOOS=${target%/*} GOARCH=${target#*/} CGO_ENABLED=0 go build ./...
done
```

四组合构建是 `CGO_ENABLED=0` 硬门禁，不得为某个平台临时打开 CGO。权威 CI 实现在 [`.github/workflows/build.yml`](../../.github/workflows/build.yml) 的 `build-matrix`、`schema-drift`、`check` 和 `release-smoke` jobs。

先做不会发布的完整快照：

```bash
./scripts/release-snapshot.sh
```

它运行 `goreleaser release --snapshot --clean`，随后调用 release verifier。成功时 `dist/` 应有四个 `tar.gz`、`manifest.json` 和 `checksums.txt`。

## 2. Manifest 与产物审计

版本 tag 必须是 `vX.Y.Z`；GoReleaser 将去掉 `v` 并把同一个 release 版本注入两个二进制。归档命名为：

```text
sift_<version>_<darwin|linux>_<amd64|arm64>.tar.gz
```

每个归档根目录都必须同时包含：

```text
sift
sift-agent-wrapper
manifest.json
```

不要手工改 manifest 或重打 tar 包。生成和校验规则（含每个二进制的 SHA-256、closed schema 和八项完整矩阵）见 [`../specs/release.md` §2](../specs/release.md)。复核命令：

```bash
go run ./tools/release verify --dist dist
```

`checksums.txt` 校验的是发布归档；归档内 `manifest.json` 校验的是两个二进制。两层都必须通过。

## 3. 正式构建与发布

确认 checkout 干净、当前提交就是待发布提交，并已创建对应 tag。再次完成 §1 快照门禁后，运行：

```bash
goreleaser release --clean
```

该命令会创建/更新 forge Release，不能当作 dry-run。发布后从 Release 重新下载四个归档和 `checksums.txt`，不要用本地 `dist/` 代替远端复核：

```bash
sha256sum --check checksums.txt       # Linux
shasum -a 256 -c checksums.txt        # macOS
```

只保留当前目录中已下载的四个归档再执行校验，避免 `checksums.txt` 中不存在的附属 artifact 造成误判。逐个解包并执行两个 `--version`；输出必须都等于归档版本。

Homebrew formula 只有在真实版本、真实归档 SHA-256 和 tap 仓库均已发布后才是用户渠道。仓库中的 formula 草稿不能作为已发布 tap 的证据，禁止把占位 checksum 发布给用户。

## 4. 升级流程

归档安装布局和失败原子性见 [`../specs/release.md` §3](../specs/release.md)。正常升级不覆盖运行中的文件：

```bash
sift install /path/to/sift_<new-version>_<os>_<arch>.tar.gz
sift service restart
sift --version
sift doctor
```

`install` 先校验归档、两个二进制版本与 SHA-256，再创建 `$SIFT_HOME/bin/<version>/`，最后以 temp symlink + rename 切换 `current`。同版本重装会拒绝；不要删除版本目录后逐文件复制。

数据库迁移在 daemon 打开数据库时前向执行。较旧 binary 遇到较新的数据库 schema 会拒绝启动，因此**切回旧 `current` 不等于数据库回滚**。需要灾备副本时，应先停止 daemon，再复制完整的 `SIFT_HOME`；不要在 WAL 写入期间只复制 `sift.db`。

升级后检查：

```bash
readlink "$SIFT_HOME/bin/current" 2>/dev/null || readlink "$HOME/.sift/bin/current"
sift service status
sift doctor
```

版本不匹配必须先修复成同一归档中的 `sift` + `sift-agent-wrapper`，不得绕过握手。

## 5. 干净机执行清单（A10）

分别在干净 macOS 与采用 systemd 的 Linux 主机执行，并保存命令、输出、OS/架构、release SHA 和时间：

1. 从 forge Release 下载当前平台归档和 `checksums.txt`，校验归档 SHA-256。
2. 按 [`../guides/installation.md`](../guides/installation.md) 从归档安装；目标机不得预装或调用 Go。
3. 配置实际 Agent、项目及对应 `gh`/`glab` 登录，保持 `SIFT_HOME` 属主权限。
4. 安装用户级托管单元；Linux 需要无人登录常驻时显式启用 linger。
5. 运行 `sift --version`、wrapper `--version`、`sift service status` 和 `sift doctor`。
6. 证明 SQLite、`siftd.sock`、`run.sock` 和 wrapper handoff 均实际工作，而不只检查文件存在。
7. 跑一次真实闭环和恢复场景；杀死 daemon 后确认 supervisor 启动了新进程且新 socket 可连接。
8. 安装下一 release，确认 `current` 原子切换，重启后数据库状态与轮询游标不丢失。
9. 保存四种 OS/架构组合的安装与二进制冒烟记录；完整闭环/恢复至少每个 OS 一次。

具体发布门槛仍以 [DESIGN V15](../DESIGN.md) 与 [PRD §10.1“发布”](../PRD.md) 为准。未完成上述实机记录时，结果只能写“流程已定义”，不能写“A10 已通过”。
