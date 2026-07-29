package brain

import (
	"regexp"
	"testing"

	"github.com/miaoxiaoyong/sift/internal/contract/schemagen"
)

func TestPromptAssetsVersioning(t *testing.T) {
	for _, asset := range []PromptAsset{T1Asset(), T2Asset(), T3Asset(), T5Asset()} {
		if len(asset.Prompt) == 0 || len(asset.Schema) == 0 {
			t.Fatalf("%s: embedded assets empty", asset.Touchpoint)
		}
		pat := regexp.MustCompile(`^T[1235]/v1/[0-9a-f]{12}$`)
		if !pat.MatchString(asset.PromptVersion) {
			t.Fatalf("%s: prompt_version %q malformed", asset.Touchpoint, asset.PromptVersion)
		}
		if asset.OutputSchemaVersion != 1 {
			t.Fatalf("%s: output_schema_version = %d", asset.Touchpoint, asset.OutputSchemaVersion)
		}
	}
	if T1Asset().PromptVersion == T2Asset().PromptVersion {
		t.Fatal("touchpoint prompt versions must differ")
	}
}

// TestSchemaDrift regenerates the v1 schemas from the contract structs and
// fails on any diff with the committed assets (brain.md §2: the .schema.json
// is generated from §7/§8 field definitions, never a hand-written copy).
func TestSchemaDrift(t *testing.T) {
	cases := []struct {
		asset PromptAsset
		typ   any
	}{
		{T1Asset(), T1Output{}},
		{T2Asset(), T2Output{}},
		{T3Asset(), T3Output{}},
		{T5Asset(), T5Output{}},
	}
	for _, tc := range cases {
		tgt, err := schemagen.TargetFor(tc.typ)
		if err != nil {
			t.Fatal(err)
		}
		data, err := schemagen.Generate(tgt)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != string(tc.asset.Schema) {
			t.Fatalf("%s schema drift: run `go generate ./internal/brain` and commit the result", tc.asset.Touchpoint)
		}
	}
}

func TestBuildMessagePartition(t *testing.T) {
	msg := BuildMessage(T1Asset(), []byte(`{"forge":{}}`))
	s := string(msg)
	for _, marker := range []string{untrustedBegin, untrustedEnd, schemaBegin, schemaEnd, `{"forge":{}}`} {
		if !regexp.MustCompile(regexp.QuoteMeta(marker)).MatchString(s) {
			t.Fatalf("message missing %q", marker)
		}
	}
	// Deterministic for identical inputs.
	if DigestBytes(msg) != DigestBytes(BuildMessage(T1Asset(), []byte(`{"forge":{}}`))) {
		t.Fatal("message digest must be deterministic")
	}
}
