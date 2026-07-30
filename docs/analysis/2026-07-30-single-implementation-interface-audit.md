---
status: done
created: 2026-07-30
summary: 单实现接口保留与收敛结论
---

# 单实现接口审计（T011）

跟踪 Issue：[#752](https://github.com/miaoxiaoyong/sift/issues/752)。

## 结论

| 包 | 接口 | 生产实现 | 测试替身/其他实现 | 结论 |
|---|---|---|---|---|
| `internal/attempt` | `Runner` | 无 | `FakeAgent`；唯一消费者是 `internal/skeleton` 测试链 | **收敛**：删除接口，测试链直接依赖 `*attempt.FakeAgent` |
| `internal/launchworker` | `Backend` | `ProcessBackend` | `countingBackend`、`execWrapperBackend`、`pausedRecoveryBackend` | **保留**：crash/recovery 测试需要替换 spawn 边界 |
| `internal/channelworker` | `SecretResolver` | `EnvironmentSecretResolver` | `resolverFunc` | **保留**：测试必须注入 secret，避免读取真实环境 |
| `internal/channelworker` | `WebhookSender` | `HTTPWebhookSender` | `senderFunc` | **保留**：测试必须隔离 HTTP side effect 与错误脱敏 |

## 判定依据

- 接口是否隔离生产 side effect，而非仅包装单个结构体。
- 是否已有独立测试实现支撑失败、崩溃恢复或安全边界验证。
- 收敛后是否让依赖关系更诚实。`attempt.Runner` 没有生产实现，且唯一消费者本身就是 M1 skeleton 测试链；保留接口会暗示不存在的运行时多态。
- `channelworker` 的事实基线是两个接口，不是一个；二者分别隔离 secret resolution 与 HTTP publish，不能合并计算。

本审计不改变 launch、Channel 或预算行为。现有 skeleton、launchworker、channelworker 测试是代码变更的回归门禁。
