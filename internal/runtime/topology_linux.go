//go:build linux

package runtime

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func observeTopology(wrapperPID, agentPID, descendantPID int) (ProcessTopologyObservation, error) {
	read := func(pid int) (ProcessTopologyIdentity, error) {
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return ProcessTopologyIdentity{}, fmt.Errorf("runtime: read topology process %d: %w", pid, err)
		}
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 {
			return ProcessTopologyIdentity{}, fmt.Errorf("runtime: malformed topology process %d", pid)
		}
		fields := strings.Fields(string(data[end+1:]))
		// After comm, stat fields begin at state (field 3): ppid is
		// fields[1], and pgrp is fields[2].
		if len(fields) < 3 {
			return ProcessTopologyIdentity{}, fmt.Errorf("runtime: short topology process %d", pid)
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			return ProcessTopologyIdentity{}, fmt.Errorf("runtime: parse topology parent %d: %w", pid, err)
		}
		pgid, err := strconv.Atoi(fields[2])
		if err != nil {
			return ProcessTopologyIdentity{}, fmt.Errorf("runtime: parse topology group %d: %w", pid, err)
		}
		return ProcessTopologyIdentity{PID: pid, PPID: ppid, PGID: pgid}, nil
	}
	wrapper, err := read(wrapperPID)
	if err != nil {
		return ProcessTopologyObservation{}, err
	}
	agent, err := read(agentPID)
	if err != nil {
		return ProcessTopologyObservation{}, err
	}
	descendant, err := read(descendantPID)
	if err != nil {
		return ProcessTopologyObservation{}, err
	}
	return ProcessTopologyObservation{Wrapper: wrapper, Agent: agent, Descendant: descendant}, nil
}
