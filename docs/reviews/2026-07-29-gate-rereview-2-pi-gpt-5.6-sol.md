# gate.md G1/G2/G4 二次定向复审

> 日期：2026-07-29
> 评审人：pi × GPT-5.6-sol
> 评审对象：[`docs/specs/gate.md`](../specs/gate.md)
> 基线：[`2026-07-29-gate-rereview-pi-gpt-5.6-sol.md`](2026-07-29-gate-rereview-pi-gpt-5.6-sol.md) 的残余 G1、G2、G4
> 闭合提交：PR #236（提交 `c694654`，对应工作项 #235）

## 1. 结论

**PASS（0×P1）。** 首次定向复审残留的 G1、G2、G4 均已关闭；连同此前已关闭的 G3、G5、G6，首次字段级评审的 G1–G6 现为 **6/6 CLOSED**。

[`gate.md`](../specs/gate.md) 已由 `draft` 转为 `active`，可作为 M4 Gate 实现与测试基线。本次同时删除了 §7 中“closed schema 尚未冻结”的过期措辞；这只是状态与正文一致性修正，不改变已审定契约。schema、事务及崩溃窗口仍须在 M4 实现中按 §7 验收，`active` 不等于实现门禁已通过。

## 2. 残余 P1 关闭对账

| 项 | 结论 | 关闭证据与判断 |
|---|---|---|
| G1 Gate input closed schema | **CLOSED** | [`gate.md` §2.2](../specs/gate.md#22-gateinputv1-闭合形态g1) 现将 `paths_complete=true` 固定为 `GateInputV1` 的唯一合法值；空 `changed_paths` 只表示完整读取后确无路径。`paths_complete=false` 在纯函数、snapshot、evaluation、cache 和后继动作之前唯一返回 `GateInputAssemblyErrorV1{code:"paths_incomplete",field:"change.changed_paths"}`，并被指定为 invalid fixture；valid fixture 只能含 `true`。同一对象不再同时是合法输入与 snapshot 前置错误，fail-closed 入口唯一。 |
| G2 exact verdict union | **CLOSED** | [`gate.md` §2.3](../specs/gate.md#23-verdictv1-闭合并集g2) 新增 closed 分支 `failed/change_not_open`，payload 唯一携带 `change_state=closed|merged`；§3 将 Change state 提到全部 guardrail、Checks、review 和 merge 之前。两个状态各有必备 canonical fixture，不能再被实现为等待 Checks、`input_unknown` 或继续合并。active [`ledger.md` §3](../specs/ledger.md#3-shadow-decision因果关系与唯一写入口) 也已把该 verdict 穷尽映射为 `block`，没有校准缺口。 |
| G4 flaky rerun side effect | **CLOSED** | [`storage.md` §8.5](../specs/storage.md#85-outbox_attempt_request_starts不可变rerun_checks-专用) 增加只供 `rerun_checks` 使用、不可更新/删除的 request-start 表；`MarkOutboxAttemptRequestStarted` 以 operation、当前未过期 lease 和 attempt 做同事务 CAS，提交后才能调用远端。特殊 reclaim 明确二分：无 request-start 的过期 attempt 写 retry result 后可新建 attempt；已有 request-start 的 attempt 原子转 `conflict`、创建/去重 `failure_review`，不得新建 attempt。`CompleteOutboxAttempt` 对 started rerun 只接受 success/conflict，不接受 retry。[`outbox.md` §4、§8](../specs/outbox.md) 与 [`gate.md` §3.2](../specs/gate.md#32-flaky-checks-rerun-副作用g4) 同步该边界，因而调用前崩溃可恢复，调用中、响应丢失或完成提交前崩溃均不会产生第二次远端 rerun。 |

## 3. 交叉一致性复核

- **Gate/Storage/Outbox：通过。** operation key、payload、retry consumption、request-start、claim/reclaim 和 complete outcome 使用同一 head/check/retry 身份；通用 reclaim 明确排除 `rerun_checks`。
- **Gate/Forge：通过。** [`forge.md` §4.12](../specs/forge.md#412-getchecks) 的 `RerunCheck` 以稳定 check ID 和 expected head 绑定目标；无法验证、目标歧义或能力不足均 fail closed，不降级调用。
- **Gate/Ledger：通过。** 新 verdict 已进入 shadow decision 的穷尽映射；每次合法 Gate evaluation 仍有 calibration。路径不完整发生在 Gate assembly contract 边界，明确不伪造 evaluation/calibration。
- **既有 G3/G5/G6：未回退。** protected-path matcher/effective policy 仍引用同一 active policy shape；guardrail generation identity 和三值校准映射未被 #236 放宽。

## 4. 实现期验收边界

本轮审定的是字段与事务契约，不把未来实现证据提前写成已通过。M4 至少仍须以生成 schema/fixtures 和崩溃注入证明：

1. `paths_complete=false` 只产生 assembly error，且不落 snapshot/evaluation/cache/outbox；
2. `closed`、`merged` 各自在其他判定前唯一产生 `failed/change_not_open`；
3. `rerun_checks` 在 request-start 前、提交后调用中、远端返回后 complete 前三个崩溃窗口分别收敛为“可新 attempt / conflict / conflict”，后两者不再调用远端；
4. request-start、attempt result、operation terminal state 与唯一 `failure_review` 的 CAS/回滚满足全有或全无。

这些是 active 规格的派生实现门禁，不是继续保持 draft 的字段级 P1。

## 5. 验证

- `git diff --check`：**通过**。
- 本次变更文档的相对 Markdown target 检查：**通过**。
- `go test ./...`：首次运行仅失败于既有 `TestDoctorBaselineChecksConfiguredDependencies` fixture 的 `signal: killed` 时序 flake；其余包通过。
- `go test -count=10 ./internal/controlplane -run '^TestDoctorBaselineChecksConfiguredDependencies$'`：**通过**。本轮仅修改文档，不把该既有 flake 记为 Gate 契约回退。

## 6. 验收判断

- G1/G2/G4：**3/3 CLOSED**
- 首次评审 G1–G6：**6/6 CLOSED**
- P1 数量：**0**
- `gate.md` 转 `active`：**YES**
- 允许开始 Gate 实现：**YES；实现与 §7 验收尚未据本报告宣称完成**
