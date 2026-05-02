package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

// strPtr is a tiny helper for taking the address of a string literal.
func strPtr(s string) *string { return &s }

// findOps returns ops in the recorded transaction at index `transactIdx`
// that match the given Op verb and Table, in order.
func findOps(t *testing.T, transacts [][]ovsdb.Operation, transactIdx int, verb, table string) []ovsdb.Operation {
	t.Helper()
	if transactIdx >= len(transacts) {
		t.Fatalf("recorded only %d transacts, wanted index %d", len(transacts), transactIdx)
	}
	var matched []ovsdb.Operation
	for _, op := range transacts[transactIdx] {
		if op.Op == verb && op.Table == table {
			matched = append(matched, op)
		}
	}
	return matched
}

// =============================================================================
// ensureDefaultRoute
// =============================================================================

func TestEnsureDefaultRoute_CreatesNewWhenAbsent(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router", &NBLogicalRouter{
		UUID: "lr-uuid-1",
		Name: "router1",
	})

	lr := LocalRouterInfo{
		RouterName: "router1",
		RouterUUID: "lr-uuid-1",
		LRPName:    "lrp-abc",
	}
	if err := c.ensureDefaultRoute(context.Background(), lr, "198.51.100.254"); err != nil {
		t.Fatalf("ensureDefaultRoute: %v", err)
	}

	tx := nb.recordedTransacts()
	if len(tx) != 1 {
		t.Fatalf("expected 1 transact, got %d", len(tx))
	}
	if len(tx[0]) != 2 {
		t.Fatalf("expected 2 ops (insert + mutate), got %d: %+v", len(tx[0]), tx[0])
	}

	if got := findOps(t, tx, 0, ovsdb.OperationInsert, "Logical_Router_Static_Route"); len(got) != 1 {
		t.Errorf("expected 1 insert on Logical_Router_Static_Route, got %d (ops=%+v)", len(got), tx[0])
	}
	muts := findOps(t, tx, 0, ovsdb.OperationMutate, "Logical_Router")
	if len(muts) != 1 {
		t.Fatalf("expected 1 mutate on Logical_Router, got %d", len(muts))
	}
	mut := muts[0]
	if len(mut.Mutations) != 1 || mut.Mutations[0].Column != "static_routes" || mut.Mutations[0].Mutator != ovsdb.MutateOperationInsert {
		t.Errorf("unexpected mutate op: %+v", mut)
	}
	uuid, ok := mut.Mutations[0].Value.(ovsdb.UUID)
	if !ok || uuid.GoUUID != "new_route" {
		t.Errorf("mutate value should reference uuid-name 'new_route', got %#v", mut.Mutations[0].Value)
	}
}

func TestEnsureDefaultRoute_NoOpWhenAlreadyCorrect(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router", &NBLogicalRouter{
		UUID:         "lr-uuid-1",
		Name:         "router1",
		StaticRoutes: []string{"route-uuid-1"},
	})
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID:     "route-uuid-1",
		IPPrefix: "0.0.0.0/0",
		Nexthop:  "198.51.100.254",
		ExternalIDs: map[string]string{
			"ovn-network-agent":         "managed",
			"ovn-network-agent-chassis": "host-a",
		},
	})

	lr := LocalRouterInfo{RouterName: "router1", RouterUUID: "lr-uuid-1", LRPName: "lrp-abc"}
	if err := c.ensureDefaultRoute(context.Background(), lr, "198.51.100.254"); err != nil {
		t.Fatalf("ensureDefaultRoute: %v", err)
	}
	if got := nb.recordedTransacts(); len(got) != 0 {
		t.Errorf("expected no transacts, got %d: %+v", len(got), got)
	}
}

func TestEnsureDefaultRoute_UpdatesChassisTagAfterFailover(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router", &NBLogicalRouter{
		UUID: "lr-uuid-1", Name: "router1", StaticRoutes: []string{"route-uuid-1"},
	})
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID:     "route-uuid-1",
		IPPrefix: "0.0.0.0/0",
		Nexthop:  "198.51.100.254",
		ExternalIDs: map[string]string{
			"ovn-network-agent":         "managed",
			"ovn-network-agent-chassis": "host-b",
		},
	})

	lr := LocalRouterInfo{RouterName: "router1", RouterUUID: "lr-uuid-1", LRPName: "lrp-abc"}
	if err := c.ensureDefaultRoute(context.Background(), lr, "198.51.100.254"); err != nil {
		t.Fatalf("ensureDefaultRoute: %v", err)
	}

	tx := nb.recordedTransacts()
	if len(tx) != 1 || len(tx[0]) != 1 {
		t.Fatalf("expected one transact with one op, got %+v", tx)
	}
	op := tx[0][0]
	if op.Op != ovsdb.OperationUpdate || op.Table != "Logical_Router_Static_Route" || op.UUID != "route-uuid-1" {
		t.Errorf("expected update on route-uuid-1, got %+v", op)
	}
}

