//go:build windows

package hookinstall

import (
	"strings"
	"testing"
)

func TestValidateLocalFixedPathRejectsNetworkNamespaces(t *testing.T) {
	for _, path := range []string{
		`\\server\profiles\alice`,
		`\\.\pipe\obot-sentry-test`,
		`\\?\GLOBALROOT\Device\HarddiskVolume1\ProgramData`,
	} {
		t.Run(strings.ReplaceAll(path, `\`, "_"), func(t *testing.T) {
			if err := validateLocalFixedPath(path); err == nil {
				t.Fatalf("validateLocalFixedPath(%q) succeeded", path)
			}
		})
	}
}

func TestWindowsProgramDataDoesNotTrustEnvironment(t *testing.T) {
	malicious := `\\attacker.example\share`
	t.Setenv("ProgramData", malicious)
	got := windowsProgramData()
	if strings.EqualFold(got, malicious) {
		t.Fatalf("windowsProgramData trusted inherited environment path %q", got)
	}
	if err := validateLocalFixedPath(got); err != nil {
		t.Fatalf("known ProgramData path %q is not local and fixed: %v", got, err)
	}
}
