//go:build !linux

package launchworker

import "testing"

func enableTestChildSubreaper(t *testing.T) { t.Helper() }

func reapTestProcessGroup(t *testing.T, _ int) { t.Helper() }
