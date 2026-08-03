package runtime

import "errors"

// ProcessTopologyIdentity is the procfs identity needed to prove the fixture
// topology. Parentage and process-group membership are observed together;
// neither a PID nor a one-shot kill probe is qualification evidence.
type ProcessTopologyIdentity struct {
	PID  int
	PPID int
	PGID int
}

// ProcessTopologyObservation is a snapshot of wrapper -> Agent -> descendant.
type ProcessTopologyObservation struct {
	Wrapper    ProcessTopologyIdentity
	Agent      ProcessTopologyIdentity
	Descendant ProcessTopologyIdentity
}

// ObserveTopology reads the platform process table for the exact three
// processes. Linux uses procfs; unsupported platforms fail closed.
func ObserveTopology(wrapperPID, agentPID, descendantPID int) (ProcessTopologyObservation, error) {
	if wrapperPID <= 0 || agentPID <= 0 || descendantPID <= 0 {
		return ProcessTopologyObservation{}, errors.New("runtime: incomplete topology identity")
	}
	return observeTopology(wrapperPID, agentPID, descendantPID)
}

// ClassifyDetachedDescendant turns the observed topology into the only safe
// qualification result for a child which left the wrapper process group.
func ClassifyDetachedDescendant(o ProcessTopologyObservation) (QualificationEvidence, error) {
	if o.Wrapper.PID <= 0 || o.Agent.PID <= 0 || o.Descendant.PID <= 0 ||
		o.Agent.PPID != o.Wrapper.PID || o.Agent.PGID != o.Wrapper.PGID ||
		o.Descendant.PPID != o.Agent.PID || o.Descendant.PGID == o.Wrapper.PGID {
		return QualificationEvidence{}, errors.New("runtime: topology is not a detached wrapper descendant")
	}
	return QualificationEvidence{Status: ProcessGroupUnverified, Reason: "detached_descendant"}, nil
}
