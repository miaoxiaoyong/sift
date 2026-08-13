// Universal -v/--verbose and -q/--quiet flag handling (issue #939). The two
// flags are accepted anywhere in the command line and stripped before any
// dispatch, so no command's own flag parser ever rejects them. Commands
// without verbose/quiet semantics simply ignore the consumed flags (behavior
// unchanged); update and version implement the semantics. Errors always go to
// stderr and machine output (--json) is never suppressed by -q.
package main

import (
	"fmt"
	"io"
)

// splitGlobalFlags extracts the universal -v/--verbose and -q/--quiet flags
// from a command line (everything after the program name). Accepted spellings
// mirror the stdlib flag package: a single dash and a double dash are
// equivalent for the long forms. The flags are consumed in any position so
// `sift -v update`, `sift update -v` and `sift update --check -q` all parse.
func splitGlobalFlags(args []string) (rest []string, verbose, quiet bool) {
	rest = make([]string, 0, len(args))
	for _, a := range args {
		switch a {
		case "-v", "-verbose", "--verbose":
			verbose = true
		case "-q", "-quiet", "--quiet":
			quiet = true
		default:
			rest = append(rest, a)
		}
	}
	return rest, verbose, quiet
}

// isGlobalFlag reports whether s is one of the accepted global flag
// spellings (used to fall back to the overview for a bare `sift -v`).
func isGlobalFlag(s string) bool {
	_, verbose, quiet := splitGlobalFlags([]string{s})
	return verbose || quiet
}

// humanf writes a human success/progress message unless --quiet suppressed
// it. It never touches stderr (errors) or machine output.
func humanf(w io.Writer, quiet bool, format string, a ...any) {
	if quiet {
		return
	}
	fmt.Fprintf(w, format, a...)
}

// verbosef writes a verbose-only progress line: emitted only with -v and
// never with -q (quiet wins) or into machine output (callers gate on JSON).
func verbosef(w io.Writer, verbose, quiet bool, format string, a ...any) {
	if !verbose || quiet {
		return
	}
	fmt.Fprintf(w, format, a...)
}
