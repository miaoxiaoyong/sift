// Command sift-agent-wrapper is the per-attempt agent wrapper.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/miaoxiaoyong/sift/internal/controlplane"
	"github.com/miaoxiaoyong/sift/internal/wrapper"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(controlplane.Version)
		return
	}
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: sift-agent-wrapper <bootstrap.json>")
		os.Exit(2)
	}
	if err := wrapper.Run(context.Background(), os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "sift-agent-wrapper:", err)
		os.Exit(1)
	}
}
