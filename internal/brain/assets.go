//go:generate go run ./genschemas

package brain

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"

	"github.com/miaoxiaoyong/sift/internal/decode"
)

// Prompt assets are versioned, git-committed files embedded into the binary
// (brain.md §2). The runtime never re-reads them from disk. Each touchpoint
// pairs a prompt (v<N>.md) with its generated output schema (v<N>.schema.json,
// produced from the contract structs by genschemas — never a hand-written
// second copy).

// ProtocolClaudeJSONV1 is the V0 provider envelope protocol (brain.md §4).
const ProtocolClaudeJSONV1 = "claude-json-v1"

//go:embed prompts/T1/v1.md prompts/T1/v1.schema.json prompts/T2/v1.md prompts/T2/v1.schema.json prompts/T3/v1.md prompts/T3/v1.schema.json prompts/T5/v1.md prompts/T5/v1.schema.json
var promptFS embed.FS

// PromptAsset is one loaded prompt/schema pair with its derived versions.
type PromptAsset struct {
	Touchpoint string
	Version    int
	Prompt     []byte
	Schema     []byte
	// PromptVersion is `<touchpoint>/v<integer>/<sha256-12>` over the prompt
	// UTF-8 bytes, the canonical output schema JSON and the protocol version
	// (brain.md §2). Changing any of the three yields a new value.
	PromptVersion string
	// OutputSchemaVersion is the integer schema version, bumped only on
	// structural schema changes.
	OutputSchemaVersion int
}

// T1Asset loads the T1 v1 prompt asset.
func T1Asset() PromptAsset { return mustAsset("T1", 1) }

// T2Asset loads the T2 v1 prompt asset.
func T2Asset() PromptAsset { return mustAsset("T2", 1) }

// T3Asset loads the T3 v1 prompt asset.
func T3Asset() PromptAsset { return mustAsset("T3", 1) }

// T5Asset loads the T5 v1 prompt asset.
func T5Asset() PromptAsset { return mustAsset("T5", 1) }

func mustAsset(touchpoint string, version int) PromptAsset {
	prompt, err := promptFS.ReadFile(fmt.Sprintf("prompts/%s/v%d.md", touchpoint, version))
	if err != nil {
		panic(fmt.Sprintf("brain: embedded prompt %s/v%d: %v", touchpoint, version, err))
	}
	schema, err := promptFS.ReadFile(fmt.Sprintf("prompts/%s/v%d.schema.json", touchpoint, version))
	if err != nil {
		panic(fmt.Sprintf("brain: embedded schema %s/v%d: %v", touchpoint, version, err))
	}
	canonicalSchema, err := decode.Canonical(schemaTreeFor(schema))
	if err != nil {
		panic(fmt.Sprintf("brain: canonical schema %s/v%d: %v", touchpoint, version, err))
	}
	h := sha256.New()
	h.Write(prompt)
	h.Write([]byte{0})
	h.Write(canonicalSchema)
	h.Write([]byte{0})
	h.Write([]byte(ProtocolClaudeJSONV1))
	return PromptAsset{
		Touchpoint:          touchpoint,
		Version:             version,
		Prompt:              prompt,
		Schema:              schema,
		PromptVersion:       fmt.Sprintf("%s/v%d/%s", touchpoint, version, hex.EncodeToString(h.Sum(nil))[:12]),
		OutputSchemaVersion: version,
	}
}

func schemaTreeFor(schema []byte) any {
	var tree any
	if err := decode.Decode(schema, &tree, decode.OpenEnvelope); err != nil {
		panic(fmt.Sprintf("brain: parse embedded schema: %v", err))
	}
	return tree
}

// Untrusted input / output-schema section markers referenced by every prompt
// asset (brain.md §2 fixed prompt partition).
const (
	untrustedBegin = "--- UNTRUSTED INPUT BEGIN ---"
	untrustedEnd   = "--- UNTRUSTED INPUT END ---"
	schemaBegin    = "--- OUTPUT SCHEMA BEGIN ---"
	schemaEnd      = "--- OUTPUT SCHEMA END ---"
)

// BuildMessage assembles the exact provider request bytes for a call:
// prompt → untrusted input delimiters → input canonical JSON → output schema
// (brain.md §2). Both physical attempts of a retry send these bytes
// unchanged; the frozen input_digest covers them.
func BuildMessage(asset PromptAsset, inputCanonical []byte) []byte {
	var b []byte
	b = append(b, asset.Prompt...)
	b = append(b, '\n')
	b = append(b, untrustedBegin...)
	b = append(b, '\n')
	b = append(b, inputCanonical...)
	b = append(b, '\n')
	b = append(b, untrustedEnd...)
	b = append(b, "\n\n"...)
	b = append(b, schemaBegin...)
	b = append(b, '\n')
	b = append(b, asset.Schema...)
	b = append(b, schemaEnd...)
	b = append(b, '\n')
	return b
}

// DigestBytes is the sha256 lowercase-hex digest used for input/request/raw
// output digests.
func DigestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
