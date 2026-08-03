//go:build !darwin

package runtime

func darwinSystemExecutable(string) bool { return false }
