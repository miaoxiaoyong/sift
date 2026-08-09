---
status: active
created: 2026-08-09
summary: 合并 CLI 与 daemon 为单一 sift 二进制
---

# ADR-014：单一 `sift` 二进制

## 决策

`cmd/siftd` 不再作为独立命令发布。daemon 启动逻辑由 `cmd/sift` 的
`daemon` 子命令调用；发布归档只包含 `sift` 与独立的
`sift-agent-wrapper` 两个二进制。

## 边界

这是进程入口的合并，不改变控制面契约：`siftd.sock`、`siftd.lock`、
`run.sock`、`sift.db`、版本握手和数据库写入边界均保持不变。daemon 从
运行中的 `sift` 可执行文件所在目录解析同版本 wrapper，不查找 `PATH`。

## 后果

- 托管配置与手工启动统一使用 `sift daemon`。
- 旧的 `siftd` 命令不再提供兼容 shim；升级时需更新启动入口。
- ADR-009 中“Go 单模块三二进制”的发布数量描述由本决策取代，Go
  技术栈决策本身仍然有效。
