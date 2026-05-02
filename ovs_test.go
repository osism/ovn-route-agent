package main

import (
	"errors"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// ovsRecorder captures calls to RouteManager.runOVS for assertion. It returns
// canned responses keyed by the full joined command line and falls back to
// (nil, nil) — i.e. empty output, no error — for unmatched commands.
type ovsRecorder struct {
	calls     [][]string
	responses map[string]ovsResponse
}

type ovsResponse struct {
	out []byte
	err error
}

func newOVSRecorder() *ovsRecorder {
	return &ovsRecorder{responses: map[string]ovsResponse{}}
}

// on registers a canned response for a command identified by its full Args.
func (r *ovsRecorder) on(args []string, out string, err error) {
	r.responses[strings.Join(args, " ")] = ovsResponse{out: []byte(out), err: err}
}

func (r *ovsRecorder) hook() ovsExecFunc {
	return func(cmd *exec.Cmd) ([]byte, error) {
		args := append([]string{}, cmd.Args...)
		r.calls = append(r.calls, args)
		if resp, ok := r.responses[strings.Join(cmd.Args, " ")]; ok {
			return resp.out, resp.err
		}
		return nil, nil
	}
}

// findAddFlows returns just the flow strings from "ovs-ofctl add-flow <br> <flow>" calls.
func (r *ovsRecorder) findAddFlows() []string {
	var flows []string
	for _, c := range r.calls {
		if len(c) >= 4 && c[0] == "ovs-ofctl" && c[1] == "add-flow" {
			flows = append(flows, c[3])
		}
	}
	return flows
}

func TestHairpinFlow(t *testing.T) {
	tests := []struct {
		name      string
		cookie    string
		ofport    string
		ip        string
		bridgeMAC string
		routerMAC string
		ipv6      bool
		want      string
	}{
		{
			"basic IPv4 hairpin flow",
			"0x998", "42", "5.182.234.199", "aa:bb:cc:dd:ee:ff", "fa:16:3e:6f:a1:64", false,
			"cookie=0x998,priority=910,ip,in_port=42,ip_dst=5.182.234.199/32,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:6f:a1:64,output:in_port",
		},
		{
			"different IPv4 IP and ofport",
			"0x998", "7", "192.0.2.1", "11:22:33:44:55:66", "fa:16:3e:ab:cd:ef", false,
			"cookie=0x998,priority=910,ip,in_port=7,ip_dst=192.0.2.1/32,actions=mod_dl_src:11:22:33:44:55:66,mod_dl_dst:fa:16:3e:ab:cd:ef,output:in_port",
		},
		{
			"SNAT router external IP",
			"0x998", "3", "5.182.234.128", "82:ba:92:54:47:48", "fa:16:3e:45:06:3e", false,
			"cookie=0x998,priority=910,ip,in_port=3,ip_dst=5.182.234.128/32,actions=mod_dl_src:82:ba:92:54:47:48,mod_dl_dst:fa:16:3e:45:06:3e,output:in_port",
		},
		{
			"IPv6 FIP",
			"0x998", "42", "2001:db8::1", "aa:bb:cc:dd:ee:ff", "fa:16:3e:00:00:01", true,
			"cookie=0x998,priority=910,ipv6,in_port=42,ipv6_dst=2001:db8::1/128,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:00:00:01,output:in_port",
		},
		{
			"IPv6 SNAT",
			"0x998", "5", "2001:db8:cafe::1", "aa:bb:cc:dd:ee:ff", "fa:16:3e:00:00:02", true,
			"cookie=0x998,priority=910,ipv6,in_port=5,ipv6_dst=2001:db8:cafe::1/128,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:00:00:02,output:in_port",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HairpinFlow(tt.cookie, tt.ofport, tt.ip, tt.bridgeMAC, tt.routerMAC, tt.ipv6)
			if got != tt.want {
				t.Errorf("HairpinFlow() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMACTweakFlow(t *testing.T) {
	tests := []struct {
		name   string
		cookie string
		ofport string
		mac    string
		ipv6   bool
		want   string
	}{
		{
			"IPv4 flow",
			"0x999", "42", "aa:bb:cc:dd:ee:ff", false,
			"cookie=0x999,priority=900,ip,in_port=42,actions=mod_dl_dst:aa:bb:cc:dd:ee:ff,NORMAL",
		},
		{
			"IPv6 flow",
			"0x999", "42", "aa:bb:cc:dd:ee:ff", true,
			"cookie=0x999,priority=900,ipv6,in_port=42,actions=mod_dl_dst:aa:bb:cc:dd:ee:ff,NORMAL",
		},
		{
			"different ofport and MAC",
			"0x999", "7", "11:22:33:44:55:66", false,
			"cookie=0x999,priority=900,ip,in_port=7,actions=mod_dl_dst:11:22:33:44:55:66,NORMAL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MACTweakFlow(tt.cookie, tt.ofport, tt.mac, tt.ipv6)
			if got != tt.want {
				t.Errorf("MACTweakFlow() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOVSCmdWrapperPrepended(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:  "br-ex",
		ovsWrapper: []string{"docker", "exec", "openvswitch_vswitchd"},
	}

	cmd := rm.ovsCmd("ovs-ofctl", "add-flow", "br-ex", "flow")
	want := []string{"docker", "exec", "openvswitch_vswitchd", "ovs-ofctl", "add-flow", "br-ex", "flow"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("ovsCmd args = %v, want %v", cmd.Args, want)
	}
}

func TestOVSCmdNoWrapper(t *testing.T) {
	rm := &RouteManager{bridgeDev: "br-ex"}

	cmd := rm.ovsCmd("ovs-ofctl", "add-flow", "br-ex", "flow")
	want := []string{"ovs-ofctl", "add-flow", "br-ex", "flow"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("ovsCmd args = %v, want %v", cmd.Args, want)
	}
}

func TestEnsureOVSFlowsWithCachedDiscovery(t *testing.T) {
	rec := newOVSRecorder()
	rm := &RouteManager{
		bridgeDev:       "br-ex",
		cachedPatchPort: "patch-provnet-0",
		cachedOfport:    "42",
		cachedBridgeMAC: "aa:bb:cc:dd:ee:ff",
		execOVSHook:     rec.hook(),
	}

	if err := rm.EnsureOVSFlows(); err != nil {
		t.Fatalf("EnsureOVSFlows() error: %v", err)
	}

	// Expect: 1 del-flows + 2 add-flows (IPv4 + IPv6 MAC-tweak).
	if len(rec.calls) != 3 {
		t.Fatalf("expected 3 OVS commands, got %d: %v", len(rec.calls), rec.calls)
	}

	wantDel := []string{"ovs-ofctl", "del-flows", "br-ex", "cookie=0x999/-1"}
	if !reflect.DeepEqual(rec.calls[0], wantDel) {
		t.Errorf("first call = %v, want %v", rec.calls[0], wantDel)
	}

	flows := rec.findAddFlows()
	wantFlows := []string{
		"cookie=0x999,priority=900,ip,in_port=42,actions=mod_dl_dst:aa:bb:cc:dd:ee:ff,NORMAL",
		"cookie=0x999,priority=900,ipv6,in_port=42,actions=mod_dl_dst:aa:bb:cc:dd:ee:ff,NORMAL",
	}
	if !reflect.DeepEqual(flows, wantFlows) {
		t.Errorf("add-flow flows = %v, want %v", flows, wantFlows)
	}
}

func TestEnsureOVSFlowsTolersDelFailure(t *testing.T) {
	// del-flows is treated as best-effort; a failure must not abort the
	// subsequent add-flow calls.
	rec := newOVSRecorder()
	rec.on(
		[]string{"ovs-ofctl", "del-flows", "br-ex", "cookie=0x999/-1"},
		"some output", errors.New("transient ofctl error"),
	)
	rm := &RouteManager{
		bridgeDev:       "br-ex",
		cachedPatchPort: "patch-provnet-0",
		cachedOfport:    "42",
		cachedBridgeMAC: "aa:bb:cc:dd:ee:ff",
		execOVSHook:     rec.hook(),
	}

	if err := rm.EnsureOVSFlows(); err != nil {
		t.Fatalf("EnsureOVSFlows() should swallow del-flows error, got: %v", err)
	}

	if got := len(rec.findAddFlows()); got != 2 {
		t.Errorf("expected 2 add-flow calls after del failure, got %d", got)
	}
}

func TestEnsureOVSFlowsAddFailurePropagates(t *testing.T) {
	rec := newOVSRecorder()
	// First add-flow (IPv4) fails — function must return an error.
	rec.on(
		[]string{"ovs-ofctl", "add-flow", "br-ex",
			"cookie=0x999,priority=900,ip,in_port=42,actions=mod_dl_dst:aa:bb:cc:dd:ee:ff,NORMAL"},
		"add-flow failed", errors.New("bad flow"),
	)
	rm := &RouteManager{
		bridgeDev:       "br-ex",
		cachedPatchPort: "patch-provnet-0",
		cachedOfport:    "42",
		cachedBridgeMAC: "aa:bb:cc:dd:ee:ff",
		execOVSHook:     rec.hook(),
	}

	err := rm.EnsureOVSFlows()
	if err == nil {
		t.Fatal("expected error from EnsureOVSFlows when add-flow fails")
	}
	if !strings.Contains(err.Error(), "add IPv4 MAC-tweak flow") {
		t.Errorf("expected wrapped IPv4 error, got: %v", err)
	}
}

func TestReconcileOVSHairpinFlowsInstallsExpectedFlows(t *testing.T) {
	rec := newOVSRecorder()
	rm := &RouteManager{
		bridgeDev:       "br-ex",
		cachedPatchPort: "patch-provnet-0",
		cachedOfport:    "42",
		cachedBridgeMAC: "aa:bb:cc:dd:ee:ff",
		execOVSHook:     rec.hook(),
	}

	mapping := map[string]string{
		"5.182.234.199": "fa:16:3e:6f:a1:64",
		"2001:db8::1":   "fa:16:3e:00:00:01",
	}

	if err := rm.ReconcileOVSHairpinFlows(mapping); err != nil {
		t.Fatalf("ReconcileOVSHairpinFlows() error: %v", err)
	}

	// First call should be del-flows for the hairpin cookie.
	wantDel := []string{"ovs-ofctl", "del-flows", "br-ex", "cookie=0x998/-1"}
	if !reflect.DeepEqual(rec.calls[0], wantDel) {
		t.Errorf("first call = %v, want %v", rec.calls[0], wantDel)
	}

	// Two add-flows expected, one per IP. Order is map-iteration-dependent,
	// so compare as sets.
	got := rec.findAddFlows()
	sort.Strings(got)
	want := []string{
		"cookie=0x998,priority=910,ip,in_port=42,ip_dst=5.182.234.199/32,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:6f:a1:64,output:in_port",
		"cookie=0x998,priority=910,ipv6,in_port=42,ipv6_dst=2001:db8::1/128,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:00:00:01,output:in_port",
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("add-flow flows = %v, want %v", got, want)
	}
}

func TestReconcileOVSHairpinFlowsEmptyMapClearsAll(t *testing.T) {
	rec := newOVSRecorder()
	rm := &RouteManager{
		bridgeDev:       "br-ex",
		cachedOfport:    "42",
		cachedBridgeMAC: "aa:bb:cc:dd:ee:ff",
		execOVSHook:     rec.hook(),
	}

	if err := rm.ReconcileOVSHairpinFlows(nil); err != nil {
		t.Fatalf("ReconcileOVSHairpinFlows(nil) error: %v", err)
	}

	if len(rec.calls) != 1 {
		t.Fatalf("expected only del-flows call, got %d: %v", len(rec.calls), rec.calls)
	}
	if rec.calls[0][1] != "del-flows" {
		t.Errorf("expected del-flows, got %v", rec.calls[0])
	}
}

func TestReconcileOVSHairpinFlowsNoCacheIsNoOp(t *testing.T) {
	rec := newOVSRecorder()
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		execOVSHook: rec.hook(),
		// cachedOfport intentionally empty.
	}

	if err := rm.ReconcileOVSHairpinFlows(map[string]string{"10.0.0.1": "aa:aa:aa:aa:aa:aa"}); err != nil {
		t.Fatalf("expected no-op when cache is empty, got: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("expected no OVS commands when cache empty, got: %v", rec.calls)
	}
}

func TestReconcileOVSHairpinFlowsRejectsInvalidIP(t *testing.T) {
	rec := newOVSRecorder()
	rm := &RouteManager{
		bridgeDev:       "br-ex",
		cachedOfport:    "42",
		cachedBridgeMAC: "aa:bb:cc:dd:ee:ff",
		execOVSHook:     rec.hook(),
	}

	err := rm.ReconcileOVSHairpinFlows(map[string]string{"not-an-ip": "aa:aa:aa:aa:aa:aa"})
	if err == nil {
		t.Fatal("expected error for invalid IP, got nil")
	}
	if !strings.Contains(err.Error(), "invalid IP") {
		t.Errorf("expected 'invalid IP' in error, got: %v", err)
	}
}

func TestReconcileOVSHairpinFlowsDryRun(t *testing.T) {
	rec := newOVSRecorder()
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		dryRun:      true,
		execOVSHook: rec.hook(),
	}

	if err := rm.ReconcileOVSHairpinFlows(map[string]string{"10.0.0.1": "aa:aa:aa:aa:aa:aa"}); err != nil {
		t.Fatalf("dry-run should not error: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("dry-run should issue no commands, got: %v", rec.calls)
	}
}

func TestRemoveOVSFlowsIssuesBothDeletes(t *testing.T) {
	rec := newOVSRecorder()
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		execOVSHook: rec.hook(),
	}

	if err := rm.RemoveOVSFlows(); err != nil {
		t.Fatalf("RemoveOVSFlows() error: %v", err)
	}

	want := [][]string{
		{"ovs-ofctl", "del-flows", "br-ex", "cookie=0x999/-1"},
		{"ovs-ofctl", "del-flows", "br-ex", "cookie=0x998/-1"},
	}
	if !reflect.DeepEqual(rec.calls, want) {
		t.Errorf("RemoveOVSFlows() calls = %v, want %v", rec.calls, want)
	}
}

func TestRemoveOVSFlowsMACTweakFailureStops(t *testing.T) {
	rec := newOVSRecorder()
	rec.on(
		[]string{"ovs-ofctl", "del-flows", "br-ex", "cookie=0x999/-1"},
		"err output", errors.New("ofctl exit 1"),
	)
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		execOVSHook: rec.hook(),
	}

	if err := rm.RemoveOVSFlows(); err == nil {
		t.Fatal("expected error when MAC-tweak del-flows fails")
	}
	// The hairpin del-flows must NOT run after the first failure.
	if len(rec.calls) != 1 {
		t.Errorf("expected 1 call before bail-out, got %d: %v", len(rec.calls), rec.calls)
	}
}

func TestDiscoverPatchPortFindsPatchType(t *testing.T) {
	rec := newOVSRecorder()
	rec.on(
		[]string{"ovs-vsctl", "list-ports", "br-ex"},
		"phy-eth0\npatch-provnet-0\nphy-eth1\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "--if-exists", "get", "Interface", "phy-eth0", "type"},
		"\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "--if-exists", "get", "Interface", "patch-provnet-0", "type"},
		"patch\n", nil,
	)
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		execOVSHook: rec.hook(),
	}

	port, err := rm.discoverPatchPort()
	if err != nil {
		t.Fatalf("discoverPatchPort() error: %v", err)
	}
	if port != "patch-provnet-0" {
		t.Errorf("port = %q, want %q", port, "patch-provnet-0")
	}
}

func TestDiscoverPatchPortNoPatchFound(t *testing.T) {
	rec := newOVSRecorder()
	rec.on(
		[]string{"ovs-vsctl", "list-ports", "br-ex"},
		"phy-eth0\nphy-eth1\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "--if-exists", "get", "Interface", "phy-eth0", "type"},
		"\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "--if-exists", "get", "Interface", "phy-eth1", "type"},
		"\n", nil,
	)
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		execOVSHook: rec.hook(),
	}

	if _, err := rm.discoverPatchPort(); err == nil {
		t.Error("expected error when no patch port present")
	}
}

func TestGetOFPortRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		wantErr bool
		want    string
	}{
		{"valid ofport", "42\n", false, "42"},
		{"empty ofport", "\n", true, ""},
		{"unassigned ofport", "-1\n", true, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newOVSRecorder()
			rec.on(
				[]string{"ovs-vsctl", "get", "Interface", "patch-provnet-0", "ofport"},
				tt.out, nil,
			)
			rm := &RouteManager{
				bridgeDev:   "br-ex",
				execOVSHook: rec.hook(),
			}
			got, err := rm.getOFPort("patch-provnet-0")
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error for output %q", tt.out)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("ofport = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunOVSWithoutHookCallsCombinedOutput(t *testing.T) {
	// Sanity check that runOVS without a hook does run a real command.
	// /usr/bin/true (or "true" on PATH) is a portable success exit; on macOS
	// and Linux build agents this should always be present.
	rm := &RouteManager{}
	cmd := rm.ovsCmd("true")
	if cmd.Args[0] != "true" {
		t.Skipf("unexpected command shape: %v", cmd.Args)
	}
	out, err := rm.runOVS("true")
	if err != nil {
		// Some hermetic CI environments lack /bin/true; treat as skip.
		t.Skipf("`true` binary unavailable in test env: %v (output: %s)", err, out)
	}
}

// Ensure the recorder's own behavior is correct so test failures don't
// stem from a buggy harness.
func TestOVSRecorderRecordsAndResponds(t *testing.T) {
	rec := newOVSRecorder()
	rec.on([]string{"ovs-vsctl", "list-ports", "br-ex"}, "p0\np1\n", nil)
	hook := rec.hook()

	out, err := hook(exec.Command("ovs-vsctl", "list-ports", "br-ex"))
	if err != nil {
		t.Fatalf("recorder hook err: %v", err)
	}
	if string(out) != "p0\np1\n" {
		t.Errorf("recorder out = %q, want %q", string(out), "p0\np1\n")
	}

	if len(rec.calls) != 1 {
		t.Fatalf("expected 1 recorded call, got %d", len(rec.calls))
	}
	want := []string{"ovs-vsctl", "list-ports", "br-ex"}
	if !reflect.DeepEqual(rec.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", rec.calls[0], want)
	}
}

// Compile-time check that the recorder hook has the right signature.
var _ ovsExecFunc = (&ovsRecorder{}).hook()
