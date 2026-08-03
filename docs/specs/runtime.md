---
status: active
created: 2026-07-31
summary: 双后端执行、PTY 与资格契约
---

# Runtime 规格

本文冻结 M6 的 execution backend、wrapper-owned PTY、tmux session、attach 与进程组资格契约。启动 handoff 的 socket/文件字段仍以 [`control-plane.md`](control-plane.md) 为准，attempt/claim/资格持久化以 [`storage.md`](storage.md) 为准，恢复动作以 [DESIGN §10.1](../DESIGN.md#101-启动期恢复矩阵) 为准；本文不建立第二套状态或事实源。

## 1. 边界与不变量

1. `process` 与 `tmux` 只决定 **wrapper 在哪里启动**。二者启动同版本 `sift-agent-wrapper`，永不直接启动 Agent。
2. Agent 始终由 wrapper 经唯一 `Launcher` 接缝直接 spawn；执行期必须满足 `agent.ppid = wrapper.pid` 与 `agent.pgid = wrapper.pgid`。wrapper/control/result/process identity 是唯一执行证据链。
3. PTY 由 wrapper 建立，与 backend 无关。tmux pane tty、session、scrollback 不是 started、finished、owner 或消失证据。
4. backend/session observer 只能产生诊断和 attach 可用性；不得写 Run/attempt phase、释放 claim、补 started、判 finished 或授权 replacement。
5. 只有与当前 Agent 定义、二进制、版本、OS/arch 精确匹配的 `process-group-verified` 资格才允许“wrapper 与进程组均消失”成为自动 retry 的充分条件。无记录、证据不全或出现脱组后代一律 `process-group-unverified`。
6. `process-group-verified` 不是同 UID 隔离或沙箱。Agent 主动逃逸仍属 TM6；真实 Agent/version 的正式资格证据留 M7。

## 2. Effective backend 与冻结

### 2.1 选择算法

Raw Agent `backend` 非空时覆盖根 `runtime.backend`；省略时继承根值。配置规范化后每个 Agent 必须得到一个 concrete `process | tmux`，不再保留“省略”状态：

```text
effectiveBackend(agent) = agent.backend ?? runtime.backend
```

Run 创建时冻结 config snapshot；每个 attempt 从该 snapshot 中对应 Agent 的 effective backend 写入 `attempts.backend`。retry/new attempt 不读取当前磁盘配置，不能因 daemon 重启或配置漂移换 backend。launch claim/dispatch 必须读取 attempt 已冻结的 backend，不能由 worker 当前默认值重建。

Agent executable 与 tmux executable 使用启动探测得到的 symlink-resolved absolute path。原始配置拼写只进入 config snapshot；实际 launch、进程身份与资格 key 都使用冻结的 resolved path，PATH 漂移不得改变执行目标。

### 2.2 Backend 行为端口

两个 backend 接受同一冻结输入：

```text
HostLaunchV1 {
  backend, run_id, attempt_no, generation, dispatch_id,
  wrapper_path, bootstrap_path
}
```

所有 path 必须绝对；五元组 `(run_id, attempt_no, generation, dispatch_id, backend)` 必须与当前 claim/attempt 一致。backend 只可返回“wrapper host 请求已接受/已由同一 binding 收敛”或 typed error；返回成功不等于 wrapper acquired、permit issued 或 Agent started。

`process` 以 argv `[wrapper_path, bootstrap_path]` 启动 wrapper。`tmux` 按 §4 启动同一 argv。两者都不得调用 shell、继承 credential 环境或把 nonce/token/session/permit 放进 argv。

## 3. Wrapper-owned PTY

### 3.1 拓扑

wrapper 在 permit 前已建立自己的进程组。它创建 PTY master/slave，将 Agent stdout/stderr 连接到 slave，再通过既有 `PermitGate.StartOnce` 启动 Agent。Agent stdin 保留独立 task pipe（§3.2），避免 PTY 双向 master 无法可靠 half-close 而让 `stdin` transport 永远等不到 EOF。PTY 激活时 Agent child **禁止**设置 `Setsid`、`Setpgid` 或 `Setctty`；常见会在 child 内执行 `setsid + Setctty` 的 PTY helper 不得使用。

PTY slave 为 stdout/stderr 提供真实 tty file descriptor；V0 不承诺 stdin 是 tty，也不要求 Agent 成为 controlling-terminal session leader。需要交互 stdin/controlling terminal 的 Agent/version 在 V0 标为 unsupported/unverified，不得改变拓扑迁就。终端尺寸固定为 backend-neutral protocol constant `80x24`，不得读取 tmux pane 尺寸，否则同一 Agent 在两个 backend 下会得到不同输入环境。改变该常量属于 runtime protocol 变更，需同步资格 key 的 `method_version`。

生产拓扑测试必须在 PTY active 时读取 OS 事实并同时断言：

- wrapper PID 等于 wrapper PGID（wrapper 是该监督组组长）；
- Agent PPID 等于 wrapper PID；
- wrapper PGID 等于 Agent PGID；
- Agent PID 不等于 PGID（Agent 不是新组长）；
- 向已验证的负 PGID 发信号覆盖 wrapper、Agent 与仍在组内的后代。

### 3.2 字节中继

wrapper 从 PTY master 读取原始字节，每块先追加 `agent.log`，再 best-effort 写 wrapper stdout；tmux backend 的 stdout 自然进入 pane，process backend 的 stdout 只提供宿主观察。pane 写失败不得改变 Agent 裁定；`agent.log` 打开或写入失败则 wrapper 必须经既有受控终止路径回收进程组，并形成失败 result/诊断，不能继续一个无权威日志的执行。

- `stdin` task transport：wrapper 通过独立 finite input（owner-only regular file descriptor 或 pipe）提供 canonical Task Spec bytes；regular file 到末尾自然 EOF，pipe 写完后关闭 writer。task bytes 不进入 PTY master，也不转发宿主/attach 键盘输入。
- `file` task transport：Task Spec 仍只通过完整 `{task_file}` argv token；Agent stdin 使用 `/dev/null` 或空且已关闭的 pipe，PTY master 不注入任务内容。
- stdout/stderr 在 slave 合流，`agent.log` 保留实际 PTY 字节顺序；日志、pane 与 attach 均不构成完成证据。
- EOF、context cancel、Agent exit 与 signal forwarding 继续复用 wrapper 的单一 wait/reap/result 路径；不得为 PTY 建第二个 Agent spawn/wait 路径。

## 4. tmux wrapper host

### 4.1 Session identity

Session binding 是下列 closed canonical JSON 的 SHA-256：

```json
{"schema_version":1,"run_id":"...","attempt_no":1,"generation":1,"dispatch_id":"..."}
```

`binding_digest` 为 64 位小写 hex，session 名严格为 `sift-<binding_digest>`。名称不含原始 Run/Agent/project 文本或秘密，且只能由当前 claim/dispatch 派生。所有 tmux target 使用 exact form `=<session_name>`，禁止前缀匹配。

binding 无需新增可变事实：`attempts` 的 run/attempt/generation/backend 与 `attempt_claims.dispatch_id` 足以重建。若任一字段缺失或漂移，不得猜 session。

### 4.2 创建与响应丢失

使用启动探测冻结的 absolute tmux path，通过 argv 调用 `new-session`；不得构造 shell 字符串。创建请求必须同时设置非秘密 session environment `SIFT_TMUX_BINDING=<binding_digest>`，并以独立 argument 传 `wrapper_path`、`bootstrap_path`。tmux 版本必须支持 `shell-command [argument ...]` 的多 argv 形式；包含空格或 shell metacharacter 的测试路径必须逐字节到达 wrapper。

首次创建与响应丢失后的重入使用同一 session 名。若 exact session 已存在，仅在以下观测全部成立时返回“同 binding 已接受”：

1. `show-environment -t =<session_name> SIFT_TMUX_BINDING` 返回的 session-scoped 值精确等于 digest；
2. session 恰有一个 live pane，且未启用 `remain-on-exit` 接管 dead pane；
3. 当前持久 claim/dispatch/generation/backend 仍与 binding 一致。

任一不符返回 semantic conflict，禁止 kill、rename、attach 或接管同名 session。session 创建成功但响应丢失时，重入只能收敛到上述同 binding；不能创建第二 session 或第二 wrapper。session 后续消失只是一条 §5 observation，不回写 launch 成功/失败。

### 4.3 Session lifecycle

wrapper 退出后 pane/session 按 tmux 默认生命周期结束；不得以 `remain-on-exit` 保留 dead pane冒充活性。daemon 重启不终止 tmux server，wrapper 继续按 control/heartbeat/result 协议运行。tmux server/session 被杀时，是否仍有 wrapper/进程组只能由 process/control evidence 判断。

## 5. Backend session observation

统一 observation：

```text
BackendSessionObservationV1 {
  backend: process | tmux,
  state: not_applicable | present | absent | unknown,
  binding_digest?, observed_at_ms,
  diagnostic_code
}
```

- `process` 恒为 `not_applicable`。
- tmux `has-session -t =name` 成功且 binding 校验通过为 `present`；明确不存在为 `absent`；timeout、输出契约错误或 binding 不一致为 `unknown`（binding mismatch 另记安全诊断）。
- observation 不进入 Gate、claim、attempt_resolution 或 completion。只有与 wrapper/process/control 观测合取后，恢复协调器才能执行 DESIGN §10.1 已定义的动作。

关键不对称：

- session present、wrapper/process identity 不成立：不能据 session 宣称 Agent 活着或死了；进入既有身份确认/受控终止，确认不了则唯一 `startup_stall` + `waiting_human` + frozen isolation。
- wrapper identity 成立、session absent：继续监督 wrapper 并记录 backend-session-lost；不得 orphan、重发或换 owner。

## 6. 观察型 attach

### 6.1 `ops.attach` 只读解析

`ops.attach` 只接受 operator capability 与 closed params `{"run_id":"..."}`。服务端必须找到该 Run 唯一 active attempt（`pending | starting | spawning | running`）、确认 `backend=tmux`、claim 有当前 dispatch，并按 §4.1 重建 exact session。多个候选、缺 binding、process backend、terminal attempt 或 session 非 `present` 均 deterministic fail closed。

成功 result：

```json
{
  "run_id":"...",
  "attempt_no":1,
  "generation":1,
  "backend":"tmux",
  "session_name":"sift-<64 lowercase hex>"
}
```

该方法不写 Run/attempt/claim/Interrupt/Gate/outbox；通用 operator 安全审计仍可追加。daemon 不执行 attach，不把 tmux client 接入控制循环。

### 6.2 CLI

`sift attach <run>` 先完成 operator RPC 与 response version/closed-result 校验，再在 CLI 自身环境以 `LookPath` 解析 tmux 并直接执行：

```text
tmux attach-session -r -t =<session_name>
```

`-r` 强制 read-only client；CLI 不转发按键给 Agent，不接受用户提供的 session 名或额外 tmux args。attach 退出码只决定 CLI 退出码，不写领域事实。daemon 不可用时无 offline fallback。

## 7. Process-group qualification

### 7.1 Exact qualification key

资格 key 为下列 canonical JSON 的 SHA-256：

```json
{
  "schema_version":1,
  "method_version":"runtime-topology/v1",
  "agent_id":"...",
  "agent_definition_hash":"...",
  "executable_path":"/resolved/real/path",
  "executable_sha256":"...",
  "version_output_digest":"...",
  "goos":"linux",
  "goarch":"amd64"
}
```

`agent_definition_hash` 是 closed canonical `{"schema_version":1,"args":[],"task_transport":"stdin|file","version_args":[]}` 的 SHA-256；不含 backend/max concurrency，因为两个 backend 必须共享 wrapper→Agent 拓扑。`executable_sha256` 对 symlink-resolved regular file bytes 计算。版本命令只经 argv、在有界 context 与无凭据空环境中执行且必须 exit 0；`version_output_digest = SHA256(stdout_bytes || 0x00 || stderr_bytes)`，不 trim/改码，原始输出不进入普通事件。launch bootstrap 携带被测量的 executable SHA-256；worker 在 host spawn 前重验该 hash。Linux wrapper 在唯一 Launcher 调用前从已验证字节 materialize 一份 unlink 的只读 image，并经其 `/proc/self/fd` 执行；Darwin wrapper 使用已验证 private hard link（系统 sealed volume 上不可 link 的 executable 只可在重验后按其 immutable system path 执行）。任一不匹配则不得启动 Agent。worker 清除该 attempt 的 key；wrapper 写入 durable invalidation marker，恢复不得让旧 verified row 授权自动 retry。digest 与 executable bytes 防止同路径替换后继承旧资格。

### 7.2 状态与门控

资格状态只有 `process-group-verified | process-group-unverified`。verified 证据必须由 topology harness 观察完整生命周期：direct child、同 PGID、受支持后代不脱组、组终止后无继续写 worktree 的后代。启动探测成功、`kill(pid,0)`、配置声明或一次瞬时 PGID 检查都不构成资格。

精确 key 无 verified 记录时默认 unverified；同 key 只要存在 detached-descendant/identity-incomplete 的 unverified 证据，即 fail closed，不能由普通 daemon 重启自动覆盖。修复 Agent binary/args/version 会产生新 key，并需重新资格测试。

恢复侧 `ProcessGroupVerified` 只能查询当前 attempt 冻结的 exact key。unverified 时即便 inspector 报 wrapper/PGID absent，也不能自动释放 isolation/retry，必须走 `process_group_unverified` 的单一 Interrupt 路径。doctor 显示 exact key 的状态与 reason，但不得输出原始 version 文本或测试日志秘密。

M6 交付持久化机制、生产查询接线和 synthetic verified/detached fixtures；真实 Agent CLI/version 的正式记录属于 M7。Darwin 缺 native identity inspector 时仍按 identity unknown fail closed；原生 Darwin absence-proof 与完整恢复证据属于 M8 V15 的逐 OS 验收，不阻断 M6 的双 backend 逻辑矩阵。

## 8. 安全与错误分类

- tmux/session/binding 数据均非 credential，但仍不得接受用户输入重建；所有 target 来自 current durable identity。
- tmux argv、wrapper argv、PTY 和 attach 不继承 daemon credential。wrapper→Agent 环境仍只有 `SIFT_RUN_DIR`。
- expected session 不存在为 backend observer 的 transient/diagnostic；binding mismatch、multiple pane、dead pane 或 claim drift 为 semantic conflict；tmux executable/capability 缺失为 auth-or-capability/startup failure。`ops.attach` 将 absent/unknown 与 semantic mismatch 都投影为非重试 `conflict`，因为 attach 没有领域重试协议；不得把 session absence 误报为 Run `not_found`。
- identity/session unknown 永远不能降级为 absent。不得向身份不确定 PID/PGID 发信号。
- session observation 与 qualification evidence 不进入 Brain/Gate，不能抑制单条 HITL 或改变 merge 决策。

## 9. 验收映射

| 契约 | 实现 Issue | 最终证据 |
|---|---|---|
| PTY + direct child/shared PGID | #844 | PTY-active process topology + signal/reap tests |
| tmux host + backend router | #845 | tmux production topology、response-loss reclaim |
| observational attach | #846 | closed RPC、`attach -r` argv、零领域写入 |
| session mismatch + qualification gate | #847 | 两 mismatch 行、verified/unverified synthetic fixtures |
| hooks carryover | #848 | project baseline 与 completion recheck |
| V2 dual backend | #849 | backend-parameterized handoff/crash suite |
| non-human V4 rows | #850 | DESIGN §10.1 row inventory |
| human/concurrent V4 | #851 | interleavings、kill/retry、four discoverers |
| M6 conclusion | #852 | repeated/race suite + independent review |

测试逐行 inventory 见 [`../testing/runtime-matrix.md`](../testing/runtime-matrix.md)。
