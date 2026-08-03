//go:build !darwin && !windows

package hookinstall

import "os"

func checkPrivilege() error {
	return errUnsupportedPlatform
}

func resolveTargetUser() (*TargetUser, error) {
	return nil, errUnsupportedPlatform
}

func validateExecutableOwner(string, os.FileInfo) error { return nil }
