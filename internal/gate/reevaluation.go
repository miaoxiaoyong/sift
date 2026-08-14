package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/xsift/sift/internal/forge"
	"github.com/xsift/sift/internal/storage"
)

// ProduceReevaluation assembles and verifies Gate facts outside the
// transaction, runs the pure Gate function and returns the canonical
// GateReEvaluationResultV1 bytes (storage.md §8.1) for the claimed operation.
//
// It reuses the same Forge/Brain reads and policy assembly as the reconciler,
// but does not freeze a head, record an evaluation or emit any successor: those
// are owned by the storage terminal protocol. src is the frozen Run identity
// resolved by the worker.
//
// Fail-closed semantics: any Forge read failure becomes a closed `failed`
// (forge_read_failed) result union carrying the frozen evidence digest, and any
// Gate assembly/validation failure becomes a `failed`
// (gate_input_assembly_failed) result. The worker therefore never records a
// wrong verdict; the terminal protocol persists the frozen failure instead.
func (r *Reconciler) ProduceReevaluation(ctx context.Context, payload storage.GateReEvaluationPayload, src storage.GateCandidate) ([]byte, error) {
	now := r.now()
	if src.ChangeID == "" || src.ChangeID != payload.ChangeID {
		return gateReEvalFailed("gate_input_assembly_failed", map[string]string{"code": "schema_invalid", "field": "change_id"}), nil
	}
	ctx = forge.WithChargeKey(ctx, "gate-reeval:"+payload.RunID+":"+payload.ChangeID)
	change, err := r.Forge.GetChange(ctx, r.Project, payload.ChangeID)
	if err != nil {
		return gateReEvalReadFailed("get_change", err), nil
	}
	diff, err := r.Forge.GetChangeDiff(ctx, r.Project, payload.ChangeID)
	if err != nil {
		return gateReEvalReadFailed("get_change", err), nil
	}
	checks, err := r.Forge.GetChecks(ctx, r.Project, payload.HeadSHA)
	if err != nil {
		return gateReEvalReadFailed("get_checks", err), nil
	}
	paths := changedPaths(diff)
	if len(paths) == 0 {
		return gateReEvalFailed("gate_input_assembly_failed", map[string]string{"code": "paths_incomplete", "field": "change.changed_paths"}), nil
	}
	// The re-evaluation targets the frozen head. If the current Change head has
	// drifted, the input cannot be assembled for the frozen head; emit a closed
	// assembly failure. Automatic conflict→replacement (storage.md §8.1) at the
	// new head is deferred to a follow-up slice.
	if change.HeadSHA != payload.HeadSHA {
		return gateReEvalFailed("gate_input_assembly_failed", map[string]string{"code": "schema_invalid", "field": "head_sha"}), nil
	}
	in, err := r.input(ctx, src, change, paths, diff, checks, now)
	if err != nil {
		return gateReEvalAssemblyFailed(err), nil
	}
	inputCanon, inputHash, err := CanonicalInput(in)
	if err != nil {
		return gateReEvalAssemblyFailed(err), nil
	}
	v, err := Evaluate(in)
	if err != nil {
		return gateReEvalAssemblyFailed(err), nil
	}
	verdictCanon, err := canonical(v)
	if err != nil {
		return gateReEvalAssemblyFailed(err), nil
	}
	verdictDigest := digest(verdictCanon)
	body, err := storage.CanonicalJSON(map[string]any{
		"schema_version": 1,
		"kind":           "succeeded",
		"payload": map[string]any{
			"gate_input_json": string(inputCanon),
			"gate_input_hash": inputHash,
			"gate_version":    Version,
			"verdict_json":    string(verdictCanon),
			"verdict_digest":  verdictDigest,
		},
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// gateReEvalReadFailed classifies a Forge read error into a forge_read_failed
// result union with the frozen evidence digest.
func gateReEvalReadFailed(stage string, err error) []byte {
	class := "transient"
	var ce *forge.ClassifiedError
	if errors.As(err, &ce) {
		switch {
		case errors.Is(err, forge.ErrAuthOrCapability):
			class = "auth_or_capability"
		case errors.Is(err, forge.ErrRateLimited):
			class = "rate_limited"
		}
	}
	evidence := map[string]string{"stage": stage, "error_class": class, "evidence_digest": storage.SHA256Hex([]byte(err.Error()))}
	return gateReEvalFailed("forge_read_failed", evidence)
}

func gateReEvalAssemblyFailed(err error) []byte {
	var ae *AssemblyError
	code, field := "schema_invalid", "unknown"
	if errors.As(err, &ae) {
		code, field = ae.Code, ae.Field
	}
	return gateReEvalFailed("gate_input_assembly_failed", map[string]string{"code": code, "field": field})
}

func gateReEvalFailed(class string, evidence map[string]string) []byte {
	evCanon, err := canonical(evidence)
	if err != nil {
		evCanon = []byte(fmt.Sprintf(`{"code":"schema_invalid","field":%q}`, class))
	}
	body, err := storage.CanonicalJSON(map[string]any{
		"schema_version": 1,
		"kind":           "failed",
		"payload": map[string]any{
			"failure_class":    class,
			"failure_evidence": json.RawMessage(evCanon),
		},
	})
	if err != nil {
		return []byte(`{"kind":"failed","payload":{"failure_class":"gate_contract_failed","failure_evidence":{"code":"verdict_schema_invalid"}},"schema_version":1}`)
	}
	return body
}
