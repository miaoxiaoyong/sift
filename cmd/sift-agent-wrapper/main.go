// Command sift-agent-wrapper is the per-attempt agent wrapper.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/xsift/sift/internal/controlplane"
	"github.com/xsift/sift/internal/version"
	"github.com/xsift/sift/internal/wrapper"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(version.Release)
		return
	}
	// --protocol-major exposes the wrapper's wire protocol major so the daemon
	// can verify the installed wrapper pairs with its own protocol generation
	// independently of the SemVer (same release can bump ProtocolMajor).
	if len(os.Args) == 2 && os.Args[1] == "--protocol-major" {
		fmt.Println(controlplane.ProtocolMajor)
		return
	}
	if len(os.Args) == 4 && os.Args[1] == "--reap-process-group" {
		pgid, err := strconv.Atoi(os.Args[2])
		if err == nil {
			err = wrapper.ReapProcessGroup(pgid, os.Args[3])
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "sift-agent-wrapper:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "--run" {
		if err := wrapper.RunExecution(context.Background(), os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "sift-agent-wrapper:", err)
			os.Exit(1)
		}
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
