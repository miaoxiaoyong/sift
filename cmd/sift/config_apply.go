package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/hosting"
)

// announceConfigApplied applies a local config edit to a running daemon. The
// daemon currently captures config while assembling its workers, so a managed
// daemon must restart rather than partially accepting a new configuration.
func announceConfigApplied(home config.Home, stdout, stderr io.Writer) {
	if !isSocket(filepath.Join(home.Path, "siftd.sock")) {
		fmt.Fprintln(stdout, "✓ 下一步：运行 sift daemon 或 sift service install 启动")
		return
	}

	spec, err := hosting.NewSpec(home.Path)
	if err == nil && spec.Backend != hosting.BackendForeground {
		if _, err := os.Stat(spec.UnitPath); err == nil {
			var serviceOut, serviceErr bytes.Buffer
			if runService([]string{"restart"}, home, &serviceOut, &serviceErr) == 0 {
				fmt.Fprintln(stdout, "✓ 配置已自动重启 daemon 并生效")
				return
			}
			fmt.Fprintln(stderr, "⚠ 配置已写入，但自动重启后台服务失败；请检查 sift service status 后重试 restart")
			if serviceErr.Len() > 0 {
				_, _ = stderr.Write(serviceErr.Bytes())
			}
			return
		}
	}
	fmt.Fprintln(stdout, "⚠ daemon 正在前台运行；请重启 `sift daemon` 使新配置完整生效")
}
