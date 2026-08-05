//go:build !darwin && !linux && !windows

package enforce

import "os"

// obot-sentry is deployed only on macOS and Windows. Keep unsupported targets
// buildable without claiming the platform-specific non-blocking guarantees.
func openConfigFilePlatform(path string) (*os.File, error) {
	return os.Open(path)
}
