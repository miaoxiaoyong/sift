# config.md M5 增补字段级评审

> 日期：2026-07-29
> 评审人：pi × GPT-5.6-sol
> 评审对象：[`docs/specs/config.md`](../specs/config.md) 的 M5 增补（PR #295 / commit `9b1c83e`）
> 依据：PRD §4.2、§5.3、§5.5、§10.2，DESIGN §8.7、§9.2、§12 V8/V13，WBS M5 §5.2–§5.3/§5.7，以及 `interrupt.md`、`storage.md`、`outbox.md`、`ledger.md` 的相邻契约

## 1. 结论

**FAIL（4×P1）。**

本次增补的字段方向正确：`daily_quota`、`critical_fuse`、逐 reason 的 expiry/封顶去向和 `attention_weight_minutes` 均给出了 closed/default/range/freeze 约束；`startup_stall` 的 fail-closed 特例和历史权重不按当前配置重算也与上游要求一致。

但候选稿仍把普通 CAS 竞争误判为额度耗尽，且“非 critical 升级后首次成为 critical”没有可执行的熔断记账路径；每日摘要的时区/调度边界及 critical 汇总的持久身份也尚未冻结。相同事实可因并发顺序、触发时刻或实现自行选择而得到不同投递结果，V8/V13 无法据此写成确定性测试。因此 **M5 增补不得据本报告视为已通过字段评审**。

`config.md` 的顶层 `status: active` 早于本次 M5 增补，承载已实现的 M1–M4 配置基线；本次不把整份文档降为 `draft`，也不把既有 active 状态解释为 M5 增补已获批准。关闭以下 P1 并复审通过后，才可补记 M5 评审处置。

## 2. P1 阻断

| ID | 发现 | 为什么阻断 | 可执行关闭条件 |
|---|---|---|---|
| MC1 | **把 `budget_counters` 的 CAS 失败等同于额度耗尽。** config §3.9 规定“额度为零或 CAS 失败”即进入合批；storage §9.1 的 version CAS 失败也可能只是另一个并发发射先提交，并不证明重读后已无额度。 | `limit=2`、两个并发候选时，第二个候选可能仅因 stale version 被合批，尽管仍有一格；投递结果取决于事务交错，不再是配额事实的确定性函数。把存储错误也吞成合批还可能留下“看似正常降级”的错误领域事实。 | 将语义改为：CAS 竞争必须在同一稳定生成键下重读并有界重试；只有权威 counter 证明 `consumed + 1 > limit` 才合批。不可恢复的存储/事务错误整笔回滚，不得冒充 quota exhausted。补 `limit=2` 双并发均成功、`limit=1` 双并发恰一条收费/一条合批及故障回滚 vectors。 |
| MC2 | **升级后首次进入 critical 的熔断路径与现有字段权威矛盾。** config §3.9 要求初发 critical 和升级成 critical 都在“同一 `EmitInterrupt` 事务”计数；但 `EmitInterrupt` 只创建对象，升级由 `AdvanceInterrupt` 推进。storage §6.1/§9.3 又规定 `charged_budget_entry_id` 唯一、升级复用原 charge、不新增 entry，而熔断只查询首次发射时 `scope_id=critical` 的 entry。 | 初发为 `high`、升级为 `critical` 的 Interrupt 不会进入 critical 滑动窗口；反之若重新调用 `EmitInterrupt`，会违反唯一生成键和“升级不新建 Interrupt/charge”。critical 可由升级路径绕过全局/per-Run fuse，直接击穿 PRD §5.5。 | 在 storage 中冻结与 attention charge 分离的 append-only **critical admission evidence**（或等价 closed 结构），以 `interrupt_id` 唯一；初发 critical 由 `EmitInterrupt`、升级首次成为 critical 由 `AdvanceInterrupt` 在各自 CAS 事务内执行同一全局/per-Run滑动窗口检查并至多写一次 admission。重放/重推不重复 admission，升级仍不新增 attention charge。同步 config/interrupt/storage，并补初发、升级、并发撞线与窗口恢复 vectors。 |
| MC3 | **`day_timezone=local` 和 `daily_summary_at` 没有唯一的规范化/调度语义。** config §3.9 说 quota day 与摘要按“发射时冻结的 day_timezone”计算，却未说明 `local` 是否在有效配置 canonical JSON 中解析为稳定 IANA zone；同时“归入当日合批，最多在该日的 daily_summary_at 发一条”在候选于当天该时刻之后产生时不可执行。DST 的不存在/重复本地时刻也没有裁决。 | 同一 `config_hash` 可在机器时区变化后代表不同日桶；09:00 后超额的对象可被实现解释为立即发、次日发或永不发。历史回放与配额/摘要查询无法稳定复现。 | 明确 `local` 在启动规范化阶段解析为可持久化的具体 zone 身份并进入有效 config snapshot/hash（无法稳定解析则拒绝并要求显式 IANA zone）；摘要 `due_at` 定义为入批时刻之后的下一次本地 `daily_summary_at`，并冻结 DST gap/fold 的选择规则。给跨 midnight、入批恰在/晚于时刻、spring-forward/fall-back vectors。 |
| MC4 | **“唯一汇总 HITL”没有稳定批次身份、成员或 operation 契约。** config §3.9 要求非 critical 每日摘要及 critical 熔断汇总；但 storage 只有单 Interrupt delivery/charge，outbox §10 只有单 `interrupt_id` 的 `channel_publish`。`interrupt.md` §8.1 也明确 M5 实现前必须补摘要 batch 字段。对真实滑动窗口而言，“该窗口”还是每个候选各自移动的区间，不能直接作为唯一 key；全局和 per-Run 同时命中时也未规定合一还是两批。 | 无法构造稳定 operation key、崩溃重放、成员去重或 `AttentionDeliveryV1.batch_id`；关闭成员能否在发送前移除、一次汇总能否批量执行、同一 critical 是否进入两个汇总均由实现猜测。V8/V13 的“每日一次/熔断一次”没有可断言对象。 | 在 storage/outbox/interrupt 冻结 versioned batch 与 member schema：batch kind/scope、稳定 `batch_id`/operation key、`due_at_ms`、成员唯一键与冻结 payload、关闭成员排除、发送后不可变证据及逐 Interrupt nonce/options 绑定。定义 global/per-Run fuse 重叠时的唯一归属与滑动 fuse episode 的起止/复用规则；`AttentionDeliveryV1.batch_id` 必须能指向该真实对象。补响应丢失重放、并发入批、成员关闭和双 scope 命中 vectors。 |

