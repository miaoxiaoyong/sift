---
status: done
created: 2026-08-13
summary: 同仓库多机器协作的现状、价值与演进路线
---

# 同仓库多机器 Sift 协作探索

> 本文结论已纳入用户入口：同一仓库/触发标签只能有一个主动 Coordinator。这里保留架构推演，不代表 Coordinator + Runner 已实现。

## 1. 问题

很多开发者分别在自己的机器安装 Sift，是否可以同时为同一个 GitHub/GitLab 仓库工作？这种能力是否有产品价值？

“多端协作”至少有四种不同含义，必须分开讨论：

1. 多名人类通过 Forge 评论、审批和 Command 操作同一个 Sift 实例；
2. 多台机器各自处理同仓库中互不相同的 Issue；
3. 多台机器从同一个 Issue 池动态抢占任务；
4. 同一个 Run/attempt 在机器之间迁移或故障接管。

## 2. 当前能力与边界

PRD §2.1 与 DESIGN §11 明确：当前形态是单机、单实例、单用户；每个安装独立运行，实例之间没有协调、共享存储或网络交互。

### 2.1 已经可行：多人操作一个实例

项目可配置多个 operator。团队成员可通过 Forge 评论、标签和审批共同控制由一台机器托管的 Sift；Run、Gate、Command 和 Ledger 仍由该实例的单一 SQLite 裁决。

这已经构成一种轻量团队模式：**一个执行协调器，多个人类操作者**。

### 2.2 有条件可行：静态分区不同 Issue

多台机器可以人为约定互不重叠的项目或触发标签，例如不同机器只消费不同仓库，或使用 `sift:alice` / `sift:bob` 一类静态标签。各机仍是完全独立实例。

这种方式不是第一方多机调度：没有统一队列、机器状态、动态负载均衡、故障接管或全局预算；配置错误仍可能重复消费。

### 2.3 当前不安全：同标签动态抢占

每台机器有独立的 `~/.sift/sift.db`。SQLite 内的 Run 唯一约束、outbox lease、attempt claim、wrapper session 和 fencing 只对**同一数据库/daemon**生效。

如果两台机器同时监听同一仓库和同一触发标签：

1. 两边都能观察同一个远端 label event；
2. 两个本地数据库都会认为自己首次摄入；
3. 两边可能同时创建 Run、worktree、评论、分支或 Change；
4. 本地 operation lease 和 attempt fencing 无法阻止另一台机器；
5. Forge 的重复 PR、分支冲突或评论 marker 只能事后收敛部分副作用，不能充当可靠的执行互斥。

因此当前不能宣称支持“多台独立 daemon 对同一任务池自动抢单”。

### 2.4 当前不支持：跨机器 Run 接管

attempt 的 wrapper/Agent 进程、worktree、run.sock、control 文件和 tmux session 都属于本机。另一台机器既没有完整权威状态，也无法安全证明旧执行体已经消失。因此同一个 Run 的跨机器恢复不能只靠复制 SQLite 或超时换 owner。

## 3. 产品价值

该能力对个人用户不是首要需求，但对小团队有明显价值：

- **汇集闲置算力**：Mac、Linux 工作站和常驻服务器共同承担 Issue；
- **汇集不同 Agent/订阅**：某机器有 Claude，另一台有 Codex、本地模型或特定凭据；
- **能力路由**：按操作系统、GPU、模型、仓库权限和工具链选择执行节点；
- **可用性**：笔记本离线后，常驻节点继续处理新任务；
- **并行吞吐**：多个独立 Issue 分派到不同机器；
- **团队治理**：统一看到谁领取了什么任务、预算使用、Gate 和审批记录；
- **隐私与数据边界**：代码可留在团队自管机器，而不是上传到托管执行平台。

最有价值的形态不是“多个 daemon 无协调地抢 Issue”，而是：

> 一个权威协调器管理项目与任务，多个受信执行节点提供不同 Agent、算力和环境。

## 4. 推荐架构：Coordinator + Runner

不要把当前多个完整 Sift 实例直接改成 peer-to-peer。推荐将职责拆成：

### 4.1 Coordinator

每个项目只有一个权威协调器，负责：

- Forge Intake 和远端事件游标；
- Run/attempt 状态与唯一 claim；
- Brain、Gate、Command、Ledger 和预算；
- Runner 注册、能力目录、调度与租约；
- Change 创建、合并和通知等外部副作用。

### 4.2 Runner

每台开发机运行受限 Runner，负责：

- 上报 OS、架构、Agent、模型、GPU、并发槽位和健康状态；
- 接收已签名的 attempt dispatch；
- 创建本地隔离 worktree；
- 启动 wrapper/Agent 并回传结构化事件、日志和结果；
- 不自行 Intake、不决定 Gate、不直接产生未经 Coordinator 授权的远端副作用。

### 4.3 安全不变量

- 一个项目在任一时刻只有一个 Coordinator epoch；
- attempt dispatch 带 runner id、generation、不可重放令牌和过期时间；
- Coordinator 不能仅凭 heartbeat 超时将任务转移到另一机器，必须处理“旧 Runner 可能仍在写代码”的不确定性；
- 远端分支命名和 push 必须带 attempt generation，并使用 expected-head/CAS；
- Runner 凭据最小化，不把所有 Forge/operator 权限复制到每台机器；
- 同一个 attempt 不做透明跨机器迁移；故障后应创建新 attempt，并隔离旧分支/工作区结果。

## 5. 可分阶段演进

### 阶段 A：文档化安全团队模式

- 明确“一仓库一个主动协调器”；
- 支持多个 operator；
- 给静态标签/仓库分区提供配置示例；
- doctor 检测已知重复 project ownership（能检测到的范围内）。

### 阶段 B：远程只读与人工控制

- 团队成员可从其他机器查看 `ps/timeline/logs`；
- 经认证调用 kill/retry/approve；
- 执行仍只在 Coordinator 所在机器。

### 阶段 C：Coordinator + Runner MVP

- Runner 注册和能力上报；
- 每个 Run 固定到一个 Runner；
- 只支持新 attempt 调度，不做运行中迁移；
- 一个项目仍只有一个 Coordinator。

### 阶段 D：团队调度

- 能力约束、并发槽位、预算和公平队列；
- Runner 离线后的受控新 attempt；
- 统一审计、可观测性和凭据治理。

高可用 Coordinator、共享 Postgres 和 peer-to-peer 选主应后置；它们会把产品从本地工具推向分布式控制面。

## 6. 不推荐的捷径

- **Issue assignee/label 作为锁**：Forge 更新通常没有足够强的 CAS 和租约语义，竞态下仍会双领；
- **多个实例共享 SQLite 文件**：跨机器文件系统、WAL、进程身份和本地 runtime 证据均不成立；
- **复制数据库后接管**：无法证明旧 wrapper/Agent 已消失；
- **仅靠分支名避免冲突**：只能减少 push 碰撞，阻止不了重复 Agent、评论、API 调用和预算收费；
- **先做 peer-to-peer**：选主、网络分区、凭据和 fencing 成本远大于 Coordinator + Runner。

## 7. 结论

- **当前**：支持“多个安装分别工作”，也可由多名 operator 协作一个实例；不支持多个独立 daemon 安全消费同仓库同标签。
- **短期安全建议**：一个仓库只启用一个主动 Sift 协调器；多机器使用不同仓库或显式静态分区。
- **长期价值**：存在，且对小团队、异构 Agent/算力和自托管场景很强。
- **推荐方向**：将其定义为“团队 Runner 池”，采用单权威 Coordinator + 多 Runner，而不是多实例无协调抢占。