func TestEnsureDefaultRoute_UpdatesNexthopWhenWrong(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router", &NBLogicalRouter{
		UUID: "lr-uuid-1", Name: "router1", StaticRoutes: []string{"route-uuid-1"},
	})
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID:     "route-uuid-1",
		IPPrefix: "0.0.0.0/0",
		Nexthop:  "198.51.100.99", // stale nexthop
		ExternalIDs: map[string]string{
			"ovn-network-agent":         "managed",
			"ovn-network-agent-chassis": "host-a",
		},
	})

	lr := LocalRouterInfo{RouterName: "router1", RouterUUID: "lr-uuid-1", LRPName: "lrp-abc"}
	if err := c.ensureDefaultRoute(context.Background(), lr, "198.51.100.254"); err != nil {
		t.Fatalf("ensureDefaultRoute: %v", err)
	}

	tx := nb.recordedTransacts()
	if len(tx) != 1 || len(tx[0]) != 1 {
		t.Fatalf("expected one transact with one op, got %+v", tx)
	}
	op := tx[0][0]
	if op.Op != ovsdb.OperationUpdate || op.UUID != "route-uuid-1" {
		t.Errorf("expected update on stale route, got %+v", op)
	}
}

func TestEnsureDefaultRoute_LeavesUnmanagedRouteAlone(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router", &NBLogicalRouter{
		UUID: "lr-uuid-1", Name: "router1", StaticRoutes: []string{"route-uuid-1"},
	})
	// Existing default route NOT managed by this agent (e.g., set by OpenStack).
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID:        "route-uuid-1",
		IPPrefix:    "0.0.0.0/0",
		Nexthop:     "203.0.113.1",
		ExternalIDs: nil,
	})

	lr := LocalRouterInfo{RouterName: "router1", RouterUUID: "lr-uuid-1", LRPName: "lrp-abc"}
	if err := c.ensureDefaultRoute(context.Background(), lr, "198.51.100.254"); err != nil {
		t.Fatalf("ensureDefaultRoute: %v", err)
	}
	if got := nb.recordedTransacts(); len(got) != 0 {
		t.Errorf("expected no transacts when an unmanaged default route exists, got %+v", got)
	}
}

func TestEnsureDefaultRoute_RouterNotFoundReturnsError(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	// No routers installed.
	_ = nb

	lr := LocalRouterInfo{RouterName: "router1", RouterUUID: "missing-uuid", LRPName: "lrp-abc"}
	err := c.ensureDefaultRoute(context.Background(), lr, "198.51.100.254")
	if err == nil {
		t.Fatal("expected error when router is missing, got nil")
	}
}

// =============================================================================
// ensureStaticMACBinding
// =============================================================================

func TestEnsureStaticMACBinding_CreatesWhenAbsent(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	if err := c.ensureStaticMACBinding(context.Background(), "lrp-abc", "198.51.100.254", "aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatalf("ensureStaticMACBinding: %v", err)
	}
	tx := nb.recordedTransacts()
	if len(tx) != 1 || len(tx[0]) != 1 {
		t.Fatalf("expected one transact with one insert, got %+v", tx)
	}
	op := tx[0][0]
	if op.Op != ovsdb.OperationInsert || op.Table != "Static_MAC_Binding" {
		t.Errorf("expected insert on Static_MAC_Binding, got %+v", op)
	}
}

func TestEnsureStaticMACBinding_NoOpWhenCorrect(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.setRows("Static_MAC_Binding", &NBStaticMACBinding{
		UUID:        "mb-1",
		LogicalPort: "lrp-abc",
		IP:          "198.51.100.254",
		MAC:         "aa:bb:cc:dd:ee:ff",
	})

	if err := c.ensureStaticMACBinding(context.Background(), "lrp-abc", "198.51.100.254", "aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatalf("ensureStaticMACBinding: %v", err)
	}
	if got := nb.recordedTransacts(); len(got) != 0 {
		t.Errorf("expected no transacts, got %+v", got)
	}
}

