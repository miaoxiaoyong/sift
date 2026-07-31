# M6 Runtime 字段复审 — Issue #843

独立评审者：Pi × Claude Sonnet（只读审查，未编辑文件）。评审基线为 #843 未提交文档；正文修订后的 root backend、Runtime/PTY/tmux/attach/qualification 及 V2/V4 inventory 均纳入复审。

## 复审结论：**PASS WITH NOTES**

**阻断项（Blockers）：无。**

> 透明性声明（必须在归档前知悉）：N1–N11 的原始账本不在本次被检视的未提交文件中（应为前序字段评审会话产物，`docs/reviews/` 无对应存档）。因此我**按主题验证关闭质量**而非按编号字面核销——核对了 diff 引入的修复是否真正闭合、当前文本是否自洽、跨文件（config ↔ runtime ↔ storage ↔ control-plane ↔ matrix）是否一致。若你的 N 账本编号与下述主题映射有出入，请在归档前确认映射；但**正文无残留实质性缺口**，映射差异属行政性，不影响门禁。

---

## 归档报告

### A. 逐主题关闭验证（diff 可见修复 + runtime.md 当前文本）

| 主题（重建映射） | 当前落点 | 关闭判定 |
|---|---|---|
| **Effective backend 选择与冻结** | config.md:135,139（默认改 `runtime.backend` + 规范化段）、runtime.md §2.1:24-32（`agent.backend ?? runtime.backend`、冻结 snapshot、retry/restart 不重读、resolved path vs 配置拼写）、storage.md backend 约束（attempt backend 来自冻结 snapshot） | ✅ 关闭。根 `runtime.backend` 已在 config.md:214 定义，无悬空引用；config/runtime/storage 三处口径一致 |
| **PTY 拓扑不变量**（setsid/Setctty 破链） | runtime.md §3.1:53-63（禁止 `Setsid/Setpgid/Setctty`、5 条生产拓扑断言、80×24 backend-neutral 常量、改常量同步 `method_version`） | ✅ 关闭。对齐 M6 plan 风险 #1 |
| **tmux session identity / 响应丢失 reclaim** | runtime.md §4.1-4.3:78-102（exact `=<name>`、禁前缀匹配、3 条件 reclaim、禁 kill/rename/attach 同名、响应丢失只收敛同 binding） | ✅ 关闭 |
| **Backend session 观测不对称** | runtime.md §5:104-124（present+身份不成立 / 身份成立+absent 两条不对称，observation 不入 Gate/claim/resolution） | ✅ 关闭 |
| **`ops.attach` 只读** | control-plane.md:346/395-403/547（table 行 + params + result schema + conflict 映射 + CLI）、runtime.md §6:128-154（closed params、deterministic fail-closed、`attach-session -r`、零领域写入） | ✅ 关闭。absent/unknown/mismatch 统一投影为非重试 `conflict`，且"不得误报为 Run not_found"——control-plane 与 runtime §8 口径一致 |
| **资格 key + 不可变 store + fail-closed 门控** | runtime.md §7:156-186（exact key、verified 仅 topology harness 全生命周期、detached/unknown fail closed、unverified 单 Interrupt 路径）、storage.md `topology_qualification_key` 列 + 绑定约束 + 新增 §5.6 `agent_topology_qualifications`（immutable、trigger 禁 UPDATE/DELETE、`UNIQUE(key,evidence_digest)` 收敛、`status=verified iff reason=qualified` CHECK、门控查询 fail closed） | ✅ 关闭。store 列投影与 runtime key input 自洽；attempt 仅引用 key 不以 FK 阻断合法 unverified；auto absence/retry 须 key 非空且门控 verified，与 matrix R57/X16 一致 |

跨文件一致性、markdown 相对链接、frontmatter `draft` 状态均与 #843 后置条件（"每条 DESIGN 行有唯一归属"+"独立字段评审后 active"）兼容。

### B. N4 决策（tmux 多 argv 可行性）—— **可安全冻结为实现期 capability/test 要求，非字段级阻断**

依据：
1. **runtime.md §4.2:90** 把多 argv 写成硬性能力要求："`tmux 版本必须支持 shell-command [argument ...] 的多 argv 形式`"，并附测试要求"包含空格或 shell metacharacter 的测试路径必须逐字节到达 wrapper"。
2. **config.md:525** 把它下沉为启动探测门禁："`runtime.backend=tmux` 或任一 effective backend 为 tmux 时"探测，要求 `tmux -V >=3.2` 且支持多 argv 形态；**process-only 配置不探测**。
3. 因此可行性问题被消解为：能力具备则放行、能力缺失则启动期 fail closed、不使用 tmux 则零成本不触及。字段侧只需声明"argv 数组直传、禁 shell 拼接"（§4.2 已声明），这正是正确姿态。

→ **N4 判定：safely frozen。** 不构成 draft→active 阻断。

### C. 非阻断注记（impl 期，归档备查）

- **N4 探测机制**：`tmux -V >=3.2` 是版本串校验，并不直接证明多 argv 能力。#845 的探测应**功能性验证**（在 throwaway server 上以多 argv `new-session` 实跑并断言字节级到达），而非仅 `tmux -V`。3.2 是偏保守的下限，能力实际存在更早；安全网是功能性探测 + process-only skip，不是版本号本身。
- **method_version / 80×24 耦合**：runtime.md §3.1 称改终端常量需同步 `method_version`，而 key（§7.1）以 `runtime-topology/v1` 为 V0 粗粒度 tag、不直接含终端尺寸。设计自洽，但实现需保证改常量确实 bump method_version（否则同 key 下输入环境已变）。仅记录，不阻断。
- **N5–N8 未在请求中列出**：按主题综合复审未发现对应这些编号的残留开放项；但因无账本，无法逐字确认其关闭状态——建议归档时一并带过。

---

## draft→active 明确结论

**规格可由 draft 移至 active。** 理由：独立字段复审无阻断项；所有 diff 修复主题在当前文本中真正闭合（非仅"已添加"）；跨文件一致、无悬空引用、链接闭合；#843 后置条件（独立字段评审通过 + 每 DESIGN 行唯一归属）满足。唯一前置：确认本报告主题映射与你的 N1–N11 账本一致（行政性，非实质性）——确认后即可 active，并进入 #844。

注：本次仅做静态字段复审；runtime.md 为 untracked 新文件故无法看其 diff 增量，相关主题按"当前文本正确性"判定。真实双后端/PTY/tmux/资格证据仍按 matrix 留 #844–#852，M6 综合门禁由 #852 独立复审定。
