# command.md 字段级评审

> 日期：2026-07-29
> 评审人：pi × GPT-5.6-sol
> 评审对象：[`docs/specs/command.md`](../specs/command.md) draft（#287 / PR #296）
> 依据：PRD §4.1–§4.5/§5.5/§7.1/§9.2、DESIGN §6.2/§6.4/§8.8/§10.1、ADR-013、WBS M5 §5.4，以及 active 的 config/forge/ledger/outbox/storage 规格和当前 interrupt draft

## 1. 结论

**FAIL（9×P1）。** 候选稿正确守住了 actor fail-closed、严格确定性解析、当前 Interrupt + nonce + options 三重约束、Ledger 因果绑定、Task Spec 不覆盖历史，以及 `startup_stall` retry 请求/结果分离和迟到事实仲裁等关键原则；但事件入口、幂等身份、标签防重放、动作效果、存储事务和回执 schema 仍有不可实现或相互矛盾的字段契约。

[`command.md`](../specs/command.md) 必须保持 `draft`。本报告不授权实现方自行选择缺失的事件身份、target 绑定、reason 后继或 ack 映射；关闭下列 P1 后再做定向复审。

## 2. P1 阻断

| 项 | 发现 | 为什么阻断 | 可执行关闭条件 |
|---|---|---|---|
| C1 | **Forge 边界无法产出 `ForgeCommandEvent`。** active [`forge.md`](../specs/forge.md) 的 `LabelEvent` 没有 label-event ID，而 §2.1 要用该 ID 写 receipt；同一 active 契约还规定评论/标签缺 actor 时在适配器内直接丢弃，但 §2.2 又要求 Command 写 `ignored_missing_actor` receipt 和安全事件。 | 标签命令没有可持久化幂等身份；缺 actor 分支在 Command 层永远不可达。双平台适配器即使完全遵守 forge spec，也无法遵守 command spec。 | 同步版本化 `forge.md`：为标签事件提供稳定远端 event ID，并明确 target/source；在“适配器丢弃”与“Command 持久化 ignored receipt”之间只保留一个 owner。若保留后者，actor 必须是边界上的显式可空字段。补 GitHub/GitLab label/comment ID、缺 actor 和分页重放契约 fixture。 |
| C2 | **幂等身份的作用域与全局 operation key 冲突。** §2.1 明说 `forge_event_id` 只在项目内稳定，receipt 唯一键也是 `(project_id, forge_event_id)`；§6 和 active outbox 却使用全库唯一的 `command:<forge_event_id>:ack`。评论 ID、label-event ID 还可能来自不同远端命名空间。 | 两项目或两事件流出现相同远端 ID 时，会命中同一 ack operation：轻则拒绝第二条合法回执，重则复用另一项目的 immutable payload/target。 | 冻结 collision-safe 的 canonical command event key，至少覆盖 project + source/event-kind + remote ID；receipt、event idempotency key、probe 关联和 ack operation key 全部使用同一身份。同步 `outbox.md`、`storage.md` 及 operation-key builder，并给跨项目/跨事件流同 ID fixture。 |
| C3 | **“closed `DomainCommand`”不足以提交其承诺的事务，也没有可查询的 immutable target binding。** union 缺 `project_id`、`target`、`event_kind`、`raw_digest` 等 `forge_event_receipts` 必填输入；语法/target 拒绝又根本不会形成该 executable union。与此同时 §1/§2.3 要精确匹配 Interrupt 冻结的 forge target，但 `interrupts` 没有 target 列，本文也未指定以哪条 immutable outbox/delivery 关系作为唯一来源。 | 存储端口无法仅凭规定输入原子写 receipt/event/ack；不同实现可按 Run 当前 Issue/Change、首次 comment payload 或最新 delivery 选出不同 target，直接削弱防错投与防重放边界。 | 将输入拆成 closed `CommandEventEnvelopeV1` + source-tagged payload + 可选 executable action，并逐字段冻结 required/null/长度/交叉约束；覆盖 receipt 所需全部字段。为 Interrupt 冻结唯一 command target：新增不可变字段，或明确绑定初次 `forge_comment` operation 并以真实 FK/唯一约束取其 immutable target；禁止从当前 Run/Forge 状态重建。 |
| C4 | **严格语法仍不精确，且 `hold_max_duration` 没有事实源。** 规范代码块把 `reject/retry/hold/ask` 写成不同数量的空格，而正文又要求逐字节匹配、参数间单个 ASCII space；实现无法判断这些空格是对齐还是语法。`hold` 还依赖“创建时冻结的 `hold_max_duration`”，但 active config、interrupt 和 storage 均无该字段/列。 | 两个严格解析器会接受不同字节；hold 上限无法读取、冻结或回放，重启/配置漂移后也无法保持同一判断。 | 用 ABNF、等价逐字节 grammar 或规范 parser vectors 冻结每个分隔符、EOF、reject 可选 reason、ask 多行及 Go-duration 接受集；移除对齐空格歧义。为 hold 上限选择唯一配置/常量来源；若可配置，则在 config 定义默认/范围并在 Interrupt 创建时持久化冻结值，补边界 vectors。 |
| C5 | **审批标签名与 active 配置矛盾。** 本文把唯一标签硬编码为 `sift:approved`，而 [`config.md` §3.14](../specs/config.md#314-labels) 将 `labels.approved` 定义为可配置字段，只是默认值为该字符串。 | 用户合法修改配置后，Command 与配置投影会对“哪一个标签可执行”得出不同结论；硬编码默认值也会让非默认审批标签静默失效。 | 明确 Command 只接受启动期有效配置快照中的 `labels.approved`，并冻结其规范化、项目/平台作用域及重启生效语义；或从 config 删除可配置性并做迁移。增加默认值、非默认值及运行期配置漂移 fixture。 |
| C6 | **标签 anti-replay 比较了未证明同一时钟域的时间。** §2.5 以 forge label event 的 `observed_at_ms` 比较本地 SQLite 中 `nonce_issued_at_ms`。forge spec 未定义 `ObservedAt` 是远端发生时间还是本地摄入时间，也未给时钟偏差/精度保证；前者与 daemon 时钟不可直接比较，后者会把历史回扫事件误判为新事件。 | 时钟偏差或秒级时间截断可让旧批准越过新 nonce，或让人在新 Interrupt 后添加的合法标签被拒；这是审批防重放边界，不是诊断误差。 | 改用两平台均可证明的同序域 issuance cutoff（例如可持久化的远端单调事件位置/发布证据），并在 nonce 初签与每次轮换时冻结；定义严格前后关系、同位置、游标回读、崩溃恢复和平台无能力时 fail-closed。若只能使用时间，必须先在 forge 契约证明同一远端时钟锚点与精度边界，不能直接比较 daemon 本地时间。 |
| C7 | **非 `startup_stall` 的 reason-specific 动作仍被委托给未定义的 “reason owner”。** §3.2 只给“恢复确定性后继/重试或重算”的描述，§3.2 末段也只举例；没有穷尽七个 reason × canonical options 的 Run 转移、Interrupt 开闭/nonce、attempt 所有权、一次性 soft exemption、Gate 重新评估、merge/check operation、Task Spec 或失败原因。 | WBS M5 §5.4 要求实现 reason-specific 确定性效果；当前实现方仍必须自行决定 `design_approval approve` 是回 `queued` 还是建 launch、`code_review approve` 如何绑定 head、各类 retry 是否建 attempt/operation。A1 无法靠“owner 自己明确”兑现。 | 在 command spec 或被唯一链接的 active owner spec 中给出穷尽动作矩阵：每格冻结前置 binding、Run transition、Interrupt lifecycle、attempt/Task Spec/Gate/exemption 变更、outbox kind/key、Ledger/calibration 结果及 stale 行为。矩阵必须覆盖 interrupt canonical options 的每个组合，未列组合一律拒绝。 |
| C8 | **人类决定与状态效果的事务 owner 重叠。** §3.1 说普通动作先调用 `ApplyInterruptCommand`，再由它调用 `RecordHumanDecision`；active [`ledger.md` §3](../specs/ledger.md#3-shadow-decision因果关系与唯一写入口) 和 [`storage.md` §11–§12.6](../specs/storage.md) 又把 `RecordHumanDecision` 定义为会写 Ledger、Run、Interrupt、Task Spec、认证和 outbox 的唯一应用/事务入口。`ResolveAttemptRace` 对 startup reject 也被要求直接追加 Ledger。 | 这些公开写端口各自持有事务且调用方拿不到 `*sql.Tx`；互相调用会拆事务或嵌套事务，平行实现则会双写 Ledger/认证或形成两个动作 owner，违背本规格自己的单事务要求。 | 选择每类命令唯一的 public transaction port，并把 Ledger append/settlement、transition 和 race arbitration 下沉为同一事务内的私有原语；同步 command/ledger/storage 的端口表和伪代码。证明普通动作、startup reject、retry request/result 都只开一次事务且只写一次 HumanDecision。 |
| C9 | **持久 outcome 与 ack 没有可实现的 closed schema/mapping，且 nonce 公开规则自相矛盾。** §2.2 依赖 `command.accepted/rejected` event 的 closed disposition 做重放，但本文未定义 event payload；`interrupt_not_current` 等内部结果也没有到 `CommandAckV1` 九个 disposition 的穷尽映射。异步 retry 在“受理”还是“最终 probe outcome”创建唯一 ack 的时点不精确。最后，§1.8 禁止 nonce 出现在公开回执，§6 又要求 `next_nonce` 渲染新的可执行命令。 | 重放无法仅从持久事实恢复同一结果；不同实现会对同一拒绝发不同 disposition，或为 retry 建立即/最终两条 ack；renderer 对新 nonce 既必须输出又必须禁止输出。 | 冻结 `CommandEventV1`、`CommandAckV1` 和 source-specific command outcome 的 closed schema（required/null/大小/canonical/unknown-field），给每个解析、target、option、CAS、probe/race 分支的唯一 event type + ack disposition + next_nonce 映射；明确 retry pending/final 的单 operation 创建时点及崩溃重放。将敏感文本规则改为明确允许仅输出“新签发的当前 nonce”，同时禁止回显提交的旧 nonce，并给逐字节 renderer fixtures。 |

## 3. 已确认正确的边界

- **A1/鉴权方向正确。** 评论与标签共用 allowlist、缺 actor fail closed、非可信输入静默，且不会把 allowlist 当 capability secret 或绕过 CAS/options。
- **当前对象约束正确。** Run + target + 唯一 open Interrupt + nonce/version + options 的组合，及“零个/多个都拒绝”，能避免按最新评论或最新 Interrupt 猜测。
- **Ledger/Task Spec 原则正确。** calibration 只从 immutable binding 解析；retry/hold/ask 不伪造二元样本；ask 新建 snapshot，reject/ask 原文保留但不进入公开 ack。
- **`startup_stall` 主体正确。** retry 请求不关 Interrupt、不写 resolution、不解隔离；成功结果遵循 ADR-013 单事务；reject 保持 frozen；迟到事实由同一 race 原语吸收。
- **外部 IO 边界正确。** Command 不在数据库事务内调用 Forge、Brain、进程检查或信号操作；ack 继续走 outbox marker 收敛。

这些原则通过不抵消 C1–C9 的字段/事务阻断。

## 4. 交叉规格处置范围

- C1/C2/C6 必须同步 [`forge.md`](../specs/forge.md)、[`outbox.md`](../specs/outbox.md) 与 [`storage.md`](../specs/storage.md)；只在 command 私下拼接 ID 或比较时间不能关闭。
- C3/C8/C9 必须同步 storage 的 receipt、Interrupt target binding、写端口归属和崩溃事务；不能新增一个拿裸 `*sql.Tx` 的 Command 旁路。
- C4/C5 涉及 active [`config.md`](../specs/config.md) 及当前 [`interrupt.md`](../specs/interrupt.md) draft；相邻字段评审应复用同一冻结值，不复制第二份默认。
- C7 必须与 active gate/ledger 和 Runtime 的 attempt 所有权对齐；不能把未冻结行为留给实现代码成为新的事实源。

## 5. 定向复审验收

复审至少应能逐项回答并给 fixture/事务向量：

1. 两平台每条 comment/label command 的 canonical event key 是什么，缺 actor 在哪一层留下何种持久事实；
2. 同 remote ID 跨项目/跨事件流不会碰 receipt/event/ack key；
3. 每个可信候选（含语法失败）如何由一个 closed envelope 原子落 receipt、event 与至多一个 ack；
4. 标签事件如何证明晚于当前 nonce issuance，且不依赖 daemon/forge 时钟碰巧同步；
5. 七种 reason 的每个 option 只有一个确定性后继；
6. normal command、startup reject、retry request/result 各只有一个事务 owner；
7. 所有拒绝/竞态/异步结果均有唯一 event/ack disposition，旧 nonce 不回显而新 nonce 可执行；
8. crash injection 在 receipt、Ledger、Run/Interrupt、probe、Task Spec、launch/ack outbox 和 event 的每个写点均只能全有或全无。

## 6. 验收判断

- `command.md` 转 `active`：**NO**
- P1 数量：**9**
- 评审报告入库：**YES**
- 允许开始 Command 实现：**NO；可先实现不冻结私有字段的 parser/transaction test harness，但不得将缺失映射写成事实标准**
