---
status: active
created: 2026-07-28
summary: Unix socket、能力授权与 wrapper 控制文件契约
---

# 控制面规格

本文冻结 `siftd.sock`、`run.sock`、operator/run/wrapper capability、RPC envelope，以及 wrapper 启动与控制文件的字段级契约。

结构来源：[DESIGN §3.2、§8.4、§8.9–§8.10、§10.1](../DESIGN.md)，[ADR-005](../decisions/005-execution-backend-and-wrapper-contract.md)、[ADR-008](../decisions/008-control-plane-endpoints-and-capabilities.md)、[ADR-010](../decisions/010-attempt-spawn-handoff.md)、[ADR-012](../decisions/012-process-group-supervision-boundary.md)，存储事务见 [`storage.md`](storage.md)，路径与权限见 [`config.md`](config.md)。

## 评审处置

评审原文：[2026-07-28-control-plane-review-pi-gpt-5.6-sol.md](../reviews/2026-07-28-control-plane-review-pi-gpt-5.6-sol.md)。

| 发现 | 处置 |
|------|------|
| C1 daemon 派生根扩大 Agent 权限 | 删除根秘密；改为 dispatch prepare + wrapper candidate |
| C2 launch 崩溃窗口未展开 | 冻结 prepare/file/spawn 顺序及逐点恢复 |
| C3 ops 读响应不 closed | 补 ps/logs/worktree/doctor params/result schema |
| C4 file transport 不通用 | 使用完整 `{task_file}` argv token 替换 |

C1–C4 已关闭，评审通过，本规格转为 `active`。

## 1. 边界与不变量

1. V0 只监听两个 Unix domain stream socket；不得创建 TCP/UDP listener。
2. `siftd.sock` 只承载 operator RPC；`run.sock` 只承载 Report 与 wrapper 启动握手。方法不能跨 socket 注册。
3. 服务端按 **socket + method + credential kind + credential binding** 授权，不信任客户端声明的角色、子命令名或 UID。
4. 所有请求经唯一 decode gateway 的 `closed` 策略；未知字段、错误类型、未知方法和未知枚举均拒绝。
5. `sift`、wrapper、Agent 均不直连 SQLite；RPC handler 只能调用 [`storage.md` §11](storage.md) 的受限端口。
6. run token 的 Report 动词不写 `runs.status`；wrapper credential 只能推进绑定 attempt 的启动协议。
7. credential 不进入 argv、环境变量、事件 payload、日志或错误文本。比较 hash 时使用常量时间比较。
8. V0 的 operator token 不是同 UID Agent 的隔离边界；`doctor` 必须持续报告 `unsafe-local`。

## 2. Socket 生命周期

| socket | 模式 | 调用方 | credential |
|--------|------|--------|------------|
| `$SIFT_HOME/siftd.sock` | `0600` | `sift` CLI | operator token |
| `$SIFT_HOME/run.sock` | `0600` | `sift report`、wrapper | run token / bootstrap nonce / wrapper session + spawn permit |

启动顺序：取得 `siftd.lock` → 加载 credential → 打开/迁移数据库 → 对 socket 路径执行 `lstat` → 安全清理 stale socket → bind → chmod 0600 并复核 → listen。规则：

- 路径不存在时直接 bind；
- 路径是 symlink 或非 socket 时拒启，不删除；
- 路径是 socket 时，只有当前进程已持有 daemon lock 才可 unlink 后重建；
- bind 后实际 owner 必须是 daemon UID；Linux 以 `SO_PEERCRED`、macOS 以 `getpeereid` 再校验 peer UID 与 daemon UID 相同；失败先拒绝，再进入 token 校验之前的安全审计；
- 正常停止关闭 listener 并 unlink 自己创建的 socket；崩溃残留由下一次启动按上述规则处理。

每个连接只处理一个 request/response 后关闭，避免连接级身份状态和请求间污染。读写均设置有界 deadline；deadline 属内部协议常量，不改变领域时限。

## 3. Framing 与 envelope

### 3.1 Framing

一帧为 `4-byte unsigned big-endian length + UTF-8 JSON bytes`。V0 request/response body 硬上限均为 `1048576` bytes；长度为 0、超限、非 UTF-8、尾随第二个 JSON value 或提前 EOF 均以 transport error 关闭连接。该上限是协议防分配常量，不是运行策略配置。

JSON 数字不得为 NaN/Infinity。网络帧无需作为存储 hash 输入；需要落库的 payload 由 handler 按 [`config.md` §4](config.md) 再 canonicalize。

### 3.2 Request v1

