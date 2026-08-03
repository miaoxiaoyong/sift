//go:build !sift_test

package wrapper

func pauseForTest(string) error          { return nil }
func dumpForTest(string, map[string]any) {}
