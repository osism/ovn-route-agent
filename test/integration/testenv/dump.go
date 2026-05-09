//go:build integration

package testenv

import (
	"os/exec"
	"strings"
	"testing"
)

// DumpHostState logs the host-side state most relevant to diagnosing a
// scenario failure: the agent's nft ruleset, /4 and /6 routes on the test
// bridge, OVS flows tagged with the agent's cookies, and FRR static routes
// in the test VRF for both address families.
//
// Output goes to t.Logf so it never compounds an existing failure into a
// fresh one. Best-effort — commands that fail (binary missing, no rules
// installed, etc.) log the error alongside the empty-or-error output.
func DumpHostState(t *testing.T) {
	t.Helper()

	runs := []struct {
		name string
		argv []string
	}{
		{"nft list ruleset", []string{"nft", "list", "ruleset"}},
		{"ip -4 route show dev " + DefaultBridgeDev, []string{"ip", "-4", "route", "show", "dev", DefaultBridgeDev}},
		{"ip -6 route show dev " + DefaultBridgeDev, []string{"ip", "-6", "route", "show", "dev", DefaultBridgeDev}},
		{"ovs-ofctl dump-flows " + DefaultBridgeDev + " cookie=0x999/-1", []string{"ovs-ofctl", "dump-flows", DefaultBridgeDev, "cookie=0x999/-1"}},
		{"ovs-ofctl dump-flows " + DefaultBridgeDev + " cookie=0x998/-1", []string{"ovs-ofctl", "dump-flows", DefaultBridgeDev, "cookie=0x998/-1"}},
		{"vtysh show ip route vrf " + DefaultVRFName + " static", []string{"vtysh", "-c", "show ip route vrf " + DefaultVRFName + " static"}},
		{"vtysh show ipv6 route vrf " + DefaultVRFName + " static", []string{"vtysh", "-c", "show ipv6 route vrf " + DefaultVRFName + " static"}},
	}

	t.Logf("=== host state dump ===")
	for _, r := range runs {
		out, err := exec.Command(r.argv[0], r.argv[1:]...).CombinedOutput()
		text := strings.TrimRight(string(out), "\n")
		switch {
		case err != nil:
			t.Logf("--- %s --- (err: %v)\n%s", r.name, err, text)
		case strings.TrimSpace(text) == "":
			t.Logf("--- %s --- (empty)", r.name)
		default:
			t.Logf("--- %s ---\n%s", r.name, text)
		}
	}
	t.Logf("=== end host state dump ===")
}

// RegisterFailureDump registers a t.Cleanup that calls DumpHostState only
// when the test has failed by the time the cleanup runs. Tests that pass pay
// nothing; failing tests get a single self-contained snapshot in their log
// output instead of forcing an operator to ssh in and re-run commands by
// hand.
func RegisterFailureDump(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			DumpHostState(t)
		}
	})
}

// DumpFRRPrefixList logs `vtysh -c "show ip prefix-list <name>"` to the test
// log. Used by scenario tests that exercise FRRPrefixList reconcile so a
// failure message includes the FRR-side state without an operator having to
// ssh in. Best-effort: silently no-ops if vtysh is missing.
func DumpFRRPrefixList(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath("vtysh"); err != nil {
		return
	}
	out, err := exec.Command("vtysh", "-c", "show ip prefix-list "+name).CombinedOutput()
	text := strings.TrimRight(string(out), "\n")
	switch {
	case err != nil:
		t.Logf("--- vtysh show ip prefix-list %s --- (err: %v)\n%s", name, err, text)
	case strings.TrimSpace(text) == "":
		t.Logf("--- vtysh show ip prefix-list %s --- (empty)", name)
	default:
		t.Logf("--- vtysh show ip prefix-list %s ---\n%s", name, text)
	}
}
