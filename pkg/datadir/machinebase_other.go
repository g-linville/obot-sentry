//go:build !darwin && !windows

package datadir

func machineBaseDir() (string, error) {
	return "/var/lib", nil
}
