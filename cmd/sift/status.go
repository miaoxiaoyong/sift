// `sift status` (issue #935): a fast, offline one-glance overview of the local
// Sift installation. It deliberately never runs an RPC, never touches the
// network and never opens the database: daemon liveness is the operator
// socket's presence plus a bounded connect (with PID from peer credentials),
// config validity is config.Load, and project counts come from the loaded
// snapshot. "Check for updates" is a hint to run `sift update --check`, never
// a network call here.
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/version"
)

type statusDaemon struct {
	Running bool `json:"running"`
	Socket  bool `json:"socket"` // socket file present (may be stale)
	PID     int  `json:"pid,omitempty"`
}

type statusConfig struct {
	Valid   bool   `json:"valid"` // present AND config.Load succeeds
	Present bool   `json:"present"`
	Path    string `json:"path"`
	Error   string `json:"error,omitempty"`
}

type statusProjects struct {
	Enabled int `json:"enabled"`
	Total   int `json:"total"`
}

type statusResult struct {
	Daemon   statusDaemon   `json:"daemon"`
	Config   statusConfig   `json:"config"`
	Version  string         `json:"version"`
	Projects statusProjects `json:"projects"`
}

func runStatus(args []string, home config.Home, stdout, stderr io.Writer) int {
	jsonOutput := os.Getenv("SIFT_JSON") == "1"
	for _, a := range args {
		switch a {
		case "--json":
			jsonOutput = true
		default:
			report(stderr, fmt.Errorf("usage: sift status [--json]"))
			return 2
		}
	}
	st := collectStatus(home)
	if jsonOutput {
		if err := printJSON(stdout, st); err != nil {
			report(stderr, err)
			return 1
		}
		return 0
	}
	renderStatusHuman(stdout, st)
	return 0
}

func collectStatus(home config.Home) statusResult {
	st := statusResult{Version: version.Release}

	sockPath := filepath.Join(home.Path, "siftd.sock")
	if isSocket(sockPath) {
		st.Daemon.Socket = true
		// A stale socket file outlives a crashed daemon; only a successful
		// bounded connect proves liveness. The bare connect/close is benign:
		// the server reads a frame, sees EOF and closes the connection.
		conn, err := net.DialTimeout("unix", sockPath, 200*time.Millisecond)
		if err == nil {
			st.Daemon.Running = true
			if uc, ok := conn.(*net.UnixConn); ok {
				st.Daemon.PID = daemonPID(uc)
			}
			_ = conn.Close()
		}
	}

	st.Config.Path = config.ConfigPath(home)
	if _, err := os.Stat(st.Config.Path); err == nil {
		st.Config.Present = true
	}
	snapshot, loadErr := config.Load(home, time.Now())
	if loadErr == nil {
		// Validity for a one-glance overview means "configured and loadable":
		// an absent file loads the zero-config defaults, but that is not a
		// configured installation yet.
		if st.Config.Present {
			st.Config.Valid = true
		}
		if snapshot.Config != nil {
			for _, p := range snapshot.Config.Projects {
				st.Projects.Total++
				if p.Enabled {
					st.Projects.Enabled++
				}
			}
		}
	} else {
		st.Config.Error = loadErr.Error()
	}
	return st
}

func renderStatusHuman(w io.Writer, st statusResult) {
	fmt.Fprintln(w, "Sift 状态")
	switch {
	case st.Daemon.Running:
		// PID is optional (peerpid_other.go / getsockopt failure yields 0):
		// a live connect proves liveness, so report 运行中 without a
		// misleading "PID 0" (issue #935 / F935-3).
		if st.Daemon.PID > 0 {
			fmt.Fprintf(w, "✓ daemon：运行中（PID %d）\n", st.Daemon.PID)
		} else {
			fmt.Fprintln(w, "✓ daemon：运行中")
		}
	case st.Daemon.Socket:
		fmt.Fprintln(w, "✗ daemon：未运行（siftd.sock 存在但无响应，可能为残留文件）")
	default:
		fmt.Fprintln(w, "✗ daemon：未运行（运行 sift daemon 或 sift service install）")
	}
	switch {
	case !st.Config.Present:
		fmt.Fprintf(w, "✗ config：未配置（运行 sift init 完成交互式配置）\n")
	case st.Config.Valid:
		fmt.Fprintf(w, "✓ config：有效（%s）\n", st.Config.Path)
	default:
		fmt.Fprintf(w, "✗ config：无效（%s；运行 sift init 重新生成）\n", st.Config.Error)
	}
	fmt.Fprintf(w, "ℹ 版本：%s（运行 sift update --check 查看是否有新版）\n", st.Version)
	switch {
	case st.Projects.Total == 0:
		fmt.Fprintln(w, "⚠ 项目：0 个（运行 sift project add 添加）")
	case st.Projects.Enabled == st.Projects.Total:
		fmt.Fprintf(w, "✓ 项目：%d 个，全部启用\n", st.Projects.Total)
	default:
		fmt.Fprintf(w, "⚠ 项目：%d 个（%d 启用）\n", st.Projects.Total, st.Projects.Enabled)
	}
}