```json
{
  "protocol_major": 1,
  "protocol_minor": 0,
  "client_version": "0.1.0",
  "request_id": "0123456789abcdef0123456789abcdef",
  "method": "ops.ps",
  "auth": {"kind": "operator", "token": "<64 lowercase hex>"},
  "params": {}
}
```

| 字段 | 约束 |
|------|------|
| `protocol_major` | V0 必须为 `1` |
| `protocol_minor` | V0 必须为 `0`；高于服务端时拒绝，不猜兼容 |
| `client_version` | canonical SemVer；major 必须与 server binary major 相同，完整值进入诊断但不参与授权 |
| `request_id` | 32 位小写十六进制；调用方随机生成 |
| `method` | 本规格方法全集之一 |
| `auth` | closed tagged union；kind 必须符合 socket/method |
| `params` | 对应 method 的 closed object |

协议/二进制版本先按 envelope 最小 schema 解码；protocol major/minor 不支持时返回 `unsupported_protocol`，binary major 不同返回 `unsupported_binary`；均不得继续解码方法参数或执行领域动作。

### 3.3 Response v1

成功：

```json
{
  "protocol_major": 1,
  "protocol_minor": 0,
  "server_version": "0.1.0",
  "request_id": "0123456789abcdef0123456789abcdef",
  "ok": true,
  "result": {}
}
```

失败：

```json
{
  "protocol_major": 1,
  "protocol_minor": 0,
  "server_version": "0.1.0",
  "request_id": "0123456789abcdef0123456789abcdef",
  "ok": false,
  "error": {
    "code": "unauthorized",
    "message": "credential rejected",
    "retryable": false,
    "details": {}
  }
}
```

`ok=true` 时只能有 `result`；`ok=false` 时只能有 `error`。错误文本不得回显 token、nonce、session、permit、原始 auth、控制文件内容或数据库错误。

### 3.4 版本握手

每次 RPC 都携完整的四元组：request 的 `(protocol_major, protocol_minor, client_version)` 与 response 的 `server_version`；不能因为调用方是同一归档中的 CLI/wrapper 而省略任一字段。`sift`、`sift-agent-wrapper` 分别以自己的 canonical SemVer 填 `client_version`，daemon 以自己的版本填 `server_version`。服务端按 §3.2 的顺序在鉴权和 params 解码前拒绝不兼容版本；客户端也必须在使用 `result/error` 前校验 response envelope、request id、protocol 版本和 server binary major。

