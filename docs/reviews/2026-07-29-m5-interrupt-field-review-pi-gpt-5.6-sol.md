FAIL

# M5 interrupt.md 字段级评审

> 日期：2026-07-29
> 评审人：pi × GPT-5.6-sol
> 评审对象：[`docs/specs/interrupt.md`](../specs/interrupt.md)（#286 / PR #292 的 M5 扩展 draft）
> 依据：PRD §4.1–§4.4、§5.3、§5.5、§7.1/§7.3；DESIGN §6.4、§8.7、§9.2；WBS M5 §5.1–§5.3
> 交叉核对：[`brain.md`](../specs/brain.md) §11–§12、[`config.md`](../specs/config.md) §3.9、[`storage.md`](../specs/storage.md) §6/§9/§11–§12、[`outbox.md`](../specs/outbox.md) §2/§5/§10、[`command.md`](../specs/command.md) §2–§3

## 1. 结论

**FAIL。** M3 已冻结的七种 reason、generation key、forge comment 首发、`startup_stall` 隔离与五件事事务没有被本次扩展破坏；M5 草案也守住了单一 `EmitInterrupt`、不新增 reason 旁路、T4/T6 不写状态和升级不重复收费等正确边界。

但 M5 新增部分尚未形成可实现的字段闭环：T4/T6 与 `brain.md` 使用不同 closed contract；Channel 配置和能力集合不存在；升级后的下一到期时点与上限去向没有可恢复权威；配额拒绝和“升级后首次进入 critical”的审计模型无法由 active storage 表达；摘要批次又被正文明确留待 storage/outbox 后补。两个实现会据此生成不同 payload、在重启后得到不同结局，部分合法路径甚至无法持久化。因此 `docs/specs/interrupt.md` 必须保持 `status: draft`。

本结论只评审字段契约，不表示 M5 实现已开始或 M3 已有实现回退。

## 2. 可执行 P1

