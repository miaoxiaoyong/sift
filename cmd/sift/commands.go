// Issue #935 (Tier-1 discoverability): commands is the single source of truth
// for every discoverability surface of the sift CLI — the top-level `sift
// help`, per-command `--help`/`-h`/`sift help <cmd>`, and the bash/zsh/fish
// completion scripts. It is deliberately hand-maintained and static: each
// command's one-liner, usage, flags and examples are written here once so
// help, completion and the dispatch table cannot drift apart, and completion
// needs no RPC or runtime state.
package main

import (
	"fmt"
	"io"
	"strings"
)

// flagMeta describes one CLI flag. value is the placeholder for a flag that
// takes an argument ("PATH", "ID", ...); empty means a boolean switch.
type flagMeta struct {
	flag  string
	value string
	desc  string
}

// commandMeta is one row of the command metadata table.
type commandMeta struct {
	name            string   // dispatch verb (args[1])
	group           string   // top-level help section
	brief           string   // top-level listing line, e.g. "logs <run-id>"
	summary         string   // Chinese one-liner
	usage           string   // full usage line, starts with "sift "
	subcommands     []string // action verbs (project/agent/service/report)
	subDescriptions map[string]string
	flags           []flagMeta
	examples        []string
}

const (
	groupBasics  = "基础命令"
	groupQuery   = "查询命令"
	groupControl = "运行控制"
	groupOther   = "其他命令"
)

