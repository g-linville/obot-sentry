//go:build windows

package datadir

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func machineBaseDir() (string, error) {
	path, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return "", fmt.Errorf("resolving ProgramData known folder: %w", err)
	}
	if !filepath.IsAbs(path) || strings.HasPrefix(filepath.Clean(path), `\\`) {
		return "", fmt.Errorf("ProgramData %q is not an absolute local path", path)
	}
	volume := filepath.VolumeName(path)
	if len(volume) != 2 || volume[1] != ':' {
		return "", fmt.Errorf("ProgramData %q has no local drive-letter volume", path)
	}
	root, err := windows.UTF16PtrFromString(volume + `\`)
	if err != nil {
		return "", fmt.Errorf("encoding ProgramData volume root: %w", err)
	}
	if driveType := windows.GetDriveType(root); driveType != windows.DRIVE_FIXED {
		return "", fmt.Errorf("ProgramData %q is on drive type %d, want fixed drive type %d", path, driveType, windows.DRIVE_FIXED)
	}
	return path, nil
}
