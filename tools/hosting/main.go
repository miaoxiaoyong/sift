// Command hosting is the M8 §8.2 hosting/tooling driver. It renders release
// artifacts that the release pipeline or a tap maintainer consumes, keeping a
// single source of truth (the internal/hosting package) for the unit/Formula
// text rather than hand-maintained copies.
//
//	go run ./tools/hosting formula --version 0.1.0 --sha256 <hex>
//
// The published Homebrew tap is its own repository (WBS §8.2 non-scope); this
// command only produces the draft formula with a concrete version + digest so
// a release can copy it into the tap. The archive name and layout it emits
// match specs/release.md §2 exactly.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/miaoxiaoyong/sift/internal/hosting"
	"github.com/miaoxiaoyong/sift/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "formula":
		err = cmdFormula(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sift hosting:", err)
		os.Exit(1)
	}
}

func cmdFormula(args []string) error {
	fs := flag.NewFlagSet("formula", flag.ContinueOnError)
	rel := fs.String("version", version.Release, "release version (canonical SemVer)")
	sha := fs.String("sha256", "", "sha256 of the darwin/arm64 release archive")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !version.IsValidSemver(*rel) {
		return fmt.Errorf("version %q is not canonical SemVer", *rel)
	}
	if *sha != "" && !isHex64(*sha) {
		return fmt.Errorf("sha256 %q is not 64 lowercase hex digits", *sha)
	}
	fmt.Print(hosting.Formula(*rel, *sha))
	return nil
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: go run ./tools/hosting formula [--version V] [--sha256 H]")
}