var commands = []commandMeta{
	{
		name:    "init",
		group:   groupBasics,
		brief:   "init",
		summary: "交互式初始化本地配置",
		usage:   "sift init [--offline] [--agent NAME] [--agent-args ARG,ARG] [--project PATH] [--operator LOGIN] [--forge github|gitlab]",
		flags: []flagMeta{
			{"--offline", "", "非交互模式：跳过所有提示与 forge 登录探测"},
			{"--agent", "NAME", "添加 Agent（id=可执行文件或可执行文件名）"},
			{"--agent-args", "ARG,ARG", "逗号分隔的 Agent 启动参数（覆盖默认值）"},
			{"--project", "PATH", "项目仓库路径（默认当前 git 工作树）"},
			{"--operator", "LOGIN", "操作员登录（github:user,gitlab:user）"},
			{"--forge", "github|gitlab", "覆盖 Forge 类型自动探测"},
		},
		examples: []string{"sift init", "sift init --agent claude --project ."},
	},
	{
		name:            "project",
		group:           groupBasics,
		brief:           "project add",
		summary:         "添加一个项目（默认当前 git 仓库，forge 自动探测）",
		usage:           "sift project add [--project PATH] [--forge github|gitlab] [--offline]",
		subcommands:     []string{"add"},
		subDescriptions: map[string]string{"add": "添加项目"},
		flags: []flagMeta{
			{"--project", "PATH", "项目仓库路径（默认当前 git 工作树）"},
			{"--forge", "github|gitlab", "覆盖 Forge 类型自动探测"},
			{"--offline", "", "非交互模式：跳过所有提示与 forge 登录探测"},
		},
		examples: []string{"cd <项目> && sift project add", "sift project add --forge gitlab"},
	},
	{
		name:            "agent",
		group:           groupBasics,
		brief:           "agent add",
		summary:         "添加一个 Agent",
		usage:           "sift agent add [--agent NAME] [--agent-args ARG,ARG] [--offline]",
		subcommands:     []string{"add"},
		subDescriptions: map[string]string{"add": "添加 Agent"},
		flags: []flagMeta{
			{"--agent", "NAME", "Agent 可执行文件，或 id=executable"},
			{"--agent-args", "ARG,ARG", "逗号分隔的 Agent 启动参数（覆盖默认值）"},
			{"--offline", "", "非交互模式：跳过所有提示与 forge 登录探测"},
		},
		examples: []string{"sift agent add --agent claude"},
	},
	{
		name:     "daemon",
		group:    groupBasics,
		brief:    "daemon",
		summary:  "启动本地守护进程",
		usage:    "sift daemon",
		examples: []string{"sift daemon"},
	},
	{
		name:    "doctor",
		group:   groupBasics,
		brief:   "doctor",
		summary: "检查本地环境并报告问题",
		usage:   "sift doctor [--offline] [--json]",
		flags: []flagMeta{
			{"--offline", "", "离线检查，不连接守护进程"},
			{"--json", "", "输出机器可读的 JSON"},
		},
		examples: []string{"sift doctor", "sift doctor --offline"},
	},
	{
		name:     "install",
		group:    groupBasics,
		brief:    "install",
		summary:  "安装 Sift 发布包",
		usage:    "sift install <release-archive.tar.gz>",
		examples: []string{"sift install sift_0.2.0_darwin_arm64.tar.gz"},
	},
	{
		name:    "update",
		group:   groupBasics,
		brief:   "update",
		summary: "升级到最新版本",
		usage:   "sift update [--check] [--version X] [--force] [--json]",
		flags: []flagMeta{
			{"--check", "", "只报告当前与最新版本，不安装"},
			{"--version", "X", "安装指定版本（不经过最新版检查）"},
			{"--force", "", "即使已是最新也重新安装"},
			{"--json", "", "输出机器可读的 JSON"},
		},
		examples: []string{"sift update --check", "sift update"},
	},
	{
		name:            "service",
		group:           groupBasics,
		brief:           "service",
		summary:         "管理后台服务（launchd/systemd/前台）",
		usage:           "sift service <install|uninstall|start|stop|restart|reload|status>",
		subcommands:     []string{"install", "uninstall", "start", "stop", "restart", "reload", "status"},
		subDescriptions: map[string]string{"install": "安装服务单元", "uninstall": "卸载服务单元", "start": "启动服务", "stop": "停止服务", "restart": "重启服务", "reload": "重载服务（等价于 restart）", "status": "查看服务状态"},
		examples:        []string{"sift service status", "sift service install"},
	},
	{
		name:    "ps",
		group:   groupQuery,
		brief:   "ps",
		summary: "查看运行中的任务",
		usage:   "sift ps [--json]",
		flags: []flagMeta{
			{"--json", "", "输出机器可读的 JSON"},
		},
		examples: []string{"sift ps"},
	},
	{
		name:    "logs",
		group:   groupQuery,
		brief:   "logs <run-id>",
		summary: "查看指定运行的日志",
		usage:   "sift logs <run-id> [--json]",
		flags: []flagMeta{
			{"--json", "", "输出机器可读的 JSON"},
		},
		examples: []string{"sift logs run-123"},
	},
	{
		name:    "timeline",
		group:   groupQuery,
		brief:   "timeline",
		summary: "查看事件时间线",
		usage:   "sift timeline [--run ID] [--project ID] [--type T] [--limit N] [--after-seq N] [--after-ms MS] [--json]",
		flags: []flagMeta{
			{"--run", "ID", "按运行 ID 过滤"},
			{"--project", "ID", "按项目 ID 过滤"},
			{"--type", "T", "按事件类型过滤"},
			{"--limit", "N", "最大事件数（1..1000，默认 100）"},
			{"--after-seq", "N", "键集分页游标（序列号）"},
			{"--after-ms", "MS", "显式时间戳游标半部（可选）"},
			{"--json", "", "输出机器可读的 JSON"},
		},
		examples: []string{"sift timeline", "sift timeline --project demo --limit 20"},
	},
	{
		name:    "metrics",
		group:   groupQuery,
		brief:   "metrics",
		summary: "查看运行指标",
		usage:   "sift metrics [--project ID] [--json]",
		flags: []flagMeta{
			{"--project", "ID", "按项目 ID 限定范围"},
			{"--json", "", "输出机器可读的 JSON"},
		},
		examples: []string{"sift metrics", "sift metrics --project demo"},
	},
	{
		name:    "worktree",
		group:   groupQuery,
		brief:   "worktree <run-id>",
		summary: "查看运行对应的工作树",
		usage:   "sift worktree <run-id> [--json]",
		flags: []flagMeta{
			{"--json", "", "输出机器可读的 JSON"},
		},
		examples: []string{"sift worktree run-123"},
	},
	{
		name:    "attach",
		group:   groupQuery,
		brief:   "attach <run-id>",
		summary: "只读连接到运行会话",
		usage:   "sift attach <run-id> [--json]",
		flags: []flagMeta{
			{"--json", "", "输出机器可读的 JSON"},
		},
		examples: []string{"sift attach run-123"},
	},
	{
		name:    "status",
		group:   groupQuery,
		brief:   "status",
		summary: "查看 Sift 整体状态（离线、快速，不联网）",
		usage:   "sift status [--json]",
		flags: []flagMeta{
			{"--json", "", "输出机器可读的 JSON"},
		},
		examples: []string{"sift status", "sift status --json"},
	},
	{
		name:    "kill",
		group:   groupControl,
		brief:   "kill <run-id>",
		summary: "停止指定运行",
		usage:   "sift kill --expected-version N --request-key KEY <run-id> [--json]",
		flags: []flagMeta{
			{"--expected-version", "N", "期望的运行版本（防止误杀）"},
			{"--request-key", "KEY", "幂等请求键"},
			{"--json", "", "输出机器可读的 JSON"},
		},
		examples: []string{"sift kill --expected-version 2 --request-key stop-1 run-123"},
	},
	{
		name:    "retry",
		group:   groupControl,
		brief:   "retry <run-id>",
		summary: "重试指定运行",
		usage:   "sift retry --expected-version N --request-key KEY <run-id> [--json]",
		flags: []flagMeta{
			{"--expected-version", "N", "期望的运行版本（防止误杀）"},
			{"--request-key", "KEY", "幂等请求键"},
			{"--json", "", "输出机器可读的 JSON"},
		},
		examples: []string{"sift retry --expected-version 2 --request-key retry-1 run-123"},
	},
	{
		name:            "report",
		group:           groupControl,
		brief:           "report <kind>",
		summary:         "向运行提交报告",
		usage:           "sift report <progress|goal|blocker|completed> --key KEY --payload JSON [--json]",
		subcommands:     []string{"progress", "goal", "blocker", "completed"},
		subDescriptions: map[string]string{"progress": "进度报告", "goal": "目标报告", "blocker": "阻塞报告", "completed": "完成报告"},
		flags: []flagMeta{
			{"--key", "KEY", "32 位小写十六进制报告键"},
			{"--payload", "JSON", "闭合 JSON 对象"},
			{"--json", "", "输出机器可读的 JSON"},
		},
		examples: []string{"sift report progress --key 0123456789abcdef0123456789abcdef --payload '{\"status\":\"on_track\"}'"},
	},
	{
		name:     "hooks-bootstrap",
		group:    groupControl,
		brief:    "hooks-bootstrap",
		summary:  "为项目安装 Git hooks",
		usage:    "sift hooks-bootstrap <project-id>",
		examples: []string{"sift hooks-bootstrap project-1"},
	},
	{
		name:     "completion",
		group:    groupOther,
		brief:    "completion",
		summary:  "生成 shell 补全脚本",
		usage:    "sift completion bash|zsh|fish",
		examples: []string{"eval \"$(sift completion zsh)\"", "sift completion bash > ~/.bash_completion.d/sift"},
	},
}