func TestEnsureStaticMACBinding_UpdatesOnFailover(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.setRows("Static_MAC_Binding", &NBStaticMACBinding{
		UUID:        "mb-1",
		LogicalPort: "lrp-abc",
		IP:          "198.51.100.254",
		MAC:         "11:22:33:44:55:66", // stale MAC from previous owner
	})

	if err := c.ensureStaticMACBinding(context.Background(), "lrp-abc", "198.51.100.254", "aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatalf("ensureStaticMACBinding: %v", err)
	}
	tx := nb.recordedTransacts()
	if len(tx) != 1 || len(tx[0]) != 1 {
		t.Fatalf("expected one transact with one update, got %+v", tx)
	}
	op := tx[0][0]
	if op.Op != ovsdb.OperationUpdate || op.Table != "Static_MAC_Binding" || op.UUID != "mb-1" {
		t.Errorf("expected update on mb-1, got %+v", op)
	}
}

func TestEnsureStaticMACBinding_IgnoresOtherLRPs(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.setRows("Static_MAC_Binding", &NBStaticMACBinding{
		UUID:        "mb-other",
		LogicalPort: "lrp-zzz",
		IP:          "198.51.100.254",
		MAC:         "aa:bb:cc:dd:ee:ff",
	})

	if err := c.ensureStaticMACBinding(context.Background(), "lrp-abc", "198.51.100.254", "aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatalf("ensureStaticMACBinding: %v", err)
	}
	tx := nb.recordedTransacts()
	if len(tx) != 1 || len(tx[0]) != 1 || tx[0][0].Op != ovsdb.OperationInsert {
		t.Errorf("expected one insert (binding for other LRP must not match), got %+v", tx)
	}
}

// =============================================================================
// EnsureGatewayRouting
// =============================================================================

func TestEnsureGatewayRouting_SkipsRouterWithoutIPv4(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	routers := []LocalRouterInfo{{
		RouterName:  "router-v6",
		RouterUUID:  "lr-uuid-v6",
		LRPName:     "lrp-v6",
		LRPNetworks: []string{"fe80::1/64"}, // no IPv4 — virtualGatewayIP must fail
	}}
	if err := c.EnsureGatewayRouting(context.Background(), routers, "aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatalf("EnsureGatewayRouting: %v", err)
	}
	if got := nb.recordedTransacts(); len(got) != 0 {
		t.Errorf("expected no transacts when no IPv4 CIDR present, got %+v", got)
	}
}

func TestEnsureGatewayRouting_ProcessesEachRouter(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router",
		&NBLogicalRouter{UUID: "lr-1", Name: "router1"},
		&NBLogicalRouter{UUID: "lr-2", Name: "router2"},
	)

	routers := []LocalRouterInfo{
		{RouterName: "router1", RouterUUID: "lr-1", LRPName: "lrp-1", LRPNetworks: []string{"198.51.100.11/24"}},
		{RouterName: "router2", RouterUUID: "lr-2", LRPName: "lrp-2", LRPNetworks: []string{"203.0.113.1/24"}},
	}
	if err := c.EnsureGatewayRouting(context.Background(), routers, "aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatalf("EnsureGatewayRouting: %v", err)
	}

	// Each router triggers one default-route create (insert+mutate, single transact)
	// and one MAC-binding insert (separate transact). 2 routers → 4 transacts.
	tx := nb.recordedTransacts()
	if len(tx) != 4 {
		t.Fatalf("expected 4 transacts (2 routers × {route, mac}), got %d: %+v", len(tx), tx)
	}
}

// =============================================================================
// EnsureActivePriorityLead
// =============================================================================

