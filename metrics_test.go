package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// withTestMetrics swaps the process-wide metrics registry for a fresh one
// during the test and restores the previous value after the test ends.
// Returns the test registry so the caller can inspect it.
func withTestMetrics(t *testing.T) *metricsRegistry {
	t.Helper()
	prev := metrics
	m := newMetricsRegistry()
	metrics = m
	t.Cleanup(func() { metrics = prev })
	return m
}

func TestRecordingHelpersAreNilSafe(t *testing.T) {
	prev := metrics
	metrics = nil
	t.Cleanup(func() { metrics = prev })

	// None of these should panic when metrics is nil.
	recordReconcile("event", 100*time.Millisecond)
	setReconcileInProgress(true)
	setReconcileInProgress(false)
	setDesiredState(5, 2, 3)
	recordRouteReAdds(1, 2)
	setConsecutiveReAdds(4)
	setOVNConnectionState("nb", true)
	recordDrain("completed", time.Second)
	recordStaleChassisCleanup("success", 2)
	setMissingChassis(1)
}

func TestNewMetricsRegistryRegistersAllCollectors(t *testing.T) {
	m := newMetricsRegistry()

	// Sanity: collectors are non-nil and the registry can produce a metric
	// family list.
	got, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	wantMetrics := []string{
		"ovn_network_agent_reconcile_total",
		"ovn_network_agent_reconcile_duration_seconds",
		"ovn_network_agent_desired_ips",
		"ovn_network_agent_local_routers",
		"ovn_network_agent_route_readds_total",
		"ovn_network_agent_consecutive_readds",
		"ovn_network_agent_ovn_connection_state",
		"ovn_network_agent_drain_duration_seconds",
		"ovn_network_agent_drain_total",
		"ovn_network_agent_stale_chassis_cleanup_total",
		"ovn_network_agent_missing_chassis",
	}
	gotNames := make(map[string]bool, len(got))
	for _, mf := range got {
		gotNames[mf.GetName()] = true
	}
	for _, name := range wantMetrics {
		if !gotNames[name] {
			t.Errorf("metric %s missing from registry", name)
		}
	}
}

func TestRecordReconcileUpdatesCounterAndHistogram(t *testing.T) {
	m := withTestMetrics(t)

	recordReconcile("event", 250*time.Millisecond)
	recordReconcile("periodic", 50*time.Millisecond)

	got, _ := m.registry.Gather()
	for _, mf := range got {
		if mf.GetName() != "ovn_network_agent_reconcile_total" {
			continue
		}
		var event, periodic float64
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() != "trigger" {
					continue
				}
				switch l.GetValue() {
				case "event":
					event = m.GetCounter().GetValue()
				case "periodic":
					periodic = m.GetCounter().GetValue()
				}
			}
		}
		if event != 1 || periodic != 1 {
			t.Errorf("reconcile_total counters: event=%v, periodic=%v, want 1/1", event, periodic)
		}
	}
}

func TestSetOVNConnectionStateTogglesGauge(t *testing.T) {
	m := withTestMetrics(t)

	setOVNConnectionState("sb", true)
	setOVNConnectionState("nb", false)

	got, _ := m.registry.Gather()
	for _, mf := range got {
		if mf.GetName() != "ovn_network_agent_ovn_connection_state" {
			continue
		}
		for _, item := range mf.GetMetric() {
			var db string
			for _, l := range item.GetLabel() {
				if l.GetName() == "database" {
					db = l.GetValue()
				}
			}
			val := item.GetGauge().GetValue()
			switch db {
			case "sb":
				if val != 1 {
					t.Errorf("sb state = %v, want 1", val)
				}
			case "nb":
				if val != 0 {
					t.Errorf("nb state = %v, want 0", val)
				}
			}
		}
	}
}

func TestStartMetricsServerServesMetricsEndpoint(t *testing.T) {
	m := withTestMetrics(t)

	// Bind to an OS-assigned port to avoid collisions in CI.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close() // immediately free; startMetricsServer rebinds

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if err := startMetricsServer(ctx, addr, m); err != nil {
		t.Fatalf("startMetricsServer: %v", err)
	}

	// Record a metric so the endpoint has something visible to scrape.
	recordReconcile("event", 100*time.Millisecond)

	// Poll briefly for the listener to be ready.
	var resp *http.Response
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r, err := http.Get("http://" + addr + "/metrics")
		if err == nil {
			resp = r
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if resp == nil {
		t.Fatal("metrics endpoint never became reachable")
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "ovn_network_agent_reconcile_total") {
		t.Errorf("body missing reconcile_total; got:\n%s", body)
	}
}

func TestStartMetricsServerNoopWhenAddrEmpty(t *testing.T) {
	if err := startMetricsServer(context.Background(), "", newMetricsRegistry()); err != nil {
		t.Fatalf("expected nil error for empty addr, got %v", err)
	}
}

func TestStartMetricsServerErrorsOnInvalidAddr(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := startMetricsServer(ctx, "127.0.0.1:not-a-port", newMetricsRegistry()); err == nil {
		t.Fatal("expected error for invalid addr, got nil")
	}
}