var commandsByName = func() map[string]commandMeta {
	m := make(map[string]commandMeta, len(commands))
	for _, c := range commands {
		m[c.name] = c
	}
	return m
}()

func commandsInGroup(group string) []commandMeta {
	var out []commandMeta
	for _, c := range commands {
		if c.group == group {
			out = append(out, c)
		}
	}
	return out
}

func commandNames() []string {
	out := make([]string, 0, len(commands))
	for _, c := range commands {
		out = append(out, c.name)
	}
	return out
}

func (m commandMeta) flagWords() []string {
	out := make([]string, 0, len(m.flags))
	for _, f := range m.flags {
		out = append(out, f.flag)
	}
	return out
}

// commandHelp renders help for one command from the metadata table. It is the
// shared implementation of `sift help <cmd>`, `sift <cmd> --help` and
// `sift <cmd> -h` (issue #935); with command == "" it renders the top-level
// overview. Unknown commands are usage-class errors (exit 2).
func commandHelp(command string, stdout, stderr io.Writer) int {
	if command == "" {
		renderTopHelp(stdout)
		return 0
	}
	meta, ok := commandsByName[command]
	if !ok {
		report(stderr, fmt.Errorf("未知命令 %q；运行 sift help 查看可用命令", command))
		return 2
	}
	renderCommandHelp(stdout, meta)
	return 0
}

// renderTopHelp lists every command grouped by area, derived from the same
// table as per-command help and completion so the three surfaces never drift.
func renderTopHelp(w io.Writer) {
	fmt.Fprintln(w, "Sift 命令参考")
	for _, group := range []string{groupBasics, groupQuery, groupControl, groupOther} {
		metas := commandsInGroup(group)
		fmt.Fprintf(w, "\n%s：\n", group)
		width := 0
		for _, m := range metas {
			if len(m.brief) > width {
				width = len(m.brief)
			}
		}
		for _, m := range metas {
			fmt.Fprintf(w, "  %s%s%s\n", m.brief, strings.Repeat(" ", width+2-len(m.brief)), m.summary)
		}
	}
	fmt.Fprintln(w, "\n用法：sift <命令> [选项]")
	fmt.Fprintln(w, "示例：sift init；sift doctor --offline；sift ps")
}

// renderCommandHelp prints the Chinese one-liner, usage, flags and examples
// for one command (issue #935: 每命令一句话 + flags + 示例).
func renderCommandHelp(w io.Writer, m commandMeta) {
	fmt.Fprintf(w, "sift %s\n\n%s\n\n用法：%s\n", m.name, m.summary, m.usage)
	if len(m.flags) > 0 {
		width := 0
		for _, f := range m.flags {
			if l := flagLabelLen(f); l > width {
				width = l
			}
		}
		fmt.Fprintln(w, "\n选项：")
		for _, f := range m.flags {
			label := flagLabel(f)
			fmt.Fprintf(w, "  %s%s%s\n", label, strings.Repeat(" ", width+2-len(label)), f.desc)
		}
	}
	if len(m.examples) > 0 {
		fmt.Fprintln(w, "\n示例：")
		for _, e := range m.examples {
			fmt.Fprintf(w, "  %s\n", e)
		}
	}
}

func flagLabel(f flagMeta) string {
	if f.value == "" {
		return f.flag
	}
	return f.flag + " " + f.value
}

func flagLabelLen(f flagMeta) int { return len(flagLabel(f)) }