func TestEnsureActivePriorityLead(t *testing.T) {
	tests := []struct {
		name         string
		entries      []*NBGatewayChassis
		localRouters []LocalRouterInfo
		// wantBoosts maps the expected new priority for each LRP that should
		// be boosted. An empty map means no transact must be issued.
		wantBoosts map[string]int
	}{
		{
			name: "already leading with safe margin — no-op",
			entries: []*NBGatewayChassis{
				{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 3},
				{UUID: "g2", Name: "lrp-a_host-b", ChassisName: "host-b", Priority: 2},
			},
			localRouters: []LocalRouterInfo{{LRPName: "lrp-a"}},
			wantBoosts:   nil,
		},
		{
			name: "boosts to outrank peer",
			entries: []*NBGatewayChassis{
				{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 1},
				{UUID: "g2", Name: "lrp-a_host-b", ChassisName: "host-b", Priority: 5},
			},
			localRouters: []LocalRouterInfo{{LRPName: "lrp-a"}},
			wantBoosts:   map[string]int{"lrp-a": 6},
		},
		{
			name: "floors at minActivePriority when peer is drained",
			entries: []*NBGatewayChassis{
				{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 1},
				{UUID: "g2", Name: "lrp-a_host-b", ChassisName: "host-b", Priority: 0},
			},
			localRouters: []LocalRouterInfo{{LRPName: "lrp-a"}},
			wantBoosts:   map[string]int{"lrp-a": minActivePriority},
		},
		{
			name: "no peers — no-op",
			entries: []*NBGatewayChassis{
				{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 1},
			},
			localRouters: []LocalRouterInfo{{LRPName: "lrp-a"}},
			wantBoosts:   nil,
		},
		{
			name: "no local entry — no-op",
			entries: []*NBGatewayChassis{
				{UUID: "g2", Name: "lrp-a_host-b", ChassisName: "host-b", Priority: 5},
			},
			localRouters: []LocalRouterInfo{{LRPName: "lrp-a"}},
			wantBoosts:   nil,
		},
		{
			name: "ignores LRPs not in local router set",
			entries: []*NBGatewayChassis{
				{UUID: "g1", Name: "lrp-other_host-a", ChassisName: "host-a", Priority: 1},
				{UUID: "g2", Name: "lrp-other_host-b", ChassisName: "host-b", Priority: 5},
			},
			localRouters: []LocalRouterInfo{{LRPName: "lrp-a"}},
			wantBoosts:   nil,
		},
		{
			name: "multiple LRPs needing boost are batched in a single transaction",
			entries: []*NBGatewayChassis{
				{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 1},
				{UUID: "g2", Name: "lrp-a_host-b", ChassisName: "host-b", Priority: 4},
				{UUID: "g3", Name: "lrp-b_host-a", ChassisName: "host-a", Priority: 1},
				{UUID: "g4", Name: "lrp-b_host-b", ChassisName: "host-b", Priority: 7},
			},
			localRouters: []LocalRouterInfo{{LRPName: "lrp-a"}, {LRPName: "lrp-b"}},
			wantBoosts:   map[string]int{"lrp-a": 5, "lrp-b": 8},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, nb, _ := newOVNClientWithFakes(t, "host-a")

			gws := make([]any, 0, len(tt.entries))
			for _, e := range tt.entries {
				gws = append(gws, e)
			}
			nb.setRows("Gateway_Chassis", gws...)

			err := c.EnsureActivePriorityLead(context.Background(), tt.localRouters, "host-a")
			if err != nil {
				t.Fatalf("EnsureActivePriorityLead: %v", err)
			}

			tx := nb.recordedTransacts()
			if len(tt.wantBoosts) == 0 {
				if len(tx) != 0 {
					t.Fatalf("expected no transacts, got %d: %+v", len(tx), tx)
				}
				return
			}

			if len(tx) != 1 {
				t.Fatalf("expected one batched transact, got %d: %+v", len(tx), tx)
			}
			if len(tx[0]) != len(tt.wantBoosts) {
				t.Fatalf("expected %d update ops, got %d: %+v", len(tt.wantBoosts), len(tx[0]), tx[0])
			}
			// Map each local UUID back to its LRP so we can assert the
			// computed priority for each update op.
			lrpByLocalUUID := make(map[string]string, len(tt.wantBoosts))
			for _, e := range tt.entries {
				if e.ChassisName != "host-a" {
					continue
				}
				lrp := e.Name[:len(e.Name)-len("_"+e.ChassisName)]
				if _, want := tt.wantBoosts[lrp]; want {
					lrpByLocalUUID[e.UUID] = lrp
				}
			}
			for _, op := range tx[0] {
				if op.Op != ovsdb.OperationUpdate || op.Table != "Gateway_Chassis" {
					t.Errorf("unexpected op: %+v", op)
					continue
				}
				lrp, ok := lrpByLocalUUID[op.UUID]
				if !ok {
					t.Errorf("update on unexpected UUID %q", op.UUID)
					continue
				}
				gotPrio, ok := op.Row["priority"].(int)
				if !ok {
					t.Errorf("op.Row[\"priority\"] missing or wrong type: %#v", op.Row)
					continue
				}
				if gotPrio != tt.wantBoosts[lrp] {
					t.Errorf("LRP %s: new priority = %d, want %d", lrp, gotPrio, tt.wantBoosts[lrp])
				}
				delete(lrpByLocalUUID, op.UUID)
			}
			if len(lrpByLocalUUID) > 0 {
				t.Errorf("expected updates for local UUIDs %v but they were not issued", lrpByLocalUUID)
			}
		})
	}
}