| 项 | 发现 | 可关闭的修改项 |
|---|---|---|
| I1 · T4 与 renderer 契约不唯一 | interrupt §7.1 允许 T4 提交 `headline`、`brief` 和 option 文案，却要求 option 顺序与 canonical 完全相同、禁止重排；brain §11.2 实际输出 `headline/conclusion/key_points/recommended_option_id/options[]`，没有 `brief` 或 option 文案，并明确允许 `options[]` 重排。interrupt 也没有冻结 conclusion/key-points 到 `brief_markdown` 的逐字节模板、Markdown 转义及“未冻结事实”可判定规则。与此同时 command §2.4 要求每个 Interrupt renderer 显示带 run_id/nonce 的完整命令，现有 forge/Channel renderer 均未承接。fallback 的 `recommended_action` 也未约束为当前 canonical option ID。 | 在 interrupt/brain/command 中选定一份 closed T4 接口和一份展示顺序语义；冻结 T4 输出到持久 `headline/brief/options` 的逐字节 renderer、escaping/拒绝规则及完整命令渲染。明确 `recommended_action` 必须命中 canonical option，并补合法 T4、重排、Markdown/marker/动作语法、fallback 与当前 nonce 的 vectors。 |
| I2 · T6、Channel 与确定性裁决无法对接 | interrupt §7.2 使用 `dispatch=immediate\|batch\|defer`，brain §12.2 使用 `delivery=immediate\|batch\|next_window`；interrupt 要求 high/critical 立即，brain 只禁止 critical 延后。brain 输入依赖 `fallback_immediate_min_severity`、`channel_candidates`、`default_channel_id`，但 config 顶层没有 Channel registry、capability/default/renderer 配置，也没有该 severity 阈值；零兼容 Channel 时 brain 又要求 candidates 至少一项，与 interrupt 的 held 路径冲突。PRD §12 #7 仍标记 Channel 实现开放。 | 先关闭 V0 Channel 选择，给 config 增加唯一的 Channel ID、类型、modality capabilities、默认选择和必要目标配置；统一 T6 enum。冻结裁决顺序（建议校验 → 最终 severity → quota/fuse → dispatch）、high/critical 规则、next-window/defer 语义，以及零候选/隔离 Channel 时不调用 T6并进入 held 的 deterministic vector。 |
| I3 · 升级/hold 缺少可恢复时钟与冻结字段 | interrupt §8.2 在到期升级时只递增 count、重算 severity、轮换 nonce 并重推，没有规定新的 `expires_at`；旧时间仍已到期，下一 tick 可立即连续升级到上限。config §3.9 要求每条 Interrupt 冻结 `expires_after/on_expire/on_max_escalations`，storage §6.1 却只存 `expires_at/on_expire/max_escalations`，没有 `expires_after` 或 `on_max_escalations`。`held` 同时表示自动 hold、无 Channel、defer 等不同原因，也没有下一调度时点。升级重算 `Severity(..., suggested_downgrade)` 时是否继续使用初发 T6 建议同样未定义。 | 冻结并持久化（或以不可变快照字段明确可恢复地引用）`expires_after`、`on_max_escalations`、最终 Channel/dispatch 与必要的 downgrade 决定；规定初发、显式 hold、自动 hold、每次升级后的 `expires_at/next_dispatch_at` 公式和 held reason。为 `AdvanceInterrupt` 补完整 CAS 配方，覆盖重启、旧 tick、max=0、首次/最后一次升级和 T6 建议在升级时的唯一处理。 |
| I4 · 配额拒绝与 critical 转入没有可表达的记账事实 | interrupt/config 要求非 critical 配额不足时仍创建原 Interrupt 并合批，但 storage §12.2 要求先“扣一次预算并取得 entry id”，`interrupts.charged_budget_entry_id` 又是 NOT NULL；`budget_entries.amount` 必须为正，故既不能无借支地创建，也不能区分“收费成功”和“配额拒绝后合批”。另一方向，high Interrupt 升级后首次成为 critical 必须占用滑动窗口；config 把它写成同一 `EmitInterrupt` 事务，实际推进端口却是 `AdvanceInterrupt`，且 storage §9.3 只按初发 critical charge entry 计数、升级又禁止新增 charge。 | 定义一份 append-only attention admission/fuse 账本或等价的 typed entry，分别表达首次收费成功、配额拒绝合批、初发 critical admission、升级首次进入 critical，且不把后两者误算为重复 charge。同步 `charged_budget_entry_id` 的含义/约束、Emit/Advance 的事务归属和滑动窗口查询，补并发额度耗尽、high→critical、重复 tick和窗口边界 vectors。 |
| I5 · 合批/critical 汇总仍是未定义协议 | interrupt §8.1 明说批次身份、成员冻结、payload 和 operation key 要在 storage/outbox 后补；当前 storage 只有单 Interrupt 的 `dispatch_state/delivery`，outbox §10 也只接受 `{interrupt_id, escalation_no, priority, rendered_text}` 和 `interrupt:<id>:publish:<n>`。它无法表示每日摘要或 critical 汇总的批次 ID、scope/window、成员版本/nonce、调度时间、关闭成员排除、每 scope 至多一次、全局与 per-Run 同时熔断时的归属，以及 summary 的稳定 operation key。正文同时要求摘要成员分别可回复，却未冻结摘要 payload/renderer。 | 在 storage/outbox 的字段权威中补齐 batch 与 immutable membership 模型、daily/critical batch key、scope/window/时点、成员版本与发送前 open-CAS、全局/per-Run 重叠优先级、payload tagged union、可见 marker及逐成员完整命令 renderer；再由 interrupt 只引用该唯一协议。补并发入批、成员发送前关闭、响应丢失重放、窗口恢复和摘要不可批量执行 vectors。 |

## 3. 已确认未回退

- 七种 PRD reason、`min_modality`、canonical fallback options 与 M3 golden renderer 子对象仍在；`startup_stall` 仍只有 `retry/reject/hold`。
- generation preimage 仍包含 domain/version/reason；`startup_stall` 固定为 `(run_id, attempt_no, generation, cause=startup_stall)`，诊断分类不拆条。
- M3 首发仍是 `forge_comment` / `comment:interrupt:<interrupt_id>:1`；单条 Channel key 仍与 forge comment key 分离。
- `startup_stall` 禁止 `auto_reject`，上限只能 hold，且 retry/迟到事实继续归 ADR-013/`ResolveAttemptRace`。
- T4/T6/Channel 失败不得关闭 Interrupt、退款、改写 reason/options 效果或创建第二发射入口。

## 4. 复审入口

I1–I5 全部关闭后，至少以以下组合复审：T4 normal/fallback 的最终 markdown 与 options 顺序；T6 三种调度及零 Channel；配额耗尽但 Interrupt 成功入批；high 升级首次进入 critical；重启后的 expiry/on-max；daily 与 global/per-Run critical 汇总并发；摘要前成员关闭；Channel 响应丢失重放；`startup_stall` 封顶仍为 open + hold + 无 resolution。

## 5. 关闭检查清单

- [x] 对照 PRD/DESIGN/WBS M5，且未另建 reason 旁路
- [x] 交叉核对 brain/config/storage/outbox/command 的字段权威
- [x] 报告写入指定路径，首行结论为 `FAIL`
- [x] 列出可执行 P1 与复审 vectors
- [ ] `docs/specs/interrupt.md` 升为 `active`（**NO：I1–I5 未关闭**）
