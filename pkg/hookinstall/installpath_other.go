//go:build !windows

package hookinstall

func validateInstallHomePath(string) error    { return nil }
func validateInstallMachinePath(string) error { return nil }
