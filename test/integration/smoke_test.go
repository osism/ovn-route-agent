//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// TestSmoke verifies the agent can connect to OVN, complete the initial
// reconcile, and exit cleanly on SIGTERM. No OVN logical state is
// configured — there are no local routers, so the reconcile is essentially
// a no-op. The point is to exercise the connect/shutdown lifecycle end to
// end against a real OVSDB.
func TestSmoke(t *testing.T) {
	testenv.Setup(t)

	cfg := testenv.Defaults()
	agent := testenv.RunAgent(t, cfg)

	readyCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := agent.WaitReady(readyCtx); err != nil {
		t.Fatalf("agent did not become ready: %v", err)
	}

	if err := agent.Stop(15 * time.Second); err != nil {
		t.Fatalf("agent did not stop cleanly: %v", err)
	}
}
