// Issue #937 (Tier-2 CRUD): `sift project list|remove` and `sift agent
// list|remove`. list reads config.yaml offline — it never contacts the daemon
// — and renders every registered project/agent as a table or structured JSON
// (--json); remove deletes by id through the same atomic write path as setup
// (temp file + rename + .bak + 0600, with config validation first so a remove
// never launders an invalid config) and is daemon-aware (socket present →
// `sift service reload` hint).
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miaoxiaoyong/sift/internal/agents"
	"github.com/miaoxiaoyong/sift/internal/cli/render"
	"github.com/miaoxiaoyong/sift/internal/config"
)

// runResourceCommand dispatches the CRUD verbs of `sift project` / `sift
// agent` (issue #937): add keeps its setup.go implementation (interactive
// wizard / flags), list and remove are the local config operations below.
func runResourceCommand(resource string, args []string, stdin io.Reader, home config.Home, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		report(stderr, fmt.Errorf("usage: sift %s <add|list|remove>", resource))
		return 2
	}
	switch args[0] {
	case "add":
		scope := setupProject
		if resource == "agent" {
			scope = setupAgent
		}
		return runSetup(args[1:], stdin, home, stdout, stderr, scope)
	case "list":
		return runResourceList(resource, args[1:], home, stdout, stderr)
	case "remove":
		return runResourceRemove(resource, args[1:], home, stdout, stderr)
	default:
		report(stderr, fmt.Errorf("未知子命令 %q；支持 add|list|remove", args[0]))
		return 2
	}
}

// runResourceList implements `sift project list` / `sift agent list`: an
// offline read of the validated config snapshot (config.Load fails closed on
// an invalid config), rendered as a table by default or as structured JSON
// with --json / SIFT_JSON=1. An absent config or empty registry renders a
// friendly hint with exit 0.
func runResourceList(resource string, args []string, home config.Home, stdout, stderr io.Writer) int {
	jsonOutput := os.Getenv("SIFT_JSON") == "1"
	for _, a := range args {
		switch a {
		case "--json":
			jsonOutput = true
		default:
			report(stderr, fmt.Errorf("usage: sift %s list [--json]", resource))
			return 2
		}
	}
	snap, err := config.Load(home, time.Now())
	if err != nil {
		report(stderr, fmt.Errorf("读取配置失败：%w", err))
		return 1
	}
	if jsonOutput {
		if err := printJSON(stdout, listResultJSON(resource, snap)); err != nil {
			report(stderr, err)
			return 1
		}
		return 0
	}
	renderResourceListHuman(stdout, resource, snap)
	return 0
}

// forgeRefJSON is the structured forge binding of one project row (issue
// #937: --json is the scripting surface, so no host information is lost).
type forgeRefJSON struct {
	Kind    string `json:"kind"`
	Project string `json:"project"`
	Host    string `json:"host"`
}

// projectItemJSON is one `sift project list --json` row.
type projectItemJSON struct {
	ID      string       `json:"id"`
	Repo    string       `json:"repo"`
	Forge   forgeRefJSON `json:"forge"`
	Enabled bool         `json:"enabled"`
	Agents  []string     `json:"agents"`
}

// agentCharJSON is the built-in characteristic profile of one agent row, from
// the internal/agents registry keyed by executable base name (issue #930,
// #937: strengths·context·cost·speed·notes).
type agentCharJSON struct {
	Strengths []string     `json:"strengths"`
	Context   string       `json:"context"`
	Cost      agents.Cost  `json:"cost"`
	Speed     agents.Speed `json:"speed"`
	Notes     string       `json:"notes"`
}

// agentItemJSON is one `sift agent list --json` row.
type agentItemJSON struct {
	ID              string        `json:"id"`
	Executable      string        `json:"executable"`
	Args            []string      `json:"args"`
	Backend         string        `json:"backend"`
	Characteristics agentCharJSON `json:"characteristics"`
}

// listResultJSON builds the structured --json result: {"projects": [...]} for
// `sift project list`, {"agents": [...]} for `sift agent list`. Empty
// registries render as empty arrays, never null.
func listResultJSON(resource string, snap *config.Snapshot) any {
	if resource == "project" {
		items := make([]projectItemJSON, 0, len(snap.Config.Projects))
		for _, p := range snap.Config.Projects {
			items = append(items, projectItemJSON{
				ID:   p.ID,
				Repo: p.Repo,
				Forge: forgeRefJSON{
					Kind:    string(p.Forge.Kind),
					Project: p.Forge.Project,
					Host:    p.Forge.Host,
				},
				Enabled: p.Enabled,
				Agents:  p.Agents,
			})
		}
		return map[string]any{"projects": items}
	}
	items := make([]agentItemJSON, 0, len(snap.Config.Agents))
	for _, a := range snap.Config.Agents {
		char := agents.For(a.Executable)
		items = append(items, agentItemJSON{
			ID:         a.ID,
			Executable: a.Executable,
			Args:       a.Args,
			Backend:    string(a.Backend),
			Characteristics: agentCharJSON{
				Strengths: char.Strengths,
				Context:   char.Context,
				Cost:      char.Cost,
				Speed:     char.Speed,
				Notes:     char.Notes,
			},
		})
	}
	return map[string]any{"agents": items}
}