## 3. 已通过的字段核对

- **逐 reason 配置：** 七种 reason 的 closed map、字段默认、duration 范围和创建时冻结方向可实现；`startup_stall` 在 schema/runtime 双重拒绝 `auto_reject`，且封顶不写 resolution，符合 ADR-013。
- **配额字段：** `daily_quota` 不接受 `critical`，全局/per-Run fuse 的范围及 `per_run_limit <= total_limit` 已给出；“重放、重推不重复收费/不退款”方向正确。MC1/MC2 关闭前，运行期扣费与 admission 仍不可据此冻结。
- **指标权重：** 七项 closed map、有限 number、`0..1440`、默认值和 Run `config_snapshot_id` 历史冻结已足够生成配置 schema。实现查询应按 `attention_charge_entry_id` 去重，并消费真实成功 delivery/batch member，不得按 delivery 次数重复加权。
- **上限结局：** `max_escalations=0` 可由现有文字解释为首次到期即执行封顶去向；非 `startup_stall` 的 `hold|auto_reject` 映射没有新增产品安全绕过。

## 4. 非阻断注记

1. PRD §12 #4/#11/#12/#14 仍标“开放”，而 config 已给出可运行默认值。按 DESIGN §14.2/WBS H11，这可解释为“默认已定、仍允许经证据校准”；后续编辑时宜把 PRD 状态改成该精确表述，避免把默认值误读成尚未决定。
2. `on_max_escalations` 在 `on_expire != escalate` 时不会被消费，但仍进入 config hash。实现 schema 应保留当前 closed/default 行为；若未来选择拒绝无效组合或规范化掉无效值，属于配置语义变化，需版本化，不能静默改变 hash。

## 5. 复审门槛

- [ ] MC1：CAS contention 与 quota exhausted 分离，并有并发/回滚 vectors
- [ ] MC2：初发与升级首次 critical 共用唯一 admission 规则，且 admission 与 charge 分离
- [ ] MC3：`local` 规范化、下一摘要时刻和 DST 规则冻结
- [ ] MC4：摘要/critical 汇总 batch、member、operation 与 Ledger 身份闭合
- [ ] config/interrupt/storage/outbox/ledger 交叉术语与链接一致
- [ ] `git diff --check`、Markdown 链接/围栏检查通过

## 6. 验收判断

- 遗留 P1：**4**
- M5 增补字段评审：**FAIL**
- `config.md` M5 增补升 active：**NO**
- `config.md` 既有 M1–M4 active 基线：**不回退**
- 允许按当前文本实现 M5 配额/熔断/汇总：**NO**
