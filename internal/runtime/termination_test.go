package runtime

import (
	"context"
	"reflect"
	"syscall"
	"testing"
	"time"
)

type observations []ProcessObservation

func (o *observations) Observe(context.Context, int) (ProcessObservation, error) {
	v := (*o)[0]
	if len(*o) > 1 {
		*o = (*o)[1:]
	}
	return v, nil
}

type signals []syscall.Signal

func (s *signals) SignalGroup(_ int, signal syscall.Signal) error {
	*s = append(*s, signal)
	return nil
}

func terminationIdentity() ProcessIdentity {
	return ProcessIdentity{PID: 12, PGID: 12, StartedAtMS: 99, Executable: "/agent", ControlNonceHash: "nonce-hash"}
}
func terminationConfig() TerminationConfig {
	return TerminationConfig{TermGrace: time.Second, KillGrace: time.Second, AbsenceRechecks: 2, RecheckInterval: time.Second}
}

func TestUnixProcessSignalerRejectsInvalidGroup(t *testing.T) {
	if err := (UnixProcessSignaler{}).SignalGroup(0, syscall.SIGTERM); err != ErrInvalidTerminationConfig {
		t.Fatalf("invalid group = %v", err)
	}
}

func TestTerminatorSignalsOnlyVerifiedIdentityAndProvesAbsence(t *testing.T) {
	id := terminationIdentity()
	obs := observations{{Exists: true, ProcessIdentity: id}, {Exists: false}}
	var got signals
	result, err := (Terminator{Inspector: &obs, Signaler: &got, Sleep: func(context.Context, time.Duration) error { return nil }}).Terminate(context.Background(), id, terminationConfig())
	if err != nil || !result.Absent || result.Cause != TerminationAbsent {
		t.Fatalf("result = %#v, %v", result, err)
	}
	if want := []syscall.Signal{syscall.SIGTERM}; !reflect.DeepEqual([]syscall.Signal(got), want) {
		t.Fatalf("signals = %v, want %v", got, want)
	}
}

func TestTerminatorNeverSignalsReusedOrUncertainPID(t *testing.T) {
	id := terminationIdentity()
	obs := observations{{Exists: true, ProcessIdentity: ProcessIdentity{PID: id.PID, PGID: id.PGID, StartedAtMS: id.StartedAtMS + 1, Executable: id.Executable, ControlNonceHash: id.ControlNonceHash}}}
	var got signals
	result, err := (Terminator{Inspector: &obs, Signaler: &got, Sleep: func(context.Context, time.Duration) error { return nil }}).Terminate(context.Background(), id, terminationConfig())
	if err != nil || result.Cause != TerminationIdentityUnknown || result.Absent {
		t.Fatalf("result = %#v, %v", result, err)
	}
	if len(got) != 0 {
		t.Fatalf("uncertain identity signalled: %v", got)
	}
}

func TestTerminatorEscalatesAndFailsClosedWhenGroupRemains(t *testing.T) {
	id := terminationIdentity()
	obs := observations{{Exists: true, ProcessIdentity: id}, {Exists: true, ProcessIdentity: id}, {Exists: true, ProcessIdentity: id}, {Exists: true, ProcessIdentity: id}, {Exists: true, ProcessIdentity: id}}
	var got signals
	result, err := (Terminator{Inspector: &obs, Signaler: &got, Sleep: func(context.Context, time.Duration) error { return nil }}).Terminate(context.Background(), id, terminationConfig())
	if err != nil || result.Cause != TerminationUnconfirmed || result.Absent {
		t.Fatalf("result = %#v, %v", result, err)
	}
	if want := []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}; !reflect.DeepEqual([]syscall.Signal(got), want) {
		t.Fatalf("signals = %v, want %v", got, want)
	}
}