// =============================================================================
// RemoveManagedNBEntries
// =============================================================================

func TestRemoveManagedNBEntries_NoLocalRouters(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	if err := c.RemoveManagedNBEntries(context.Background()); err != nil {
		t.Fatalf("RemoveManagedNBEntries: %v", err)
	}
	if got := nb.recordedTransacts(); len(got) != 0 {
		t.Errorf("expected no transacts, got %+v", got)
	}
}

func TestRemoveManagedNBEntries_DeletesManagedRouteAndMACBinding(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	c.state.LocalRouters = []LocalRouterInfo{
		{RouterName: "router1", RouterUUID: "lr-1", LRPName: "lrp-abc"},
	}
	c.state.HasLocalRouters = true

	nb.setRows("Logical_Router", &NBLogicalRouter{
		UUID: "lr-1", Name: "router1", StaticRoutes: []string{"route-managed", "route-foreign"},
	})
	nb.setRows("Logical_Router_Static_Route",
		&NBLogicalRouterStaticRoute{
			UUID:        "route-managed",
			IPPrefix:    "0.0.0.0/0",
			Nexthop:     "198.51.100.254",
			ExternalIDs: map[string]string{"ovn-network-agent": "managed"},
		},
		&NBLogicalRouterStaticRoute{
			UUID:     "route-foreign",
			IPPrefix: "10.0.0.0/8",
			Nexthop:  "203.0.113.1",
			// no managed tag → must be left alone
		},
	)
	nb.setRows("Static_MAC_Binding",
		&NBStaticMACBinding{UUID: "mb-local", LogicalPort: "lrp-abc", IP: "198.51.100.254", MAC: "aa:aa:aa:aa:aa:aa"},
		&NBStaticMACBinding{UUID: "mb-other", LogicalPort: "lrp-zzz", IP: "203.0.113.1", MAC: "bb:bb:bb:bb:bb:bb"},
	)

	if err := c.RemoveManagedNBEntries(context.Background()); err != nil {
		t.Fatalf("RemoveManagedNBEntries: %v", err)
	}

	tx := nb.recordedTransacts()
	// Expect: one transact for the route (mutate + delete), one for the local MAC binding.
	if len(tx) != 2 {
		t.Fatalf("expected 2 transacts, got %d: %+v", len(tx), tx)
	}

	// First transact: route deletion (mutate router.static_routes + delete row).
	routeTx := tx[0]
	var sawMutate, sawDelete bool
	for _, op := range routeTx {
		switch op.Op {
		case ovsdb.OperationMutate:
			if op.Table == "Logical_Router" {
				sawMutate = true
				if len(op.Mutations) != 1 || op.Mutations[0].Mutator != ovsdb.MutateOperationDelete {
					t.Errorf("expected delete mutation on static_routes, got %+v", op.Mutations)
				}
			}
		case ovsdb.OperationDelete:
			if op.Table == "Logical_Router_Static_Route" && op.UUID == "route-managed" {
				sawDelete = true
			}
		}
	}
	if !sawMutate || !sawDelete {
		t.Errorf("route transact missing mutate or delete: %+v", routeTx)
	}

	// Second transact: MAC binding delete (only the one whose LRP is local).
	mbTx := tx[1]
	if len(mbTx) != 1 {
		t.Fatalf("expected exactly one MAC-binding op, got %+v", mbTx)
	}
	if mbTx[0].Op != ovsdb.OperationDelete || mbTx[0].Table != "Static_MAC_Binding" || mbTx[0].UUID != "mb-local" {
		t.Errorf("expected delete on mb-local, got %+v", mbTx[0])
	}
}

