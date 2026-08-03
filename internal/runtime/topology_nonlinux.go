//go:build !linux

package runtime

import "errors"

func observeTopology(int, int, int) (ProcessTopologyObservation, error) {
	return ProcessTopologyObservation{}, errors.New("runtime: topology observation unavailable")
}
