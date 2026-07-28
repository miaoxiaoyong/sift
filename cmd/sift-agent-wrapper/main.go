// Command sift-agent-wrapper is the per-attempt agent wrapper.
//
// This is a WBS M1 bootstrap stub; see docs/plans/2026-07-29-s1-m1-bootstrap-decode.md.
package main

import (
	"fmt"
	"os"

	"github.com/miaoxiaoyong/sift/internal/controlplane"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(controlplane.Version)
		return
	}
	fmt.Fprintln(os.Stderr, "sift-agent-wrapper: wrapper stub — not implemented (WBS M1 bootstrap)")
}