func TestRemoveManagedNBEntries_SkipsManagedRouteOnNonLocalRouter(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	c.state.LocalRouters = []LocalRouterInfo{
		{RouterName: "router-local", RouterUUID: "lr-local", LRPName: "lrp-local"},
	}
	c.state.HasLocalRouters = true

	nb.setRows("Logical_Router",
		&NBLogicalRouter{UUID: "lr-local", Name: "router-local"},
		&NBLogicalRouter{UUID: "lr-remote", Name: "router-remote", StaticRoutes: []string{"route-remote"}},
	)
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID: "route-remote", IPPrefix: "0.0.0.0/0", Nexthop: "198.51.100.254",
		ExternalIDs: map[string]string{"ovn-network-agent": "managed"},
	})

	if err := c.RemoveManagedNBEntries(context.Background()); err != nil {
		t.Fatalf("RemoveManagedNBEntries: %v", err)
	}
	if got := nb.recordedTransacts(); len(got) != 0 {
		t.Errorf("must not touch routes on non-local routers, got %+v", got)
	}
}

// =============================================================================
// CleanupStaleChassisManagedEntries
// =============================================================================

func TestCleanupStale_DeletesRouteAndCorrelatedMACBinding(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router", &NBLogicalRouter{
		UUID: "lr-1", Name: "router1", StaticRoutes: []string{"route-stale"},
	})
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID:       "route-stale",
		IPPrefix:   "0.0.0.0/0",
		Nexthop:    "198.51.100.254",
		OutputPort: strPtr("lrp-abc"),
		ExternalIDs: map[string]string{
			"ovn-network-agent":         "managed",
			"ovn-network-agent-chassis": "host-gone",
		},
	})
	nb.setRows("Static_MAC_Binding",
		&NBStaticMACBinding{UUID: "mb-correlated", LogicalPort: "lrp-abc", IP: "198.51.100.254", MAC: "aa:aa:aa:aa:aa:aa"},
		&NBStaticMACBinding{UUID: "mb-unrelated", LogicalPort: "lrp-other", IP: "10.0.0.1", MAC: "bb:bb:bb:bb:bb:bb"},
	)

	staleChassis := map[string]bool{"host-gone": true}
	if err := c.CleanupStaleChassisManagedEntries(context.Background(), staleChassis); err != nil {
		t.Fatalf("CleanupStaleChassisManagedEntries: %v", err)
	}

	tx := nb.recordedTransacts()
	if len(tx) != 2 {
		t.Fatalf("expected 2 transacts (route + mac), got %d: %+v", len(tx), tx)
	}
	// First transact must delete the stale route.
	if got := findOps(t, tx, 0, ovsdb.OperationDelete, "Logical_Router_Static_Route"); len(got) != 1 || got[0].UUID != "route-stale" {
		t.Errorf("expected delete of route-stale in first transact, got %+v", tx[0])
	}
	// Second transact must delete only the correlated MAC binding (not mb-unrelated).
	if len(tx[1]) != 1 || tx[1][0].Op != ovsdb.OperationDelete || tx[1][0].UUID != "mb-correlated" {
		t.Errorf("expected delete of mb-correlated only, got %+v", tx[1])
	}
}

func TestCleanupStale_PreservesMACBindingWhenLiveChassisOwnsSamePort(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router", &NBLogicalRouter{
		UUID: "lr-1", Name: "router1", StaticRoutes: []string{"route-stale", "route-live"},
	})
	nb.setRows("Logical_Router_Static_Route",
		&NBLogicalRouterStaticRoute{
			UUID: "route-stale", IPPrefix: "0.0.0.0/0", Nexthop: "198.51.100.254",
			OutputPort: strPtr("lrp-abc"),
			ExternalIDs: map[string]string{
				"ovn-network-agent":         "managed",
				"ovn-network-agent-chassis": "host-gone",
			},
		},
		&NBLogicalRouterStaticRoute{
			UUID: "route-live", IPPrefix: "10.0.0.0/8", Nexthop: "198.51.100.254",
			OutputPort: strPtr("lrp-abc"),
			ExternalIDs: map[string]string{
				"ovn-network-agent":         "managed",
				"ovn-network-agent-chassis": "host-a", // live owner
			},
		},
	)
	nb.setRows("Static_MAC_Binding",
		&NBStaticMACBinding{UUID: "mb-shared", LogicalPort: "lrp-abc", IP: "198.51.100.254", MAC: "aa:aa:aa:aa:aa:aa"},
	)

	if err := c.CleanupStaleChassisManagedEntries(context.Background(), map[string]bool{"host-gone": true}); err != nil {
		t.Fatalf("CleanupStaleChassisManagedEntries: %v", err)
	}

	tx := nb.recordedTransacts()
	// Stale route is deleted; MAC binding is preserved because lrp-abc is still live.
	if len(tx) != 1 {
		t.Fatalf("expected only the route-delete transact (no MAC binding delete), got %d: %+v", len(tx), tx)
	}
	if got := findOps(t, tx, 0, ovsdb.OperationDelete, "Logical_Router_Static_Route"); len(got) != 1 || got[0].UUID != "route-stale" {
		t.Errorf("expected delete of route-stale, got %+v", tx[0])
	}
}

