FAIL

# interrupt.md 二次字段级评审

> 日期：2026-07-29
> 评审人：pi × GPT-5.6-sol
> 评审对象：[`docs/specs/interrupt.md`](../specs/interrupt.md)（#97 修订后 draft）
> 首轮报告：[`2026-07-29-interrupt-review-pi-gpt-5.6-sol.md`](2026-07-29-interrupt-review-pi-gpt-5.6-sol.md)
> 依据：PRD §4.1–§4.4/§5.5/§7.1、DESIGN §8.7/§10.1、WBS M3 §3.6–§3.7、[ADR-010](../decisions/010-attempt-spawn-handoff.md)、[ADR-013](../decisions/013-startup-stall-retry-convergence.md)
> 交叉核对：active [`storage.md`](../specs/storage.md) §5–§6/§12.2/§12.5、[`config.md`](../specs/config.md) §3.9、[`outbox.md`](../specs/outbox.md) §2/§5

## 1. 结论

**FAIL。** #97 已补齐七种 reason 的完整 options、brief/links 基本规则、M3 comment kind/key、severity 边界公式和 manual Run 的链接分类；I3–I5 已核销，I1/I2/I6 仍有字段级阻断。当前正文仍允许两个实现生成不同字节的 brief/generation key，golden vectors 与 severity 算法直接冲突，且 manual Run 的发布目标没有 active 存储契约承载。`docs/specs/interrupt.md` 继续保持 `status: draft`。

本结论只评审规格，不表示 M3 Runtime、Interrupt 发射器或 Command 已实现。

## 2. 剩余阻断

| 项 | 对应首轮项 | 级别 | 发现 | 可关闭的修改项 |
|----|------------|------|------|----------------|
| R1 | I1 | P1 | §3.2 先要求把 CRLF/CR 规范成 LF，随后又要求拒绝“其余控制字符”并“不得……换行”。LF 本身是控制字符；这里既可实现为接受规范化后的 LF，也可实现为拒绝所有换行，无法得到唯一 brief 字节。 | 明确 LF 的唯一结局：若字段值必须单行，直接规定 CR/LF 全部拒绝；若允许换行，明确 LF 是控制字符拒绝规则的唯一例外，并删除“不得换行”或说明它只禁止 renderer 额外插入换行。补 CRLF、裸 CR、裸 LF 三个逐字节/拒绝 vectors。 |
| R2 | I1 | P1 | §3.5 称七个 vectors 固定 `severity=normal`，但 §4.2 明定 `guardrail_violation`、`merge_conflict`、`failure_review`、`startup_stall` 的 base 是 `high`；JSON 又完全省略 `severity`。§7 却把它们称为“golden object”并要求逐字节相同。实现/测试可分别以 `normal`、`high` 或“不在 vector 范围内”为准。 | 将 vectors 明确限定为 renderer 子对象并删除 severity 声明，或补全对象并按 §4.2 给四种 reason 写 `high`；同步 §7 的“golden object”措辞。 |
| R3 | I2 | P1 | §5 的 typed preimage 仍未形成唯一字节协议。通用语法要求每段为 `<type:name>\x00<value>\x00`，但 `startup_stall` 行写成单个 `enum:cause=startup_stall`，未说明它是字段名还是已内嵌值。`sha256:head_sha` 也未冻结 digest 编码（原始 32 bytes、大小写 hex 等），且仓库中的 forge `head_sha` 是 Git object ID，并不保证是 SHA-256。不同合规实现会散列不同 preimage，或拒绝同一合法 SHA-1 仓库的 head。 | 把 cause 写成明确的 `enum:cause` + 值 `startup_stall`；逐类型冻结值编码。Git head 使用能承载仓库 object format 的类型/规范文本编码，真正 SHA-256 digest 则规定小写 64 位 hex（或另一种唯一编码），并补完整 preimage/最终 digest vectors。 |
| R4 | I6 | P1 | §3.4 为 manual Run 引入“创建时冻结的已验证 discussion target”，但 active `storage.md` §5.2 的 `runs` 没有该字段，仓库其余 active 契约也没有 discussion target 的来源、验证或冻结位置。manual Run 又明确没有 Issue；在 Change 创建前，`design_approval` 等 Interrupt 没有可恢复的 comment 目标。这与 WBS M3“每个 reason 都能……可发布”及 §1.3 五件事同事务不相容，崩溃恢复也无法重建相同 payload。 | 在权威存储/创建契约中定义 manual discussion target 的字段、验证和冻结时点，并让 outbox payload 从该冻结值产生；或明确 manual Run 使用已有且已持久化的 forge 对象作为目标。补“manual、无 Issue、尚无 Change”的成功发布 vector，而不是只写拒发边界。 |

此外，§3.4 只冻结了 Interrupt 的 `brief` 和 links 顺序，没有给 forge comment 的完整 markdown（headline/options/links 如何组合）字节模板。若 M3 的“确定性 renderer”意在跨实现同字节，还应一并补完整 comment golden vector；至少不得把仅覆盖 `brief_markdown` 的 §3.2 称为完整发布 renderer。

## 3. I1–I6 对账

| 首轮项 | 结论 | 对账 |
|--------|------|------|
| I1 | **NO** | options 四字段、事实顺序、links 排序/去重和逐 reason vectors 已补；Escape 的换行规则与 severity golden 声明仍不唯一（R1/R2）。 |
| I2 | **NO** | domain/version/reason 与 `startup_stall` 诊断不拆键已对齐 DESIGN；typed field/value 语法及 hash/Git object ID 编码仍未冻结（R3）。 |
| I3 | **YES** | M3 首发已唯一冻结为 `forge_comment` / `purpose=interrupt` / `subject_id=interrupt_id` / `generation=1` / `comment:interrupt:<id>:1`，M5 Channel key 已分离。 |
| I4 | **YES** | M3 `BaseSeverity` 与 M5 boolean `suggested_downgrade` 已进入同一算法边界，调用方不能指定最终 severity。 |
| I5 | **YES** | `min(escalation_count,max_escalations)`、max=0、恰达/超过上限及 critical 饱和均已唯一。 |
| I6 | **NO** | 最低/可选链接和合法空数组已分类；但 manual Run 的可发布目标没有权威字段，最低链接合法不等于 comment 可发布（R4）。 |

## 4. 已确认对齐

- 七种 reason 的 headline、min_modality 与完整互斥 options 已冻结；`startup_stall` 不含 approve。
- `startup_stall` 固定生成身份为 `(run_id, attempt_no, generation, cause=startup_stall)`，诊断分类不拆条；DESIGN 已同步。
- `startup_stall` 禁止 `auto_reject`、升级封顶后 hold，且事实优先/隔离/终态不释放 worktree 与 ADR-010/013、storage 一致。
- M3/M5 切片仍准确：M3 是 fallback + forge comment；T4/T6、Channel、tick/升级、critical 熔断和 Command 执行不冒充已实现。

## 5. 关闭检查清单

- [x] 二次字段级评审 `docs/specs/interrupt.md`
- [x] 对照首轮报告与 #97 修订后正文
- [x] 对照 PRD/DESIGN/WBS、ADR-010/013 与 active storage/config/outbox 契约
- [x] 报告写入规定路径且首行为 `FAIL`
- [ ] I1–I6 全部核销（**NO：I1/I2/I6 仍有 R1–R4 阻断**）
- [ ] `interrupt.md` 转 `status: active`（**NO：结论为 FAIL**）
- [x] 未修改实现代码
- [x] 未自行创建实现 Issue
