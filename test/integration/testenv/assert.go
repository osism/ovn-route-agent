//go:build integration

package testenv

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// AssertKernelRoute fails the test if no /32 route for ip exists on the
// default bridge device. Polls for up to timeout to give the agent time
// to install the route after a state change.
func AssertKernelRoute(t *testing.T, ip string, timeout time.Duration) {
	t.Helper()
	if net.ParseIP(ip) == nil {
		t.Fatalf("AssertKernelRoute: invalid IP %q", ip)
	}
	deadline := time.Now().Add(timeout)
	for {
		out, err := exec.Command("ip", "-4", "route", "show", ip+"/32", "dev", DefaultBridgeDev).CombinedOutput()
		if err == nil && strings.Contains(string(out), ip) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("kernel route %s/32 on %s not present after %s (last output: %q, err: %v)",
				ip, DefaultBridgeDev, timeout, strings.TrimSpace(string(out)), err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// AssertOVSFlow fails the test if no flow with the given cookie exists on
// br-ex. Cookie format mirrors ovs-ofctl: e.g. "0x999".
func AssertOVSFlow(t *testing.T, cookie string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		out, err := exec.Command("ovs-ofctl", "dump-flows", DefaultBridgeDev,
			fmt.Sprintf("cookie=%s/-1", cookie)).CombinedOutput()
		if err == nil {
			// dump-flows always prints a header line; a match means
			// at least one additional line containing "cookie=".
			lines := 0
			for _, line := range strings.Split(string(out), "\n") {
				if strings.Contains(line, "cookie=") {
					lines++
				}
			}
			if lines > 0 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("OVS flow with cookie %s on %s not present after %s (last output: %q, err: %v)",
				cookie, DefaultBridgeDev, timeout, strings.TrimSpace(string(out)), err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// AssertFRRRoute fails the test if no static /32 route for ip exists in
// the default VRF.
func AssertFRRRoute(t *testing.T, ip string, timeout time.Duration) {
	t.Helper()
	if net.ParseIP(ip) == nil {
		t.Fatalf("AssertFRRRoute: invalid IP %q", ip)
	}
	deadline := time.Now().Add(timeout)
	for {
		routes, err := frrStaticRoutes(DefaultVRFName)
		if err == nil {
			for _, r := range routes {
				if r == ip {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("FRR static route %s/32 in vrf %s not present after %s (current: %v, err: %v)",
				ip, DefaultVRFName, timeout, routes, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// AssertNftRule fails the test if `nft list table ip <DefaultNftTable>`
// does not contain substring. The substring must be specific enough to
// uniquely identify the rule (e.g. a VIP address combined with a port).
func AssertNftRule(t *testing.T, substring string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		out, err := exec.Command("nft", "list", "table", "ip", DefaultNftTable).CombinedOutput()
		if err == nil && strings.Contains(string(out), substring) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("nft table %s does not contain %q after %s (err: %v, output: %q)",
				DefaultNftTable, substring, timeout, err, strings.TrimSpace(string(out)))
		}
		time.Sleep(200 * time.Millisecond)
	}
}