// renderResourceListHuman prints the table surface. Project columns answer
// "项目在哪个目录": id | repo（目录绝对路径）| forge（kind:project）| enabled
// | agents. Agent columns: id | executable | args | backend | 特点（registry
// strengths·context·cost·speed）.
func renderResourceListHuman(w io.Writer, resource string, snap *config.Snapshot) {
	if resource == "project" {
		projects := snap.Config.Projects
		if len(projects) == 0 {
			fmt.Fprintln(w, "⚠ 尚未注册项目；运行 sift project add 添加")
			return
		}
		rows := make([][]string, 0, len(projects))
		for _, p := range projects {
			agentsCol := strings.Join(p.Agents, ",")
			if agentsCol == "" {
				agentsCol = "—"
			}
			rows = append(rows, []string{
				p.ID,
				p.Repo,
				fmt.Sprintf("%s:%s", p.Forge.Kind, p.Forge.Project),
				enabledLabel(p.Enabled),
				agentsCol,
			})
		}
		fmt.Fprint(w, render.Table([]string{"id", "repo", "forge", "enabled", "agents"}, rows))
		return
	}
	entries := snap.Config.Agents
	if len(entries) == 0 {
		fmt.Fprintln(w, "⚠ 尚未注册 Agent；运行 sift agent add 添加")
		return
	}
	rows := make([][]string, 0, len(entries))
	for _, a := range entries {
		argsCol := strings.Join(a.Args, " ")
		if argsCol == "" {
			argsCol = "—"
		}
		rows = append(rows, []string{
			a.ID,
			a.Executable,
			argsCol,
			string(a.Backend),
			agents.For(a.Executable).Summary(),
		})
	}
	fmt.Fprint(w, render.Table([]string{"id", "executable", "args", "backend", "特点"}, rows))
}

func enabledLabel(v bool) string {
	if v {
		return "是"
	}
	return "否"
}

// runResourceRemove implements `sift project remove <id>` / `sift agent
// remove <id>` (issue #937). The write path is the setup one (validation +
// atomic temp-file rename + .bak + 0600); a missing id is a clear error with
// a non-zero exit and re-running the same remove is that same not-found path
// (idempotent, never a crash). A present daemon socket adds the reload hint.
func runResourceRemove(resource string, args []string, home config.Home, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		report(stderr, fmt.Errorf("usage: sift %s remove <id>", resource))
		return 2
	}
	id := args[0]
	removed, err := removeSetupItem(home, resource, id)
	if err != nil {
		report(stderr, err)
		return 1
	}
	if !removed {
		report(stderr, fmt.Errorf("%s %q 不存在（运行 sift %s list 查看已注册项）", resourceLabel(resource), id, resource))
		return 1
	}
	fmt.Fprintf(stdout, "%s 已移除%s %s\n", render.Status("ok"), resourceLabel(resource), id)
	if isSocket(filepath.Join(home.Path, "siftd.sock")) {
		fmt.Fprintln(stdout, "⚠ daemon 运行中，运行 sift service reload 使新配置生效")
	}
	return 0
}

// removeSetupItem deletes the registered entry with id from config.yaml
// (resource is "project" or "agent"). setupDocument validates the current
// document first so a remove never launders an invalid config, then the entry
// is dropped and the document is rewritten through writeSetupDocument's
// temp-file + rename + .bak + 0600 path (backup only when the file existed).
// The bool is false when no entry carries the id — the caller turns that into
// the not-found error.
func removeSetupItem(home config.Home, resource, id string) (bool, error) {
	doc, existed, err := setupDocument(home)
	if err != nil {
		return false, err
	}
	key := resource + "s" // projects / agents
	items := list(doc, key)
	filtered := make([]any, 0, len(items))
	removed := false
	for _, item := range items {
		m, ok := item.(map[string]any)
		if ok && m["id"] == id {
			removed = true
			continue
		}
		filtered = append(filtered, item)
	}
	if !removed {
		return false, nil
	}
	doc[key] = filtered
	if err := writeSetupDocument(home, doc, existed); err != nil {
		return false, err
	}
	return true, nil
}

func resourceLabel(resource string) string {
	if resource == "agent" {
		return "Agent"
	}
	return "项目"
}