func TestCleanupStale_SkipsLegacyRouteWithoutChassisTag(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router", &NBLogicalRouter{UUID: "lr-1", Name: "router1", StaticRoutes: []string{"r1"}})
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID:        "r1",
		IPPrefix:    "0.0.0.0/0",
		Nexthop:     "198.51.100.254",
		ExternalIDs: map[string]string{"ovn-network-agent": "managed"}, // no chassis tag
	})

	if err := c.CleanupStaleChassisManagedEntries(context.Background(), map[string]bool{"host-gone": true}); err != nil {
		t.Fatalf("CleanupStaleChassisManagedEntries: %v", err)
	}
	if got := nb.recordedTransacts(); len(got) != 0 {
		t.Errorf("legacy untagged routes must be left alone, got %+v", got)
	}
}

func TestCleanupStale_SkipsUnmanagedRoutes(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router", &NBLogicalRouter{UUID: "lr-1", Name: "router1", StaticRoutes: []string{"r1"}})
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID:     "r1",
		IPPrefix: "0.0.0.0/0",
		Nexthop:  "198.51.100.254",
		ExternalIDs: map[string]string{
			"ovn-network-agent-chassis": "host-gone",
			// no "ovn-network-agent": "managed" key
		},
	})

	if err := c.CleanupStaleChassisManagedEntries(context.Background(), map[string]bool{"host-gone": true}); err != nil {
		t.Fatalf("CleanupStaleChassisManagedEntries: %v", err)
	}
	if got := nb.recordedTransacts(); len(got) != 0 {
		t.Errorf("unmanaged routes must be left alone, got %+v", got)
	}
}

// =============================================================================
// DrainGateways
// =============================================================================

func TestDrainGateways_NothingToDrain(t *testing.T) {
	c, nb, sb := newOVNClientWithFakes(t, "host-a")
	// Only entries for other chassis or already-drained.
	nb.setRows("Gateway_Chassis",
		&NBGatewayChassis{UUID: "g1", ChassisName: "host-b", Priority: 5},
		&NBGatewayChassis{UUID: "g2", ChassisName: "host-a", Priority: 0},
	)
	_ = sb

	if err := c.DrainGateways(context.Background(), "host-a"); err != nil {
		t.Fatalf("DrainGateways: %v", err)
	}
	if got := nb.recordedTransacts(); len(got) != 0 {
		t.Errorf("expected no transacts, got %+v", got)
	}
}

func TestDrainGateways_BatchesAndCompletesWhenSBDrained(t *testing.T) {
	c, nb, sb := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Gateway_Chassis",
		&NBGatewayChassis{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 5},
		&NBGatewayChassis{UUID: "g2", Name: "lrp-b_host-a", ChassisName: "host-a", Priority: 3},
		&NBGatewayChassis{UUID: "g3", Name: "lrp-c_host-a", ChassisName: "host-a", Priority: 2},
	)
	// SB shows no chassisredirect ports for host-a → first poll returns 0.
	sb.setRows("Chassis", &SBChassis{UUID: "ch-a", Name: "ch-a", Hostname: "host-a"})

	if err := c.DrainGateways(context.Background(), "host-a"); err != nil {
		t.Fatalf("DrainGateways: %v", err)
	}

	tx := nb.recordedTransacts()
	if len(tx) != 1 {
		t.Fatalf("expected one batched transact, got %d: %+v", len(tx), tx)
	}
	if len(tx[0]) != 3 {
		t.Fatalf("expected 3 update ops in the batch, got %d: %+v", len(tx[0]), tx[0])
	}
	for _, op := range tx[0] {
		if op.Op != ovsdb.OperationUpdate || op.Table != "Gateway_Chassis" {
			t.Errorf("unexpected op in drain batch: %+v", op)
		}
		if got, ok := op.Row["priority"].(int); !ok || got != 0 {
			t.Errorf("drain op should set priority=0, got %#v", op.Row["priority"])
		}
	}
}

