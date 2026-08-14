---
status: done
created: 2026-08-14
summary: 使 launchd service start 幂等并诊断 GUI 域错误
---

# #965 launchd start

## 范围

- `sift service start` 不改写 plist，也不复用 `install` 的 bootout replacement。
- 已加载 `cn.hexai.sift` label 使用 `launchctl kickstart`；未加载时仅在保留 plist 存在的情况下 `launchctl bootstrap`。
- launchctl 的真实非零失败必须传播；权限错误不能被当作“未加载”吞掉。
- SSH 或未登录 GUI 导致的 user GUI domain 错误，输出登录 GUI 后重试或前台运行的可行动提示。
- 保持 #959 的固定 launchd PATH、install 行为与稳定 label 不变。

## 实施与测试

1. 为 start plan 增加 label probe、kickstart 与 bootstrap fallback；仅将明确的“service 不存在”视为未加载。
2. 补 hosting 单测覆盖 loaded/unloaded、bootstrap/kickstart failure、权限与 GUI domain 诊断；补 CLI 回归覆盖 start 不写 plist。
3. 同步 [`specs/hosting.md`](../specs/hosting.md)、[`testing/hosting.md`](../testing/hosting.md) 与 [`runbooks/troubleshooting.md`](../runbooks/troubleshooting.md)。
4. 运行 `go test ./...`、`go build ./...`、`go vet ./...`。
