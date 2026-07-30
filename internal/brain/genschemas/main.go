// Command genschemas regenerates the versioned Brain output schemas
// (prompts/<touchpoint>/v<N>.schema.json) from the contract structs via
// schemagen. It is invoked by the //go:generate directive in assets.go; the
// brain drift test fails if the committed files diverge (brain.md §2: the
// .schema.json is generated, never a hand-written second copy).
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/miaoxiaoyong/sift/internal/brain"
	"github.com/miaoxiaoyong/sift/internal/schema/schemagen"
)

func main() {
	targets := []struct {
		dir   string
		value any
	}{
		{"prompts/T1", brain.T1Output{}},
		{"prompts/T2", brain.T2Output{}},
		{"prompts/T3", brain.T3Output{}},
		{"prompts/T4", brain.T4Output{}},
		{"prompts/T5", brain.T5Output{}},
		{"prompts/T6", brain.T6Output{}},
		{"prompts/T7", brain.T7Output{}},
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
		path := filepath.Join(t.dir, "v1.schema.json")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			fatalf("write %s: %v", path, err)
		}
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "genschemas: "+format+"\n", args...)
	os.Exit(1)
}
