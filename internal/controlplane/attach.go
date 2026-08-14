package controlplane

import (
	"context"
	"errors"

	"github.com/xsift/sift/internal/runtime"
	"github.com/xsift/sift/internal/storage"
)

type attachParams struct {
	RunID string `json:"run_id"`
}

type attachResult struct {
	RunID       string `json:"run_id"`
	AttemptNo   int    `json:"attempt_no"`
	Generation  int    `json:"generation"`
	Backend     string `json:"backend"`
	SessionName string `json:"session_name"`
}

func (s *Server) handleOpsAttach(req Request) Response {
	if !onlyKeys(req.Params, "run_id") {
		return failure(req.RequestID, "invalid_request", "invalid params", false)
	}
	var p attachParams
	if !decodeParams(req.Params, &p) || p.RunID == "" {
		return failure(req.RequestID, "invalid_request", "invalid params", false)
	}
	if s.db == nil {
		return failure(req.RequestID, "unavailable", "runtime observation is unavailable", true)
	}
	target, err := s.db.AttachTargetForRun(context.Background(), p.RunID)
	if errors.Is(err, storage.ErrAttachRunNotFound) {
		return failure(req.RequestID, "not_found", "run not found", false)
	}
	if err != nil {
		return failure(req.RequestID, "conflict", "run cannot be attached", false)
	}
	if target.Backend != "tmux" || s.tmuxPath == "" || s.tmuxSocketPath == "" {
		return failure(req.RequestID, "conflict", "run is not attachable", false)
	}
	name, err := runtime.TmuxSessionName(target.RunID, target.AttemptNo, target.Generation, target.DispatchID)
	if err != nil {
		return failure(req.RequestID, "conflict", "run launch identity is incomplete", false)
	}
	digest := name[len("sift-"):]
	observe := s.tmuxObserver
	if observe == nil {
		observe = runtime.ObserveTmuxSession
	}
	if err := observe(context.Background(), s.tmuxPath, s.tmuxSocketPath, name, digest); err != nil {
		return failure(req.RequestID, "conflict", "tmux session is not attachable", false)
	}
	// The external observation has no transaction with durable state. Re-read
	// the exact target before returning it so recovery cannot hand the CLI a
	// session derived from a superseded generation or dispatch.
	current, err := s.db.AttachTargetForRun(context.Background(), p.RunID)
	if err != nil || current != target {
		return failure(req.RequestID, "conflict", "run launch identity changed during observation", false)
	}
	return success(req.RequestID, attachResult{RunID: target.RunID, AttemptNo: target.AttemptNo, Generation: target.Generation, Backend: target.Backend, SessionName: name})
}
