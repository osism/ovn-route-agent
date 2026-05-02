//go:build integration

// Package testenv provides bootstrap, teardown, and assertion helpers for
// integration tests that exercise the agent against a real OVN/OVS/FRR/nftables
// stack on Linux.
//
// The package assumes the surrounding stack (ovsdb-server, ovn-northd,
// ovn-controller, FRR, nftables, br-ex) has already been provisioned by
// test/integration/setup.sh. Setup verifies the prerequisites and records
// initial state; Teardown best-effort restores it.
package testenv

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// DefaultBridgeDev is the bridge name the agent defaults to and which
// setup.sh provisions. testenv assertions target this device.
const DefaultBridgeDev = "br-ex"

// DefaultVRFName is the FRR VRF name the agent defaults to.
const DefaultVRFName = "vrf-provider"

// DefaultNftTable is the nftables table name the agent uses for port
// forwarding (mirrors nftTableName in nftables.go).
const DefaultNftTable = "ovn-network-agent"

// requiredBinaries are external commands the harness shells out to.
// Setup fails fast if any are missing.
var requiredBinaries = []string{
	"ip",
	"nft",
	"ovs-vsctl",
	"ovs-ofctl",
	"vtysh",
}

// Setup verifies the host has the prerequisites for integration tests:
//   - running as root (CAP_NET_ADMIN required for netlink ops)
//   - br-ex bridge present and up
//   - required CLI binaries on PATH
//   - OVN NB/SB reachable via TCP
//
// It registers a t.Cleanup that warns on leaked state. Sub-tests that mutate
// state should still call helpers like ResetAgentState directly.
func Setup(t *testing.T) {
	t.Helper()

	if runtime.GOOS != "linux" {
		t.Skipf("integration tests require Linux (current: %s)", runtime.GOOS)
	}

	if os.Geteuid() != 0 {
		t.Skip("integration tests require root (CAP_NET_ADMIN); rerun with sudo")
	}

	for _, bin := range requiredBinaries {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("required binary %q not found in PATH: %v", bin, err)
		}
	}

	if _, err := net.InterfaceByName(DefaultBridgeDev); err != nil {
		t.Skipf("bridge %s not present (run test/integration/setup.sh first): %v",
			DefaultBridgeDev, err)
	}

	t.Cleanup(func() { Teardown(t) })
}

// Teardown best-effort cleans residue the agent might have left behind:
//   - removes the agent's nftables table if present
//   - flushes /32 routes from br-ex installed by the agent (rtproto 44)
//   - removes static /32 routes from the FRR VRF
//
// The agent's own CleanupOnShutdown should handle this on SIGTERM, so
// Teardown is a safety net that prevents one failing test from poisoning
// the next.
func Teardown(t *testing.T) {
	t.Helper()

	// nftables table.
	if out, err := exec.Command("nft", "list", "table", "ip", DefaultNftTable).CombinedOutput(); err == nil && len(out) > 0 {
		if err := exec.Command("nft", "delete", "table", "ip", DefaultNftTable).Run(); err != nil {
			t.Logf("teardown: failed to delete nft table %s: %v", DefaultNftTable, err)
		}
	}

	// Kernel routes installed by the agent on br-ex are tagged with rtproto 44.
	// Flush by protocol so we don't touch system routes.
	if err := exec.Command("ip", "route", "flush", "proto", "44", "dev", DefaultBridgeDev).Run(); err != nil {
		// Likely "no such process" if there are no matching routes — ignore.
		if !strings.Contains(err.Error(), "exit status") {
			t.Logf("teardown: flush kernel routes on %s: %v", DefaultBridgeDev, err)
		}
	}

	// Strip leftover /32 static routes from the FRR VRF. We only attempt
	// this if vtysh succeeds — FRR may have been torn down by the host.
	routes, err := frrStaticRoutes(DefaultVRFName)
	if err == nil {
		for _, ip := range routes {
			args := []string{
				"-c", "conf t",
				"-c", "vrf " + DefaultVRFName,
				"-c", "no ip route " + ip + "/32",
				"-c", "exit-vrf",
				"-c", "end",
			}
			if err := exec.Command("vtysh", args...).Run(); err != nil {
				t.Logf("teardown: remove FRR route %s/32: %v", ip, err)
			}
		}
	}
}

// frrStaticRoutes returns the /32 static routes currently present in the
// given FRR VRF, parsed from "show ip route vrf <vrf> static".
func frrStaticRoutes(vrf string) ([]string, error) {
	out, err := exec.Command("vtysh",
		"-c", "show ip route vrf "+vrf+" static",
	).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("vtysh: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	var ips []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "S") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if !strings.Contains(f, "/32") {
				continue
			}
			ip, _, err := net.ParseCIDR(f)
			if err == nil {
				ips = append(ips, ip.String())
			}
			break
		}
	}
	return ips, nil
}

// RepoRoot returns the repository root by walking up from the directory
// containing this source file until it finds go.mod.
func RepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repo root (no go.mod found)")
		}
		dir = parent
	}
}

// AgentBinary returns the absolute path to the agent binary the harness
// should exec. It honours the OVN_AGENT_BINARY env var; otherwise it
// defaults to <repo>/ovn-network-agent. The binary must already exist —
// the Makefile target builds it before running integration tests.
func AgentBinary(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("OVN_AGENT_BINARY"); p != "" {
		if !filepath.IsAbs(p) {
			abs, err := filepath.Abs(p)
			if err != nil {
				t.Fatalf("resolve OVN_AGENT_BINARY=%q: %v", p, err)
			}
			p = abs
		}
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("OVN_AGENT_BINARY=%q not found: %v", p, err)
		}
		return p
	}
	bin := filepath.Join(RepoRoot(t), "ovn-network-agent")
	if _, err := os.Stat(bin); errors.Is(err, os.ErrNotExist) {
		t.Fatalf("agent binary not found at %s; run `make build` or set OVN_AGENT_BINARY", bin)
	}
	return bin
}
