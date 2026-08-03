//go:build darwin

package datadir

func machineBaseDir() (string, error) {
	return "/Library/Application Support", nil
}
