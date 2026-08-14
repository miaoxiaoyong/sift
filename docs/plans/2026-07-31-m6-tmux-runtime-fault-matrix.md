---
status: active
created: 2026-07-31
summary: M6 双后端 Runtime 收口顺序
---

# M6：tmux 与完整故障矩阵

跟踪 Issue：[#842](https://github.com/xsift/sift/issues/842)。判定基线为 [WBS M6](../WBS.md#m6tmux-与完整故障矩阵)、[DESIGN §8.4](../DESIGN.md#84-runtime)、[§10.1](../DESIGN.md#101-启动期恢复矩阵) 与 [§12 V2/V4](../DESIGN.md#12-验证策略)；字段与行为契约先由 #843 下沉到 focused runtime spec，不继续扩张已越过提醒线的 DESIGN。

## 整体进度判断

- M1–M5 阶段门均已通过，八个里程碑完成 5 个，当前位于 M6 起点；M7 真实取证与 M8 发布尚未开始。
- WBS 原始 checkbox 为 221/266（约 83%），但这不是发布完成度：M6–M8 包含完整双后端恢复、真实设备/Forge/Agent 证据和发布安装矩阵，不能由前五阶段的代码量替代。
- 自动化门禁中 V1/V3/V5–V8/V10a/V11–V14 已在其权威阶段闭合；M6 需要最终闭合 V2 与双后端 V4，V9 真实低频链留 M7，V10b 最终暴露面与 V15 发布矩阵留 M8。
- M1–M3 尚有少量未勾项。部分是阶段化表述而非新缺口（如 V11 已在 M4/M5 闭合），部分是真实 Runtime carryover：双后端恢复全集、backend-session mismatch、生产 qualification gate、hooks baseline/自动复核。#843 先逐项归属，#847/#848 实现，#852 只按实际证据回填。

因此下一阶段的主风险不是新增业务功能，而是证明两种宿主在最坏时序下仍共享同一 ownership/evidence chain。

## 不变量与范围边界

1. tmux 只承载 wrapper；Agent 始终由 wrapper 直接启动并留在 wrapper 进程组。
2. PTY 由 wrapper 建立并中继到宿主输出与 `agent.log`。pane、scrollback、session 存在性都不是完成事实。
3. tmux session 只提供 attach 与 mismatch 诊断，不参与 started/finished、claim 替换或消失证明。
4. process/tmux 尽量使用同一 backend-parameterized V2/V4 测试；只有 session 特有观测可以是 tmux-only 行。
5. `process-group-verified` 只表示精确 Agent executable/version 的拓扑资格，不表示同 UID 沙箱或 TM6 已闭合。真实 Agent 资格记录仍由 M7 取得。
6. kill 在确认消失后不创建 attempt；retry 只能创建一个。证明不了消失时保持隔离并复用唯一 `startup_stall` Interrupt。
7. 每个 child 串行合并后才开始下一个；不得以 blanket retry、延长 timeout、跳过 tmux 测试或放宽 owner 数量断言制造稳定性。

## 串行切片

| 顺序 | Issue | 交付 | 后置条件 |
|---|---|---|---|
| 1 | [#843](https://github.com/xsift/sift/issues/843) | Runtime/backend/PTY/session/attach/qualification 字段契约与矩阵 inventory | 独立字段评审后 active；每条 DESIGN 行有唯一归属 |
| 2 | [#844](https://github.com/xsift/sift/issues/844) | wrapper-owned PTY 与 process 拓扑重构 | 真 PTY 不改变 direct-child/同 PGID/日志与信号契约 |
| 3 | [#845](https://github.com/xsift/sift/issues/845) | tmux backend、确定性 session 与 effective backend router | 只启动 wrapper；response loss/reclaim 不双起 |
| 4 | [#846](https://github.com/xsift/sift/issues/846) | `sift attach <run>` | 只读观察；session 名不可由调用方注入 |
| 5 | [#847](https://github.com/xsift/sift/issues/847) | backend-session mismatch 与 qualification execution gate | session 仅为诊断；脱组 fixture 禁止自动 retry |
| 6 | [#848](https://github.com/xsift/sift/issues/848) | hooks baseline 写入与 attempt 完成后自动复核 | 不执行 hooks、不用 Agent 修改覆盖可信 baseline |
| 7 | [#849](https://github.com/xsift/sift/issues/849) | process/tmux 共用 V2 handoff/crash suite | lease→started 全边界原子；permit replay spawn=1 |
| 8 | [#850](https://github.com/xsift/sift/issues/850) | 双后端非人工恢复矩阵 | §10.1 pending/starting/spawning/running 行逐项命名覆盖 |
| 9 | [#851](https://github.com/xsift/sift/issues/851) | 人工态、kill/retry、迟到事实与四发现者并发 | 一条 Interrupt/一次收费/至多一个 successor owner |
| 10 | [#852](https://github.com/xsift/sift/issues/852) | M6 综合门禁 | 独立评审后才更新 M6 结论并开放 M7 |

## WBS 对账

| WBS M6 项 | 主要 Issue | 阶段证据 |
|---|---|---|
| tmux 只承载 wrapper，Agent direct child/同进程组 | #843–#845 | 生产拓扑测试 + 双后端 suite |
| wrapper 分配 PTY 并中继；session 非事实源 | #843/#844/#845 | PTY 字节与身份测试、session-loss 不影响 result/log |
| process/tmux 同一 V2/V4 套件 | #849–#851 | backend factory 生成同名 subtests |
| DESIGN §12 V4 全矩阵 | #849–#851 | #843 inventory 逐行无空缺 |
| 脱组后代标 unverified 且禁止含糊 retry | #847/#850 | synthetic qualification negative vector |
| 两后端旧 owner 消失前无新 owner；kill/retry 分诊 | #849–#851 | paused/crash/concurrent discovery assertions |
| `sift attach` 只观察 | #846/#852 | closed RPC/CLI 与零领域写入断言 |

## 测试与 CI 策略

- 每片：定向测试、适用的 `-race` 与 `-count=3`，再执行 `go test ./...`、`go vet ./...`、`git diff --check`。
- tmux 集成测试进入显式安装 tmux 的 Linux CI job；缺少 tmux 必须使该 job 失败，不得 silent skip。普通 process/四组合 build 不新增 tmux 运行依赖。
- backend-parameterized suite 输出每个 backend/矩阵行的具名 PASS sentinel，避免正则零匹配；#843 inventory 还须逐点对账 Interrupt 五件事与 retry-probe-success 的 V2 崩溃证据，不能用 package-level PASS 代替。
- 使用同步点、进程身份与持久投影证明时序，不使用 sleep 竞争；需要有界等待时由测试控制观测器/时钟。
- #852 重跑全矩阵与 race，并归档独立只读复审；组件 PR 不得提前宣称完整 V4/M6 通过。

## 主要风险与控制

1. **PTY 与进程组冲突**：常见 PTY helper 会在 Agent child 调 `setsid`/`Setctty`，静默创建新进程组并破坏证据链。#843 必须冻结“Agent child 不调用 `setsid`/`setpgid`/`Setctty`”；#844/#845 在 PTY 激活的 process/tmux 生产拓扑下同时断言 Agent PPID 指向 wrapper、`wrapper PGID == Agent PGID` 与终止组范围。
2. **tmux 外部副作用响应丢失**：`new-session` 成功后客户端可能丢响应。session identity 必须由冻结 launch identity 确定性派生，并在复用前验证绑定；不得看到同名 session 就盲目接管。
3. **把 session 误升格为事实源**：恢复只能由 wrapper/control/result/process identity 裁定；session mismatch 仅产生诊断并触发既有受控终止流程。
4. **测试矩阵体积与 flake**：先抽 backend-neutral harness，再扩行；每个交错使用显式 barrier。禁止以全套重跑代替根因修复。
5. **资格结论越界**：M6 只交付机制和 synthetic vectors；M7 才能为真实 Agent/version 写正式资格证据，且资格不等于沙箱。

## M6 完成定义

只有 #852 可宣布 M6 通过。退出条件是 WBS 七项全部有合取证据、V2/V4 权威 inventory 无空行、双后端关键向量重复/race 通过、独立复审无阻断。M6 通过仅允许进入 M7；不得据此宣称真实 Agent、双 Forge、手机审批、凭证可挂载性或 PoC 发布完成。
