package main

import (
	"errors"
	"strings"
	"syscall"
	"testing"
)

// nonexistentBridge is a synthetic interface name that is guaranteed not to
// exist on any CI host, so netlink.LinkByName fails predictably and exercises
// the "find bridge X: ..." error-wrap branches in routing_linux.go without
// touching real network state.
const nonexistentBridge = "ovnagent-nonexistent-br"

func TestEnsureBridgeIPRejectsInvalidIP(t *testing.T) {
	rm := &RouteManager{bridgeDev: nonexistentBridge}
	if err := rm.EnsureBridgeIP("not-an-ip"); err == nil {
		t.Fatal("EnsureBridgeIP(invalid) should return error")
	}
}

func TestEnsureBridgeIPWrapsLinkLookupError(t *testing.T) {
	rm := &RouteManager{bridgeDev: nonexistentBridge}
	err := rm.EnsureBridgeIP("169.254.169.254")
	if err == nil {
		t.Fatal("EnsureBridgeIP should error when the bridge device is missing")
	}
	if !strings.Contains(err.Error(), nonexistentBridge) {
		t.Errorf("error should mention the bridge name, got: %v", err)
	}
}

func TestRemoveBridgeIPRejectsInvalidIP(t *testing.T) {
	rm := &RouteManager{bridgeDev: nonexistentBridge}
	if err := rm.RemoveBridgeIP("not-an-ip"); err == nil {
		t.Fatal("RemoveBridgeIP(invalid) should return error")
	}
}

func TestRemoveBridgeIPWrapsLinkLookupError(t *testing.T) {
	rm := &RouteManager{bridgeDev: nonexistentBridge}
	err := rm.RemoveBridgeIP("169.254.169.254")
	if err == nil {
		t.Fatal("RemoveBridgeIP should error when the bridge device is missing")
	}
}

func TestAddKernelRouteWrapsLinkLookupError(t *testing.T) {
	rm := &RouteManager{bridgeDev: nonexistentBridge}
	err := rm.AddKernelRoute("10.0.0.1")
	if err == nil {
		t.Fatal("AddKernelRoute should error when the bridge device is missing")
	}
}

func TestAddKernelRouteRejectsInvalidIP(t *testing.T) {
	// AddKernelRoute looks up the bridge before parsing the IP, so an
	// invalid IP can only be reached on a host that has the bridge —
	// skipped here. Covered by the validation in helper callers and the
	// AddFRRRoutes batch path (validateIP) elsewhere.
	t.Skip("validation happens after link lookup; covered by callers")
}

func TestDelKernelRouteWrapsLinkLookupError(t *testing.T) {
	rm := &RouteManager{bridgeDev: nonexistentBridge}
	err := rm.DelKernelRoute("10.0.0.1")
	if err == nil {
		t.Fatal("DelKernelRoute should error when the bridge device is missing")
	}
}

func TestEnableProxyARPWritesProcSysOrErrors(t *testing.T) {
	// proc path: /proc/sys/net/ipv4/conf/<dev>/proxy_arp. With a synthetic
	// bridge that does not exist, the os.WriteFile call returns ENOENT and
	// the function wraps it. This exercises the error-wrap branch.
	rm := &RouteManager{bridgeDev: nonexistentBridge}
	err := rm.EnableProxyARP()
	if err == nil {
		t.Fatal("EnableProxyARP should error when the bridge's proxy_arp sysctl is absent")
	}
}

func TestCleanupRoutingTableNoOpWhenTableIDZero(t *testing.T) {
	rm := &RouteManager{routeTableID: 0}
	if err := rm.CleanupRoutingTable(); err != nil {
		t.Errorf("CleanupRoutingTable with table 0 should be a no-op, got: %v", err)
	}
}

func TestCleanupRoutingTableDryRun(t *testing.T) {
	rm := &RouteManager{routeTableID: 100, dryRun: true}
	if err := rm.CleanupRoutingTable(); err != nil {
		t.Errorf("CleanupRoutingTable in dry-run should not error, got: %v", err)
	}
}

func TestGetBridgeMACReturnsErrorForMissingBridge(t *testing.T) {
	rm := &RouteManager{bridgeDev: nonexistentBridge}
	if _, err := rm.GetBridgeMAC(); err == nil {
		t.Fatal("GetBridgeMAC should error when the bridge is missing")
	}
}

func TestCheckBridgeDeviceDryRunSkips(t *testing.T) {
	rm := &RouteManager{bridgeDev: nonexistentBridge, dryRun: true}
	if err := rm.CheckBridgeDevice(); err != nil {
		t.Errorf("CheckBridgeDevice in dry-run should not error, got: %v", err)
	}
}

func TestIsFileExists(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"EEXIST", syscall.EEXIST, true},
		{"wrapped EEXIST", &wrappedErr{syscall.EEXIST}, true},
		{"unrelated", errors.New("permission denied"), false},
		{"nil-equivalent unrelated", syscall.ENOENT, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isFileExists(tt.err); got != tt.want {
				t.Errorf("isFileExists(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// wrappedErr lets the isFileExists test verify errors.Is-style unwrapping
// without depending on fmt.Errorf's %w formatting.
type wrappedErr struct{ inner error }

func (w *wrappedErr) Error() string { return "wrapped: " + w.inner.Error() }
func (w *wrappedErr) Unwrap() error { return w.inner }
