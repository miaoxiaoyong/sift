package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/miaoxiaoyong/sift/internal/storage"
)

type wrapperIdentityParams struct {
	PID         int64  `json:"pid"`
	StartedAtMS int64  `json:"started_at_ms"`
	Executable  string `json:"executable"`
	PGID        int64  `json:"pgid"`
}
type agentIdentityParams struct {
	PID         int64  `json:"pid"`
	StartedAtMS int64  `json:"started_at_ms"`
	Executable  string `json:"executable"`
}
type acquireParams struct {
	RunID             string                `json:"run_id"`
	AttemptNo         int                   `json:"attempt_no"`
	Generation        int                   `json:"generation"`
	DispatchID        string                `json:"dispatch_id"`
	WrapperInstanceID string                `json:"wrapper_instance_id"`
	SessionCandidate  string                `json:"session_candidate"`
	WrapperIdentity   wrapperIdentityParams `json:"wrapper_identity"`
}
type permitParams struct {
	RunID             string                `json:"run_id"`
	AttemptNo         int                   `json:"attempt_no"`
	Generation        int                   `json:"generation"`
	WrapperInstanceID string                `json:"wrapper_instance_id"`
	WrapperIdentity   wrapperIdentityParams `json:"wrapper_identity"`
	ControlDigest     string                `json:"control_digest"`
	ControlNonceHash  string                `json:"control_nonce_hash"`
	PermitCandidate   string                `json:"permit_candidate"`
}
type startedParams struct {
	RunID             string              `json:"run_id"`
	AttemptNo         int                 `json:"attempt_no"`
	Generation        int                 `json:"generation"`
	WrapperInstanceID string              `json:"wrapper_instance_id"`
	AgentIdentity     agentIdentityParams `json:"agent_identity"`
	ControlDigest     string              `json:"control_digest"`
	ResultDigest      *string             `json:"result_digest"`
}

func decodeParams(v map[string]any, out any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(out) == nil
}

func (s *Server) handoffRequest(req Request) Response {
	if s.db == nil {
		return failure(req.RequestID, "unauthorized", "credential rejected", false)
	}
	now := time.Now().UnixMilli()
	switch req.Method {
	case "claim.acquire":
		if req.Auth.Kind != "bootstrap" || req.Auth.Token != "" || req.Auth.Session != "" || req.Auth.Permit != "" || !validToken(req.Auth.Nonce) {
			return failure(req.RequestID, "unauthorized", "credential rejected", false)
		}
		var p acquireParams
		if !onlyKeys(req.Params, "run_id", "attempt_no", "generation", "dispatch_id", "wrapper_instance_id", "session_candidate", "wrapper_identity") || !decodeParams(req.Params, &p) || !validToken(p.SessionCandidate) {
			return failure(req.RequestID, "invalid_request", "invalid params", false)
		}
		err := s.db.AcquireLaunchClaim(contextBackground(), storage.AcquireLaunchClaim{RunID: p.RunID, AttemptNo: p.AttemptNo, Generation: p.Generation, DispatchID: p.DispatchID, BootstrapNonce: req.Auth.Nonce, InstanceID: p.WrapperInstanceID, Session: p.SessionCandidate, Wrapper: toWrapper(p.WrapperIdentity), NowMS: now})
		if err != nil {
			return s.handoffFailure(req.RequestID, p.RunID, p.AttemptNo, req.Method, err)
		}
		return success(req.RequestID, map[string]any{"disposition": "acquired"})
	case "claim.permit_spawn":
		if req.Auth.Kind != "wrapper_session" || req.Auth.Token != "" || req.Auth.Nonce != "" || req.Auth.Permit != "" || !validToken(req.Auth.Session) {
			return failure(req.RequestID, "unauthorized", "credential rejected", false)
		}
		var p permitParams
		if !onlyKeys(req.Params, "run_id", "attempt_no", "generation", "wrapper_instance_id", "wrapper_identity", "control_digest", "control_nonce_hash", "permit_candidate") || !decodeParams(req.Params, &p) || !validToken(p.PermitCandidate) || !validToken(p.ControlNonceHash) {
			return failure(req.RequestID, "invalid_request", "invalid params", false)
		}
		err := s.db.PermitSpawn(contextBackground(), storage.PermitSpawn{RunID: p.RunID, AttemptNo: p.AttemptNo, Generation: p.Generation, InstanceID: p.WrapperInstanceID, Session: req.Auth.Session, Permit: p.PermitCandidate, ControlDigest: p.ControlDigest, ControlNonceHash: p.ControlNonceHash, Wrapper: toWrapper(p.WrapperIdentity), NowMS: now})
		if err != nil {
			return s.handoffFailure(req.RequestID, p.RunID, p.AttemptNo, req.Method, err)
		}
		return success(req.RequestID, map[string]any{"disposition": "permitted"})
	case "claim.started":
		if req.Auth.Kind != "wrapper_started" || req.Auth.Token != "" || req.Auth.Nonce != "" || !validToken(req.Auth.Session) || !validToken(req.Auth.Permit) {
			return failure(req.RequestID, "unauthorized", "credential rejected", false)
		}
		var p startedParams
		if !onlyKeys(req.Params, "run_id", "attempt_no", "generation", "wrapper_instance_id", "agent_identity", "control_digest", "result_digest") || !decodeParams(req.Params, &p) {
			return failure(req.RequestID, "invalid_request", "invalid params", false)
		}
		result := ""
		if p.ResultDigest != nil {
			result = *p.ResultDigest
		}
		disposition, err := s.db.ConfirmStarted(contextBackground(), storage.StartedClaim{RunID: p.RunID, AttemptNo: p.AttemptNo, Generation: p.Generation, InstanceID: p.WrapperInstanceID, Session: req.Auth.Session, Permit: req.Auth.Permit, ControlDigest: p.ControlDigest, ResultDigest: result, Agent: storage.AgentIdentity{PID: p.AgentIdentity.PID, StartedAtMS: p.AgentIdentity.StartedAtMS, Executable: p.AgentIdentity.Executable}, NowMS: now})
		if err != nil {
			return s.handoffFailure(req.RequestID, p.RunID, p.AttemptNo, req.Method, err)
		}
		return success(req.RequestID, map[string]any{"disposition": disposition})
	}
	return failure(req.RequestID, "unknown_method", "unknown method", false)
}
func toWrapper(p wrapperIdentityParams) storage.WrapperIdentity {
	return storage.WrapperIdentity{PID: p.PID, StartedAtMS: p.StartedAtMS, Executable: p.Executable, PGID: p.PGID}
}
func contextBackground() context.Context { return context.Background() }
func (s *Server) handoffFailure(id, runID string, attemptNo int, method string, err error) Response {
	if s.db != nil {
		disposition := "conflict"
		if errors.Is(err, storage.ErrHandoffStale) {
			disposition = "stale"
		} else if errors.Is(err, storage.ErrHandoffUnauthorized) {
			disposition = "unauthorized"
		}
		_ = s.db.RecordHandoffSecurityEvent(contextBackground(), runID, attemptNo, method, disposition, time.Now().UnixMilli())
	}
	switch {
	case errors.Is(err, storage.ErrHandoffUnauthorized):
		return failure(id, "unauthorized", "credential rejected", false)
	case errors.Is(err, storage.ErrHandoffStale):
		return failure(id, "stale", "handoff is stale", false)
	case errors.Is(err, storage.ErrHandoffConflict):
		return failure(id, "conflict", "handoff conflicts with current state", false)
	default:
		return failure(id, "internal", "handoff failed", true)
	}
}
