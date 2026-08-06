package forge

import (
	"context"
	"encoding/json"
)

func (a *Adapter) MergeChange(ctx context.Context, p ProjectRef, id, expected, method string) (Change, error) {
	if !a.AutoMergeSupported(p) {
		return Change{}, &ClassifiedError{Class: ErrAuthOrCapability, Summary: "capability_unsupported: expected-head CAS is unproven"}
	}
	if a.capabilities != nil {
		enabled, err := a.capabilities.AutoMergeEnabled(ctx, p)
		if err != nil || !enabled {
			return Change{}, &ClassifiedError{Class: ErrAuthOrCapability, Summary: "capability_unsupported: persisted auto_merge capability is unavailable"}
		}
	}
	if id == "" || expected == "" {
		return Change{}, &ClassifiedError{Class: ErrContractViolation, Summary: "change id and expected head sha are required"}
	}
	if method != "merge" {
		return Change{}, &ClassifiedError{Class: ErrContractViolation, Summary: "only merge method is supported"}
	}
	path := a.base(p) + "/pulls/" + pathPart(id) + "/merge"
	payload := map[string]string{"sha": expected, "merge_method": "merge"}
	if a.Kind == KindGitLab {
		path = a.base(p) + "/merge_requests/" + pathPart(id) + "/merge"
		// GitLab has no merge_method equivalent. Its project configuration
		// selects the strategy, but sha remains the required CAS field.
		payload = map[string]string{"sha": expected}
	}
	in, _ := json.Marshal(payload)
	var response json.RawMessage
	if e := a.call(ctx, p, path, "PUT", in, &response); e != nil {
		if unsupportedCAS(e) {
			a.disableAutoMerge(p)
		}
		return Change{}, e
	}
	// GitHub's merge response is not a pull-request projection. Re-read the
	// Change rather than accepting a successful transport response as merge
	// evidence; this also gives both platforms the same neutral result.
	c, e := a.getChange(ctx, p, id, false)
	if e != nil {
		return Change{}, e
	}
	if c.State != ChangeMerged {
		return Change{}, &ClassifiedError{Class: ErrSemanticConflict, Summary: "merge response did not produce a merged change"}
	}
	return c, nil
}
