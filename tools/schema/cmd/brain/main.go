// Command brain regenerates the versioned Brain output schemas from their
// contract structs. The root module invokes it through the //go:generate
// directive in internal/brain/assets.go; CI rejects artifact drift.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/xsift/sift/internal/brain"
	"github.com/xsift/sift/tools/schema/schemagen"
)

func main() {
	out := flag.String("out", "../../internal/brain/prompts", "output directory for generated Brain schemas")
	flag.Parse()
	targets := []struct {
		dir   string
		value any
	}{
		{"T1", brain.T1Output{}},
		{"T2", brain.T2Output{}},
		{"T3", brain.T3Output{}},
		{"T4", brain.T4Output{}},
		{"T5", brain.T5Output{}},
		{"T6", brain.T6Output{}},
		{"T7", brain.T7Output{}},
	}
	for _, t := range targets {
		tgt, err := schemagen.TargetFor(t.value)
		if err != nil {
			fatalf("%v", err)
		}
		data, err := schemagen.Generate(tgt)
		if err != nil {
			fatalf("generate %s: %v", t.dir, err)
		}
		path := filepath.Join(*out, t.dir, "v1.schema.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			fatalf("write %s: %v", path, err)
		}
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "genschemas: "+format+"\n", args...)
	os.Exit(1)
}