func TestDrainGateways_TimeoutReturnsNilWithRemainingPorts(t *testing.T) {
	c, nb, sb := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Gateway_Chassis",
		&NBGatewayChassis{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 5},
	)
	// SB has a chassisredirect port still bound to this chassis → polling
	// will not converge. With a tight ctx deadline the function must return
	// nil after the first poll observes >0 and the select fires ctx.Done.
	sb.setRows("Chassis", &SBChassis{UUID: "ch-a", Name: "ch-a", Hostname: "host-a"})
	sb.setRows("Port_Binding", &SBPortBinding{
		UUID:        "pb-1",
		LogicalPort: "cr-lrp-a",
		Type:        "chassisredirect",
		Chassis:     strPtr("ch-a"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := c.DrainGateways(ctx, "host-a"); err != nil {
		t.Fatalf("DrainGateways: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("drain blocked too long: %v (ctx should fire promptly)", elapsed)
	}

	tx := nb.recordedTransacts()
	if len(tx) != 1 || len(tx[0]) != 1 {
		t.Errorf("expected one batched priority update before timeout, got %+v", tx)
	}
}

// =============================================================================
// RestoreDrainedGateways
// =============================================================================

func TestRestoreDrainedGateways_RestoresOnlyDrainedLocalEntries(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Gateway_Chassis",
		// drained local entries — must be restored to 1
		&NBGatewayChassis{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 0},
		&NBGatewayChassis{UUID: "g2", Name: "lrp-b_host-a", ChassisName: "host-a", Priority: 0},
		// already-active local entry — must be left alone
		&NBGatewayChassis{UUID: "g3", Name: "lrp-c_host-a", ChassisName: "host-a", Priority: 5},
		// drained entry on a different chassis — must be left alone
		&NBGatewayChassis{UUID: "g4", Name: "lrp-a_host-b", ChassisName: "host-b", Priority: 0},
	)

	c.RestoreDrainedGateways(context.Background(), "host-a")

	tx := nb.recordedTransacts()
	if len(tx) != 1 {
		t.Fatalf("expected one transact, got %d: %+v", len(tx), tx)
	}
	if len(tx[0]) != 2 {
		t.Fatalf("expected 2 restore ops, got %d: %+v", len(tx[0]), tx[0])
	}
	want := map[string]bool{"g1": true, "g2": true}
	for _, op := range tx[0] {
		if op.Op != ovsdb.OperationUpdate || op.Table != "Gateway_Chassis" {
			t.Errorf("unexpected op: %+v", op)
		}
		if !want[op.UUID] {
			t.Errorf("update on unexpected UUID %q", op.UUID)
		}
		if got, ok := op.Row["priority"].(int); !ok || got != 1 {
			t.Errorf("restore op should set priority=1, got %#v", op.Row["priority"])
		}
		delete(want, op.UUID)
	}
	if len(want) > 0 {
		t.Errorf("missing restore ops for %v", want)
	}
}

func TestRestoreDrainedGateways_NoOpWhenNothingDrained(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.setRows("Gateway_Chassis",
		&NBGatewayChassis{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 5},
	)
	c.RestoreDrainedGateways(context.Background(), "host-a")
	if got := nb.recordedTransacts(); len(got) != 0 {
		t.Errorf("expected no transacts, got %+v", got)
	}
}

func TestRestoreDrainedGateways_ListErrorIsLoggedNotPanicking(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.listErr = errors.New("connection refused")

	// Must not panic; signature returns no error.
	c.RestoreDrainedGateways(context.Background(), "host-a")
	if got := nb.recordedTransacts(); len(got) != 0 {
		t.Errorf("expected no transacts on list error, got %+v", got)
	}
}

// =============================================================================
// transactOps error propagation (sanity check on the shared helper)
// =============================================================================

func TestTransactOpsPropagatesTransportError(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.transactErr = fmt.Errorf("connection lost")

	err := c.transactOps(context.Background(), []ovsdb.Operation{{Op: ovsdb.OperationUpdate, Table: "Gateway_Chassis"}})
	if err == nil || err.Error() != "connection lost" {
		t.Errorf("expected 'connection lost', got %v", err)
	}
}
