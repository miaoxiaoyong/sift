// Package replay re-runs exported M4 replay records without any live IO.
package replay

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/miaoxiaoyong/sift/internal/brain"
	"github.com/miaoxiaoyong/sift/internal/gate"
)

type GateDelta struct {
	RecordID string
	Expected gate.Verdict
	Actual   gate.Verdict
}

// GateReport quantifies decision movement. These are candidate leak/false-block
// changes; a true error rate additionally needs later human-decision labels.
type GateReport struct {
	Records, Unchanged         int
	AllowToBlock, BlockToAllow int
	InconclusiveChanges        int
	Deltas                     []GateDelta
}

// ReplayGateJSONL feeds each frozen Gate input back into the production pure
// Gate function. It rejects malformed records instead of consulting live data.
func ReplayGateJSONL(r io.Reader) (GateReport, error) {
	var report GateReport
	return report, each(r, func(raw json.RawMessage) error {
		var header struct {
			RecordType string `json:"record_type"`
			RecordID   string `json:"record_id"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return err
		}
		if header.RecordType != "gate" {
			return nil
		}
		var record struct {
			Input    json.RawMessage `json:"input"`
			Expected json.RawMessage `json:"expected_verdict"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		var in gate.Input
		var expected gate.Verdict
		if err := json.Unmarshal(record.Input, &in); err != nil {
			return fmt.Errorf("gate %s input: %w", header.RecordID, err)
		}
		if err := json.Unmarshal(record.Expected, &expected); err != nil {
			return fmt.Errorf("gate %s expected verdict: %w", header.RecordID, err)
		}
		actual, err := gate.Evaluate(in)
		if err != nil {
			return fmt.Errorf("gate %s replay: %w", header.RecordID, err)
		}
		report.Records++
		if sameVerdict(expected, actual) {
			report.Unchanged++
			return nil
		}
		report.Deltas = append(report.Deltas, GateDelta{RecordID: header.RecordID, Expected: expected, Actual: actual})
		before, after := gate.ShadowDecision(expected), gate.ShadowDecision(actual)
		switch {
		case before == "allow" && after == "block":
			report.AllowToBlock++
		case before == "block" && after == "allow":
			report.BlockToAllow++
		default:
			report.InconclusiveChanges++
		}
		return nil
	})
}

type BrainReport struct{ Records, Validated, Fallbacks int }

// ReplayBrainJSONL reruns the same closed decoder/validator against recorded
// provider output. It never invokes a provider; fallback calls remain the
// recorded deterministic fallback path.
func ReplayBrainJSONL(r io.Reader, contracts map[string]brain.TouchpointContract) (BrainReport, error) {
	var report BrainReport
	return report, each(r, func(raw json.RawMessage) error {
		var header struct {
			RecordType string `json:"record_type"`
			RecordID   string `json:"record_id"`
			Touchpoint string `json:"touchpoint"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return err
		}
		if header.RecordType != "brain_call" {
			return nil
		}
		var record struct {
			PromptVersion       string          `json:"prompt_version"`
			OutputSchemaVersion int             `json:"output_schema_version"`
			Status              string          `json:"status"`
			Selected            *int            `json:"selected_attempt_no"`
			Validated           json.RawMessage `json:"validated_output"`
			Attempts            []struct {
				Number int     `json:"provider_attempt"`
				Raw    *string `json:"raw_output"`
			} `json:"attempts"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		tp, ok := contracts[header.Touchpoint]
		if !ok {
			return fmt.Errorf("brain %s: no contract for %s", header.RecordID, header.Touchpoint)
		}
		if tp.Asset.PromptVersion != record.PromptVersion || tp.Asset.OutputSchemaVersion != record.OutputSchemaVersion {
			return fmt.Errorf("brain %s: recorded prompt or output schema is unavailable", header.RecordID)
		}
		report.Records++
		if record.Status == "fallback" {
			report.Fallbacks++
			return nil
		}
		if record.Status != "valid" || record.Selected == nil {
			return fmt.Errorf("brain %s: invalid terminal trace", header.RecordID)
		}
		var rawOutput *string
		for _, attempt := range record.Attempts {
			if attempt.Number == *record.Selected {
				rawOutput = attempt.Raw
				break
			}
		}
		if rawOutput == nil {
			return fmt.Errorf("brain %s: selected attempt output missing", header.RecordID)
		}
		result, _, _, err := brain.ParseEnvelope([]byte(*rawOutput))
		if err != nil {
			return fmt.Errorf("brain %s: replay envelope: %w", header.RecordID, err)
		}
		actual, err := tp.ValidateOutput(result)
		if err != nil {
			return fmt.Errorf("brain %s: replay validation: %w", header.RecordID, err)
		}
		if !bytes.Equal(actual, record.Validated) {
			return fmt.Errorf("brain %s: validated output drift", header.RecordID)
		}
		report.Validated++
		return nil
	})
}

func each(r io.Reader, fn func(json.RawMessage) error) error {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for s.Scan() {
		if len(bytes.TrimSpace(s.Bytes())) == 0 {
			continue
		}
		if err := fn(append(json.RawMessage(nil), s.Bytes()...)); err != nil {
			return err
		}
	}
	return s.Err()
}
func sameVerdict(a, b gate.Verdict) bool {
	aJSON, _ := json.Marshal(a)
	bJSON, _ := json.Marshal(b)
	return bytes.Equal(aJSON, bJSON)
}
