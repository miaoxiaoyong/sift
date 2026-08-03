//go:build !linux

package wrapper

func enableChildSubreaper() error { return nil }
func reapExitedChildren() error   { return nil }