wrapper 还有一段文件到进程的握手：[`bootstrap.json` v2](#71-bootstrapjson-v2) 必须携 `protocol_major`、`protocol_minor`、`daemon_version`、`wrapper_version`。wrapper 先校验自身版本与 `wrapper_version` 完全相等、再校验 daemon/wrapper binary major 与协议版本；失败时 unlink 已读取的 bootstrap、不得调用 acquire、更不得写 control 或 spawn。随后 `claim.acquire` 的 RPC envelope 再以 wrapper 实际版本作为 `client_version` 完成双向校验，文件字段不能替代 RPC 握手。

**破坏性变更：** 本次冻结将旧 `bootstrap.json` v1 升为 v2，并新增三个必填版本字段（`protocol_minor`、`daemon_version`、`wrapper_version`）；v1 与字段缺失文件必须 fail closed，不做默认补值。RPC envelope 仍为 v1，wire protocol major/minor 仍为 `1/0`。

错误码全集：

| code | retryable | 语义 |
|------|-----------|------|
| `invalid_frame` | false | framing/UTF-8 失败；通常直接断开 |
| `invalid_request` | false | closed schema、ID 或参数失败 |
| `unsupported_protocol` | false | protocol major/minor 不支持 |
| `unsupported_binary` | false | client/server binary major 不同 |
| `unknown_method` | false | method 未注册或注册在另一 socket |
| `unauthorized` | false | credential kind/value/binding 错误 |
| `not_found` | false | 对象不存在或不向该 credential 暴露 |
| `stale` | false | generation/version/attempt 已过期 |
| `conflict` | false | 当前事实与请求不可同时成立 |
| `not_ready` | true | 仅 Report 在合法 `spawning` 窗口可用 |
| `in_progress` | true | 受控终止/probe 已在执行 |
| `internal` | true | 未分类内部失败；只返回 correlation id |

## 4. Credential

所有 capability 由其合法持有方使用 `crypto/rand` 生成 32 bytes，以 64 位小写十六进制传输；数据库只存 SHA-256 hash。credential kind 不能互换。`auth` 是以下 closed tagged union：

| kind | 字段 | 允许方法 |
|------|------|----------|
| `operator` | `token` | `ops.*` |
| `run_token` | `token` | `report.submit` |
| `bootstrap` | `nonce` | `claim.acquire` |
| `wrapper_session` | `session` | `claim.permit_spawn` |
| `wrapper_started` | `session`, `permit` | `claim.started` |

任何变体多字段、少字段或用于错误方法均返回 `unauthorized`；服务端不得先尝试多个 credential kind 再“碰撞成功”。

### 4.1 Operator token

`operator.token` 内容为 `64 lowercase hex + LF`。首次启动文件不存在时，daemon 以临时文件 `0600`、fsync、rename 原子创建；已存在时要求 regular file、当前 UID 所有、模式不宽于 `0600`，否则拒启。启动期读取一次并留在内存；V0 不热轮换，轮换需停止 daemon、删除文件并重启。

所有到达 `siftd.sock` 的请求——包括认证失败、未知方法与 schema 失败——都追加低敏安全事件；事件只记 peer UID、request id、method（若可安全解出）、结果与时间，不记 auth。

### 4.2 Run token 与 bootstrap nonce

launch worker 认领 operation 后生成 run token、bootstrap nonce 与 dispatch id，并先经 `PrepareLaunchDispatch` CAS 持久化三者绑定关系及 token/nonce hash，提交后才原子写 `bootstrap.json` 和 spawn wrapper。run token 绑定 `(run_id, attempt_no, generation)`，只授权 `report.submit`；bootstrap nonce 额外绑定 dispatch id，只授权一次 `claim.acquire`。

明文只存在 launch worker 内存与 bootstrap/control 文件。准备事务提交后若在写文件前崩溃，恢复确认无 wrapper/control 后递增 generation、作废旧 dispatch/hash 并重新准备；不得尝试从 hash 恢复明文。bootstrap 已落盘时，新 lease owner校验文件 digest 与 claim 后复用同一 dispatch，不另生成 token。

wrapper 读 `bootstrap.json` 后立即 unlink，但在 acquire 确认前把 nonce/run token 留在内存。run token 最终写入 `control.json` 给 Agent；attempt 非 `running` 时 Report 通常拒绝，唯一可重试例外见 §5.2。

### 4.3 Wrapper session 与 spawn permit

wrapper 在 acquire 前生成随机 session candidate；daemon 以 bootstrap nonce 验证请求，CAS 绑定 `(run_id, attempt_no, generation, wrapper_instance_id)` 并把 candidate hash 作为已签发 session。相同 instance + candidate 重放幂等确认，不同 instance 或换 candidate 拒绝。

wrapper 在 permit 请求前生成随机 permit candidate；daemon 以 session 验证请求，只在 `starting → spawning` CAS 成功时保存 candidate hash并授权它。相同 session + candidate + control digest 重放幂等确认，换 candidate 拒绝。

session/permit 明文只存在 wrapper 内存和对应 RPC request 生命周期，数据库只存 hash，不写 `control.json`、argv、环境变量或日志。客户端生成 candidate 不等于自授权；线性化点仍是 daemon CAS。`claim.started` 必须同时出示已授权的 session 与 permit。

### 4.4 Launch operation lease 与 dispatch

`launch_agent` 沿用 [`outbox.md` §4](outbox.md) 的 lease envelope。`ClaimOutboxOperation` 产生的 `(operation_id, outbox_attempt_id, lease_owner, lease_expires_at_ms)` 是 daemon 内部的 expected lease；这些字段和 lease owner **不下发 wrapper**，也不进入 bootstrap。只有持有 expected lease 的 worker 可调用 `PrepareLaunchDispatch`，该事务同时校验 operation key 对应 `(run_id, attempt_no, generation)`、operation 仍为 `executing`、lease owner 未被替换，并一次性写入 dispatch id、nonce/token hash。事务提交前不得写 bootstrap 或调用 backend。

提交后固定顺序为：原子写 bootstrap → 回填文件 digest 执行证据 → backend 只启动 wrapper。每个外部步骤前 worker 都重新确认 expected lease；确认失败的旧 worker 立即停止。lease 到期本身不证明 wrapper 未启动，也不授权直接生成新 dispatch：daemon 恢复必须先扫描 attempt、bootstrap/control 与进程事实，再按 §4.2 选择复用同一 dispatch，或在证明旧 wrapper 尚未取得 spawn 能力后递增 generation。恢复完成前禁止 claim/reclaim launch operation。

`claim.acquire` 是 dispatch 的收口线性化点：`AcquireLaunchClaim` 在一个事务中验证当前 claim/dispatch、绑定 session、推进 `pending → starting`，并把对应 launch outbox attempt/result/operation 标为 succeeded。acquire 与 worker completion 不得各自提交两套“launch succeeded”结果。lease 在 RPC 到达前刚过期不是单独的拒绝理由；只要恢复尚未作废该 dispatch 且所有 binding 仍匹配，daemon 以当前事实完成上述事务。若恢复已递增 generation 或替换 dispatch，则返回 `stale`。

wrapper 从不持有 operation lease，也从不写 SQLite。它唯一允许的持久写入是本 attempt run dir 中 §7 的原子文件；operation、claim、session、permit、attempt/Run 阶段与事件全部只能由 daemon 的受限存储端口写入。

## 5. `run.sock` 方法

### 5.1 方法授权矩阵

| method | auth | 合法阶段 | 可产生的存储效果 |
|--------|------|----------|------------------|
| `report.submit` | run token | `running`；`spawning` 仅返回 not_ready | rate bucket、receipt、event、可选 Interrupt |
| `claim.acquire` | bootstrap nonce | `pending` | `AcquireLaunchClaim`：pending→starting + launch operation succeeded/result + event |
| `claim.permit_spawn` | wrapper session | `starting` | wrapper identity 校验、`starting → spawning`、permit hash + event |
| `claim.started` | wrapper session + permit | `spawning` 或 startup_stall 仲裁态 | Agent identity、attempt/Run transition、事件与可选 Interrupt 关闭 |

服务端先按 method 选择所需 auth schema，再校验 binding；不得提供“已认证后可调用任意 run.sock 方法”的通用 session。

### 5.2 `report.submit`

params：

```json
{
  "run_id": "...",
  "attempt_no": 1,
  "generation": 1,
  "report_key": "...",
  "kind": "progress",
  "payload": {}
}
```

- `kind = progress | goal | blocker | completed`；payload schema 后续由 `specs/report.md` 定义。
- token binding、attempt 与 generation 必须一致。
- `running`：进入 `RecordReport`；相同 report key + digest 返回 `duplicate`，同 key 异 digest 返回 `conflict`。
- `spawning` 且 permit 已签发、started 尚未提交：返回 `not_ready`，details 只含按 config 限定的 `retry_after_ms`。
- pending/starting/finished/orphaned、过期 generation、跨 Run token：永久拒绝，不返回 `not_ready`。
- `completed` 只写事件，不修改 Run 状态。

成功 disposition 为 `accepted | duplicate`。

### 5.3 `claim.acquire`

params 必含 run/attempt/generation/dispatch、`wrapper_instance_id` 与 wrapper identity：

```json
{
  "run_id": "...",
  "attempt_no": 1,
  "generation": 1,
  "dispatch_id": "...",
  "wrapper_instance_id": "...",
  "session_candidate": "<64 lowercase hex>",
  "wrapper_identity": {
    "pid": 123,
    "started_at_ms": 0,
    "executable": "/absolute/path/sift-agent-wrapper",
    "pgid": 123
  }
}
```

服务端验证 nonce hash、generation、dispatch、operation lease 派发事实与进程身份。首次成功 CAS 绑定 instance并保存 session candidate hash，响应为 `{"disposition":"acquired"}`；同 instance + 完全相同请求返回相同结果，不新增事件。换 candidate 或竞争 instance 拒绝。失败不启动 Agent，wrapper 必须退出。

`wrapper_identity` 必须与 socket peer 可观测进程及已解析 wrapper executable 一致；`pid>0`、`started_at_ms>0`、`pgid>0`，且 pid 必须属于 pgid。`wrapper_instance_id` 与 `dispatch_id` 为 32 位小写十六进制随机 ID。params 中没有 operation lease 字段；伪造 lease owner 不存在可接受入口。

### 5.4 `claim.permit_spawn`

params 为 run/attempt/generation、wrapper instance、wrapper identity、`control_digest`、`control_nonce_hash` 与 `permit_candidate`；auth 出示 session。`control_nonce_hash` 是 control.json 内 nonce 的 SHA-256 小写十六进制，daemon 持久化它供受控终止时与文件重验。完整 schema：

```json
{
  "run_id": "...",
  "attempt_no": 1,
  "generation": 1,
  "wrapper_instance_id": "...",
  "wrapper_identity": {
    "pid": 123,
    "started_at_ms": 1,
    "executable": "/absolute/path/sift-agent-wrapper",
    "pgid": 123
  },
  "control_digest": "<64 lowercase hex>",
  "control_nonce_hash": "<64 lowercase hex>",
  "permit_candidate": "<64 lowercase hex>"
}
```

daemon 读取并校验 `control.json`：wrapper identity、run token hash、worktree 与 claim 一致，Agent identity 为空；文件 schema/owner/mode/digest 任一不符均拒绝。成功 CAS `starting → spawning` 并保存 permit candidate hash，响应为 `{"disposition":"permitted"}`。同 session/candidate/digest 的重放返回相同结果，不新增事件、不再次推进阶段；任何字段漂移或换 candidate 为 `conflict`。

### 5.5 `claim.started`

params 为 run/attempt/generation、wrapper instance、Agent identity、`control_digest`，并同时出示 session + permit：

```json
{
  "run_id": "...",
  "attempt_no": 1,
  "generation": 1,
  "wrapper_instance_id": "...",
  "agent_identity": {
    "pid": 456,
    "started_at_ms": 0,
    "executable": "/absolute/path/agent"
  },
  "control_digest": "<sha256>",
  "result_digest": null
}
```

通常必须验证 Agent 身份仍存活；若已极快退出，可用身份一致且原子落盘的 `result.json` digest 证明曾启动。handler 必须调用统一 `ResolveAttemptRace`，不得另写一套迟到事实逻辑：

- 无 `attempt_resolution`：事实优先，推进 attempt/Run；startup_stall Interrupt 存在时以 `superseded_by_fact` 关闭；
- 已有 `reject | retry_after_absence`：记录迟到身份并返回 `superseded_by_decision`，不推进旧 Run；
- 完全相同的 started 重放返回既有 disposition，不重复事件或 transition。

成功 disposition 为 `running | finished_observed | superseded_by_fact | superseded_by_decision | duplicate`。

### 5.6 Wrapper spawn guard 与确定性拒绝

wrapper 的本地状态机必须与 daemon 阶段分开实现：

```text
bootstrap_read → acquired → control_written → permit_wait
permit_wait --permitted--> spawn_ready --consume one-shot--> spawn_entered
spawn_entered --spawn ok + control rewrite--> started_wait → supervising
spawn_entered --spawn error----------------> exit (由 daemon 恢复，禁止重试 spawn)
```

permit RPC 超时或响应丢失时，只能在 `permit_wait` 以**完全相同**的 session、candidate 与 params 重试；新 RPC envelope 使用新的 request_id。收到首个 `permitted` 后，wrapper 在调用 spawn adapter **之前**原子消费进程内 one-shot guard；从 `spawn_entered` 起，迟到/重复 permit response 一律忽略，不能回到 `permit_wait`，spawn 失败也不得用同 permit 再试。wrapper 崩溃后的新进程没有该 session；daemon 依靠已持久化 `spawning` 与受控终止/absence 证据收敛，而不是让新 wrapper 复用 permit。

鉴权 gateway 和三个 handler 必须按下表给出可断言结果；所有拒绝均为零阶段推进、零 capability 写入、零 spawn 调用，只追加 §9 允许的安全事件：

| 用例 | 结果 |
|------|------|
| run token 调任一 `claim.*`；bootstrap 调 permit/started；session 调 acquire/started；缺 permit 的 session 调 started；permit/session 交换字段 | `unauthorized` |
| credential secret 不匹配其已选 kind，或跨 run/attempt binding | `unauthorized` |
| 已认证 credential 携旧 generation、旧 dispatch 或已被替换的 attempt | `stale` |
| 同 bootstrap 下竞争 `wrapper_instance_id`；既有 instance 换 session candidate | `conflict` |
| permit 重放时换 candidate、control digest、wrapper identity 或 instance | `conflict` |
| started 的 session 与 permit 不属于同一 claim/generation，或 Agent/control/result 证据不一致 | `unauthorized`（凭据组合错）或 `conflict`（已认证后的证据错） |
| 方法与 auth 合法但阶段尚未到达或已越过，且不是规范定义的幂等重放 | `conflict` |
| 任一 wrapper 请求未知/缺失字段、错误类型、非 canonical ID/hash/identity | `invalid_request` |
| 任一 wrapper 请求协议或 binary major 不兼容 | `unsupported_protocol` / `unsupported_binary`，优先于 auth/params |

V10a wrapper 段至少逐行覆盖上表，并额外断言：同 acquire 请求事件数不变；同 permit 请求的 daemon transition 数为 1；permit 响应重放/延迟交错下 spawn adapter 调用数始终为 1；同 started 请求不重复 transition/event；竞争 instance、跨 generation 与各 credential kind 交叉均无控制文件外的副作用。

## 6. `siftd.sock` 方法

### 6.1 方法全集

| method | 类型 | 结果 |
|--------|------|------|
| `ops.ps` | read | Run/attempt/Interrupt/outbox/预算摘要 |
| `ops.logs` | read | 日志文件定位及有界读取结果 |
| `ops.worktree` | read | Run worktree 路径与隔离状态 |
| `ops.doctor` | read | 健康项、warning/error、`unsafe-local` 暴露面 |
| `ops.kill` | write | 立即收敛或 accepted/in_progress |
| `ops.retry` | write | 立即收敛或 accepted/in_progress |

所有方法要求 operator token。read 方法不得返回 token/hash、nonce/session/permit、完整 config snapshot、Brain raw output或控制文件内容。

### 6.2 读方法 schema

`ops.ps` params：

```json
{"run_id":null,"project_id":null,"status":null,"limit":100,"after_run_id":null}
```

所有字段必填但可按上例为 null；`status` 非空时使用 Run 状态枚举，`limit=1..1000`。result：

```json
{
  "runs": [{
    "run_id":"...","project_id":"...","status":"running","version":3,
    "attempt":{"attempt_no":1,"generation":1,"phase":"running","isolation_state":"none","heartbeat_at_ms":0},
    "open_interrupt_count":0,"pending_outbox_count":0,"updated_at_ms":0
  }],
  "next_after_run_id": null,
  "attention_remaining":{"low":3,"normal":5,"high":5}
}
```

无当前 attempt 时 `attempt=null`；列表按 `run_id` 升序做 keyset pagination。不得返回 token/hash或自然语言 Task Spec。

`ops.logs` params：

```json
{"run_id":"...","attempt_no":null,"offset":0,"limit":262144}
```

attempt 为空时选择最大 attempt_no；`offset>=0`，`limit=1..262144`。result 为 `{"attempt_no":1,"offset":0,"next_offset":123,"eof":false,"data_base64":"..."}`。offset 是该 attempt 逻辑日志流的字节偏移，轮转文件按旧到新拼接；已清理导致 offset 不可达时返回 `not_found`，不得偷偷从当前文件开头返回。原始字节用 RFC 4648 standard base64，CLI 解码后负责转义控制字符。

`ops.worktree` params 为 `{"run_id":"..."}`；result 为：

```json
{"run_id":"...","attempt_no":1,"path":"/absolute/path","exists":true,"isolation_state":"none","read_only_recommended":false}
```

无 attempt/worktree 返回 `not_found`；本方法只返回路径，不创建 shell、不修改目录。

`ops.doctor` params 必须是 `{}`。result：

```json
{
  "offline": false,
  "exit_code": 1,
  "security_posture": "unsafe-local",
  "checks": [{"id":"operator-token-readable-by-agent","level":"warning","message":"...","details":{}}]
}
```

`exit_code=0|1|2` 与 [`config.md`](config.md) §7 一致；`level=ok|warning|error`，checks 按 id 升序。details 是按 check id 绑定的 closed schema，不得成为任意 JSON 逃生口。

### 6.3 `ops.kill` / `ops.retry`

params：

```json
{"run_id":"...","expected_version":3,"request_key":"..."}
```

`request_key` 对同一 operator 请求幂等；CAS 失败返回 `stale`。若执行者可能存活，命令只启动/复用受控终止流程并返回：

```json
{"disposition":"accepted","probe_id":"...","message":"waiting for executor absence evidence"}
```

不得回“已终止”。确认执行体消失后：kill 终结 attempt 且 Run→failed，不创建 attempt；retry 终结当前 attempt并按策略创建新 attempt。确认不了则冻结并发唯一 `startup_stall` Interrupt。重复请求返回同一 probe/结果，不并发发信号。

Linux 以 `/proc/<pid>/stat`、`/proc/<pid>/exe` 与 owner-only `control.json` 独立重建 PID、启动时间、可执行路径、进程组和 control nonce hash；任一证据缺失、格式错误或不匹配均不得发信号。Darwin 当前没有等价的 native inspector，统一返回身份未知并走同一冻结路径，直到补齐 `proc_pidinfo` 实现；不得以 `kill(pid, 0)` 或 PID 单独作为替代证明。

同步完成 result 为 `{"disposition":"completed","run_id":"...","run_version":4,"status":"failed","new_attempt_no":null}`；retry 创建 attempt 时 `new_attempt_no` 非空。异步受理使用上例 `accepted`；已有同 request key 返回原 disposition。除 `completed | accepted` 外无成功 disposition。

## 7. Wrapper 文件契约

所有 JSON 文件使用 closed schema 和 canonical JSON；写入协议统一为：同目录随机临时文件 0600 → 写全 → fsync file → rename → 必要时 fsync directory。读取方拒绝 symlink、非 regular file、owner 不符、模式宽于 0600、超出 frame 上限或 digest 不符。

### 7.1 `bootstrap.json` v2

由 launch outbox worker 在 `PrepareLaunchDispatch` 提交后创建，wrapper 无论版本/内容校验成功与否都在安全读取后立即 unlink：

```json
{
  "schema_version": 2,
  "protocol_major": 1,
  "protocol_minor": 0,
  "daemon_version": "0.1.0",
  "wrapper_version": "0.1.0",
  "run_id": "...",
  "attempt_no": 1,
  "generation": 1,
  "dispatch_id": "...",
  "bootstrap_nonce": "...",
  "run_token": "...",
  "run_dir": "/absolute/path",
  "worktree_path": "/absolute/path",
  "agent": {
    "id": "...",
    "executable": "/absolute/path",
    "args": [],
    "task_transport": "stdin"
  },
  "task_spec_snapshot_id": "...",
  "task_spec": {}
}
```

bootstrap 的 operation/claim/payload digest 必须一致。文件写成后将整文件 digest 回填到 operation 执行证据；新 lease owner只可复用与当前 claim/dispatch/digest 一致的文件。wrapper 只接受 argv 中的 bootstrap **路径**，不接受 credential argv；读取后即使 acquire 失败也不得恢复该文件。

### 7.2 `task.json` v1

wrapper 从 bootstrap 中取 Task Spec，先写 `task.json` 再请求 permit。内容：

```json
{"schema_version":1,"task_spec_snapshot_id":"...","task_spec":{}}
```

- `stdin` transport：Agent argv 为配置 argv，wrapper 把 canonical `task_spec` JSON 写入 Agent stdin；task.json 仍保留作恢复/审计输入。
- `file` transport：wrapper 不写 task stdin，把配置 argv 中唯一、完整的 `{task_file}` token 替换为 `task.json` 绝对路径；不做子串插值或 shell 展开。
- 两种 transport 都只向 Agent 环境注入 `SIFT_RUN_DIR=<run_dir>`；不继承 daemon 的 credential 环境。
- 文件在 attempt 清理前保留；重试的新 attempt 使用自己的 snapshot 和文件，不覆盖历史 attempt 输入。

### 7.3 `control.json` v1

wrapper 首次在 permit 前写 wrapper 部分，spawn 后原子改写为含 Agent identity 的完整版本：

```json
{
  "schema_version": 1,
  "run_id": "...",
  "attempt_no": 1,
  "generation": 1,
  "wrapper_instance_id": "...",
  "wrapper_identity": {"pid":123,"started_at_ms":0,"executable":"...","pgid":123},
  "agent_identity": null,
  "worktree_path": "...",
  "task_spec_snapshot_id": "...",
  "control_nonce": "...",
  "run_token": "...",
  "updated_at_ms": 0
}
```

Agent identity 非空时必须是完整 pid/started/executable 三元组。每次写后以整文件 SHA-256 作为 `control_digest`；session/permit 不写入本文件。该文件存活到 attempt 现场清理，隔离期不得删除。

### 7.4 `heartbeat` v1

```json
{"schema_version":1,"run_id":"...","attempt_no":1,"generation":1,"wrapper_instance_id":"...","observed_at_ms":0}
```

wrapper 按 config 周期原子替换。heartbeat 只是活性线索，不是 Agent started、完成或执行体消失证据。

### 7.5 `result.json` v1

```json
{
  "schema_version": 1,
  "run_id": "...",
  "attempt_no": 1,
  "generation": 1,
  "wrapper_instance_id": "...",
  "agent_identity": {"pid":456,"started_at_ms":0,"executable":"..."},
  "exit_code": 0,
  "signal": null,
  "finished_at_ms": 0,
  "final_head_sha": "...",
  "control_digest": "..."
}
```

`exit_code` 与 `signal` 必须恰有一个非空。`final_head_sha` 必须为 40 或 64 位小写十六进制并由 wrapper 在 Agent 退出后读取；读取失败则成功结果无效并按 contract violation 收敛。`result.json` 只证明进程结果；不能由 wrapper/Agent 直接推进 Run 或创建 Change。

### 7.6 `agent.log`

wrapper 原样追加 Agent PTY 字节流并按 config 轮转。日志不是 JSON、不得作为完成证据；控制字符由 CLI 展示层转义，RPC 不返回终端控制序列。

## 8. CLI 行为

- `sift report` 只读 `$SIFT_RUN_DIR/control.json` 的 run token并连接 `run.sock`；缺目录、文件不安全或 token 不合法时本地失败，不回退运维 socket。
- `sift ps/logs/worktree/doctor/kill/retry` 只连接 `siftd.sock` 并读 operator token；不得把 operator token发送到 `run.sock`。
- daemon 不可用时，写命令一律失败且不改 DB/文件。V0 只有显式 `sift doctor --offline` 可走离线只读诊断；输出必须含 `offline:true`，不得迁移、创建 token、清理 socket或修正权限。
- `sift report` 对 `not_ready` 使用 config 的指数退避上限；其他 auth/stale/conflict 错误不重试。

## 9. 安全审计

至少记录：socket、peer UID、request id、method、credential kind、绑定对象（若认证后可得）、disposition、error code、recorded time。以下必须是安全事件：错误 socket/method、未知 credential kind、auth 失败、跨 Run/attempt、旧 generation、竞争 wrapper、session/permit 不符、控制文件 digest 漂移、超限 frame、operator 写命令及离线写尝试。

事件 payload 对 token/nonce/session/permit 只允许写固定字符串 `redacted`；不得写 hash 供离线撞库关联。认证前的大量失败允许按低基数聚合计数，但不得静默丢掉首次与熔断事件。

## 10. M1 基线与 M3 handoff 验收

1. V10a：无/错误 operator token 的全部 ops 请求被拒；run token 不能调用 claim 或 ops；bootstrap/session/permit 不能调用 Report；wrapper 三段的 credential/instance/generation 交叉拒绝与零副作用逐行满足 §5.6，permit 响应重放时 spawn adapter 调用计数仍为 1。
2. V10b：同 UID Agent 在 V0 可显式读取 operator token并成功调用 ops，同时 `doctor` 必须报告 `unsafe-local`，不得伪称隔离闭合。
3. protocol major/minor、未知字段/方法/枚举、超限 frame 全部 fail closed；错误不泄露 credential。
4. 两 socket 均为 0600 Unix socket，第二 daemon 被 lock 拒绝；测试进程无 TCP/UDP listener。
5. dispatch 准备事务前后、bootstrap rename 前后与 wrapper spawn 前后逐点崩溃均收敛为复用同 dispatch 或递增 generation 重发，不双起；acquire/permit/started 响应丢失重放幂等。竞争 wrapper、旧 generation、错 session/permit 永久拒绝并记事件。
6. Run 只有 Agent identity + started 验证后进入 running；极快退出可由身份一致 result 补证；迟到事实遵守 `attempt_resolution` 仲裁。
7. `spawning` 的 kill/retry 只返回 accepted，absence 未确认前不换 owner；kill 永不创建新 attempt。
8. Report 在合法 spawning 窗口只返回有界 not_ready；过期/cross-run 永久拒绝；completed 不改状态。
9. bootstrap 0600 且读后 unlink；控制文件原子写，损坏/宽权限/symlink 均拒绝；session/permit 不落控制文件。
10. stdin/file 两种 Task transport 使用同一冻结 snapshot；新 attempt 不覆盖旧 attempt 输入。
11. daemon 不可用时只有 `doctor --offline` 可读；所有离线写路径不存在。
12. darwin/linux × arm64/amd64 构建；peer credential 与进程身份平台实现均有测试替身。

## 11. 评审冻结项

1. `task_transport=file` 只做完整 `{task_file}` token 替换，不形成 shell 插值面。
2. 单连接单请求与 1 MiB frame 足够；大日志必须 offset/limit 分块，不放宽 frame。
3. operator token 只由在线 daemon 首启创建；offline doctor 不创建文件。
4. dispatch prepare + bootstrap 文件证据 + wrapper candidate 是 V0 唯一启动 credential 重放协议。

## 12. 自查结果

- [x] 双 socket 的 method 集合与 credential kind 无交叉授权。
- [x] run token 只能 Report，completed 不写 Run 状态；wrapper 启动只经 operation lease + 三段 handoff，wrapper 不写 DB。
- [x] wrapper/daemon 版本握手字段完整；bootstrap v1→v2 的破坏性变更已显式标注。
- [x] permit 重放与 spawn one-shot 的状态边界明确；V10a wrapper 拒绝矩阵可直接派生测试。
- [x] launch dispatch 崩溃窗口可由 generation/文件证据收敛；session/permit candidate 重放不需在 SQLite 保存明文。
- [x] `spawning` 的 kill/retry 如实返回 accepted，absence 证据前不换 owner。
- [x] bootstrap/task/control/heartbeat/result/log 均有路径、权限、schema 或字节语义及生命周期。
- [x] 每 attempt 独立 run dir，stdin/file transport 均引用冻结 Task Spec。
- [x] offline 路径只有 doctor 只读诊断；无离线写库、迁移或 token 创建。
- [x] 相对链接存在、代码围栏闭合、无尾随空白。

**自查结论：** 字段级契约完整，评审通过，允许转 `active`。
