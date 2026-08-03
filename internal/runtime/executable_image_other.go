//go:build !linux && !darwin

package runtime

import (
	"fmt"
	"os"
)

func ReleaseExecutableImage(image *os.File) {
	if image != nil {
		_ = image.Close()
	}
}

func executableImagePath(*os.File) (string, error) {
	return "", fmt.Errorf("runtime: executing a sealed executable image is unsupported on this platform")
}
