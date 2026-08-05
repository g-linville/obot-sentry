package enforce

import (
	"errors"
	"os"
	"path/filepath"
)

var errConfigPathNotAbsolute = errors.New("configuration path is not absolute")

// openConfigFile requires callers to resolve every configuration location to
// an absolute path before any filesystem access. Platform implementations then
// open that path without a separate pathname preflight.
func openConfigFile(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, &os.PathError{Op: "open", Path: path, Err: errConfigPathNotAbsolute}
	}
	return openConfigFilePlatform(filepath.Clean(path))
}
