package brain

import (
	"regexp"
	"testing"
)

func TestPromptAssetsVersioning(t *testing.T) {
	for _, asset := range []PromptAsset{T1Asset(), T2Asset(), T3Asset(), T4Asset(), T5Asset(), T6Asset(), T7Asset()} {
		if len(asset.Prompt) == 0 || len(asset.Schema) == 0 {
			t.Fatalf("%s: embedded assets empty", asset.Touchpoint)
		}
		pat := regexp.MustCompile(`^T[1-7]/v1/[0-9a-f]{12}$`)
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
