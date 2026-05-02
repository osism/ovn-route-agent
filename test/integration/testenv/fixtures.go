//go:build integration

package testenv

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

// LocalHostname mirrors the agent's hostname normalisation in ovn.go's
// getHostname (short-form, no domain). All scenario state that pretends to
// belong to "this host" must use this value.
func LocalHostname(t *testing.T) string {
	t.Helper()
	h, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}
	if idx := strings.IndexByte(h, '.'); idx != -1 {
		h = h[:idx]
	}
	return h
}

// LocalChassisUUID returns the SB Chassis UUID that ovn-controller registered
// for the local host. The test fails if no matching chassis appears within a
// short polling window — that means setup.sh did not finish wiring
// ovn-controller, or it crashed mid-run.
//
// We match on Chassis.name (which equals the OVS system-id, set explicitly by
// setup.sh to `hostname -s`) rather than Chassis.hostname, because OVN
// derives Chassis.hostname from gethostname(2) and on some hosts that
// returns an FQDN that differs from LocalHostname's short form.
//
// Polling tolerates a benign startup race: if ResetOVNState ran while
// ovn-controller had momentarily lost its chassis row (e.g. just after we
// paused northd or cleared SB state), ovn-controller re-registers within a
// reconciliation tick.
func LocalChassisUUID(t *testing.T, ctx context.Context, sb client.Client) string {
	t.Helper()
	hostname := LocalHostname(t)

	deadline := time.Now().Add(15 * time.Second)
	var lastCount int
	for {
		var chassis []SBChassis
		if err := sb.List(ctx, &chassis); err != nil {
			t.Fatalf("list SB chassis: %v", err)
		}
		lastCount = len(chassis)
		for _, ch := range chassis {
			if ch.Name == hostname || ch.Hostname == hostname {
				return ch.UUID
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no SB chassis matching name/hostname %q after 15s (have %d entries)",
				hostname, lastCount)
			return ""
		}
		select {
		case <-ctx.Done():
			t.Fatalf("LocalChassisUUID: context cancelled while waiting for chassis: %v", ctx.Err())
			return ""
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// tunnelKeyAllocator hands out monotonically-increasing tunnel keys so each
// fixture inserts unique Datapath_Binding / Port_Binding rows. Scoped to
// process lifetime; tests that need deterministic keys should set them
// explicitly.
var tunnelKeyAllocator atomic.Int32

// nextTunnelKey returns a tunnel_key value safely above the OVN_internal range
// so it cannot collide with anything ovn-controller might create on its own.
func nextTunnelKey() int {
	v := tunnelKeyAllocator.Add(1)
	return 16384 + int(v)
}

// RouterRef is the handle returned by MakeLocalRouter — it captures the UUIDs
// the test needs to reference later (e.g. to attach NAT entries or query
// Gateway_Chassis state).
type RouterRef struct {
	Name        string // logical router name
	RouterUUID  string
	LRPName     string // e.g. "lrp-router1"
	LRPUUID     string
	LRPMAC      string // MAC of the LRP
	LRPNetworks []string
	CRPort      string // "cr-lrp-router1"
	CRPortUUID  string
	DatapathID  string // SB Datapath_Binding UUID
}

// LocalRouterOpts configures MakeLocalRouter. ChassisUUID defaults to the
// local SB chassis. GatewayChassisHostnames lets the test seed peer entries
// (used by failover and restore-drained scenarios).
type LocalRouterOpts struct {
	Name        string
	LRPMAC      string
	LRPNetworks []string

	// ChassisUUID is the SB Chassis UUID to bind the chassisredirect
	// Port_Binding to. Defaults to the local chassis.
	ChassisUUID string

	// GatewayChassis is a list of (chassis_name, priority) entries to insert
	// into NB Gateway_Chassis. If nil, a single entry is created for the
	// local hostname at priority DefaultLocalPriority.
	GatewayChassis []GatewayChassisEntry
}

// GatewayChassisEntry is one row in NB Gateway_Chassis seeded by MakeLocalRouter.
type GatewayChassisEntry struct {
	ChassisName string
	Priority    int
}

// DefaultLocalPriority is the Gateway_Chassis priority assigned to the local
// chassis when LocalRouterOpts.GatewayChassis is unset. Matches the agent's
// minActivePriority floor so EnsureActivePriorityLead is a no-op for plain
// "single active router" scenarios.
const DefaultLocalPriority = 2

// MakeLocalRouter inserts a Logical_Router + Logical_Router_Port + matching
// SB Datapath_Binding + chassisredirect Port_Binding so the agent treats the
// router as locally-active. Returns the inserted UUIDs.
//
// Caller must have called PauseOVNNorthd(t) first; otherwise ovn-northd will
// garbage-collect the SB rows shortly after they are inserted.
func MakeLocalRouter(t *testing.T, ctx context.Context, nb, sb client.Client, opts LocalRouterOpts) RouterRef {
	t.Helper()
	if opts.Name == "" {
		t.Fatal("MakeLocalRouter: opts.Name required")
	}
	if opts.LRPMAC == "" {
		opts.LRPMAC = "fa:16:3e:11:22:33"
	}
	if len(opts.LRPNetworks) == 0 {
		opts.LRPNetworks = []string{"198.51.100.11/24"}
	}
	if opts.ChassisUUID == "" {
		opts.ChassisUUID = LocalChassisUUID(t, ctx, sb)
	}
	if len(opts.GatewayChassis) == 0 {
		opts.GatewayChassis = []GatewayChassisEntry{{
			ChassisName: LocalHostname(t),
			Priority:    DefaultLocalPriority,
		}}
	}

	lrpName := "lrp-" + opts.Name

	// --- NB: insert Gateway_Chassis + LRP + Logical_Router in ONE tx -----
	// Logical_Router_Port and Gateway_Chassis are non-root tables in the
	// OVN_Northbound schema, so the OVSDB engine deletes any unreferenced
	// row at commit time. Splitting into multiple transactions would
	// orphan the LRP (no LR points at it yet) and the Gateway_Chassis
	// entries (no LRP gateway_chassis points at them yet). All three
	// inserts must happen atomically with named-UUID cross-references.
	const lrpUUIDName = "lrp_named"
	const lrUUIDName = "lr_named"

	var ops []ovsdb.Operation
	gcRefs := make([]any, 0, len(opts.GatewayChassis))
	for i, e := range opts.GatewayChassis {
		gcUUIDName := fmt.Sprintf("gc_%d", i)
		ops = append(ops, ovsdb.Operation{
			Op:       ovsdb.OperationInsert,
			Table:    "Gateway_Chassis",
			UUIDName: gcUUIDName,
			Row: ovsdb.Row{
				"name":         lrpName + "_" + e.ChassisName,
				"chassis_name": e.ChassisName,
				"priority":     e.Priority,
			},
		})
		gcRefs = append(gcRefs, nameUUID(gcUUIDName))
	}

	lrpNetworks := make([]any, len(opts.LRPNetworks))
	for i, n := range opts.LRPNetworks {
		lrpNetworks[i] = n
	}
	lrpRow := ovsdb.Row{
		"name":     lrpName,
		"mac":      opts.LRPMAC,
		"networks": ovsdb.OvsSet{GoSet: lrpNetworks},
	}
	if len(gcRefs) > 0 {
		lrpRow["gateway_chassis"] = ovsdb.OvsSet{GoSet: gcRefs}
	}
	ops = append(ops, ovsdb.Operation{
		Op:       ovsdb.OperationInsert,
		Table:    "Logical_Router_Port",
		UUIDName: lrpUUIDName,
		Row:      lrpRow,
	})

	ops = append(ops, ovsdb.Operation{
		Op:       ovsdb.OperationInsert,
		Table:    "Logical_Router",
		UUIDName: lrUUIDName,
		Row: ovsdb.Row{
			"name":  opts.Name,
			"ports": ovsdb.OvsSet{GoSet: []any{nameUUID(lrpUUIDName)}},
		},
	})

	results := Transact(t, ctx, nb, ops)
	// Layout: [Gateway_Chassis...] [LRP] [LR]
	lrpUUID := results[len(opts.GatewayChassis)].UUID.GoUUID
	lrUUID := results[len(opts.GatewayChassis)+1].UUID.GoUUID

	// --- SB: insert Datapath_Binding then chassisredirect Port_Binding ---
	dpKey := nextTunnelKey()
	dpRow := &SBDatapathBinding{
		UUID:        "dp_named",
		TunnelKey:   dpKey,
		ExternalIDs: map[string]string{"name": opts.Name, "name2": opts.Name},
	}
	dpOps, err := sb.Create(dpRow)
	if err != nil {
		t.Fatalf("create datapath op: %v", err)
	}
	dpResults := Transact(t, ctx, sb, dpOps)
	dpUUID := dpResults[0].UUID.GoUUID

	pbKey := nextTunnelKey()
	crPortName := "cr-" + lrpName
	// Build the Port_Binding insert as a raw OVSDB op rather than going
	// through libovsdb's Create. With Create, the *string Chassis field
	// (declared as min=0,max=1 UUID ref in the schema) silently round-trips
	// to NULL — the resulting row has chassis=<nil> even when we pass a real
	// UUID. Raw ops with an explicit ovsdb.UUID{} value bypass that quirk
	// and produce a row with chassis correctly set, which is what the
	// agent's local-router detection requires.
	pbInsertOp := ovsdb.Operation{
		Op:       ovsdb.OperationInsert,
		Table:    "Port_Binding",
		UUIDName: "pb_named",
		Row: ovsdb.Row{
			"datapath":     realUUID(dpUUID),
			"tunnel_key":   pbKey,
			"logical_port": crPortName,
			"type":         "chassisredirect",
			"chassis":      realUUID(opts.ChassisUUID),
			// Raw ops require explicit OVSDB types: a plain Go map
			// serialises as JSON object, but the wire format for
			// OVSDB map columns is `["map", [[k,v]...]]`. OvsMap does
			// that wrapping. (libovsdb's Create handles this for us;
			// raw Operation.Row does not.)
			"options": ovsdb.OvsMap{GoMap: map[any]any{"distributed-port": lrpName}},
		},
	}
	pbResults := Transact(t, ctx, sb, []ovsdb.Operation{pbInsertOp})
	pbUUID := pbResults[0].UUID.GoUUID

	return RouterRef{
		Name:        opts.Name,
		RouterUUID:  lrUUID,
		LRPName:     lrpName,
		LRPUUID:     lrpUUID,
		LRPMAC:      opts.LRPMAC,
		LRPNetworks: opts.LRPNetworks,
		CRPort:      crPortName,
		CRPortUUID:  pbUUID,
		DatapathID:  dpUUID,
	}
}

// AddFIP inserts a dnat_and_snat NAT entry on the given router for externalIP
// and returns the new NAT UUID.
func AddFIP(t *testing.T, ctx context.Context, nb client.Client, router RouterRef, externalIP, logicalIP string) string {
	t.Helper()
	natRow := &NBNAT{
		UUID:       "nat_named",
		Type:       "dnat_and_snat",
		ExternalIP: externalIP,
		LogicalIP:  logicalIP,
	}
	natOps, err := nb.Create(natRow)
	if err != nil {
		t.Fatalf("create nat op: %v", err)
	}
	mutateOp := ovsdb.Operation{
		Op:    "mutate",
		Table: "Logical_Router",
		Where: []ovsdb.Condition{{
			Column:   "_uuid",
			Function: ovsdb.ConditionEqual,
			Value:    realUUID(router.RouterUUID),
		}},
		Mutations: []ovsdb.Mutation{{
			Column:  "nat",
			Mutator: ovsdb.MutateOperationInsert,
			Value:   ovsdb.OvsSet{GoSet: []any{nameUUID("nat_named")}},
		}},
	}
	results := Transact(t, ctx, nb, append(natOps, mutateOp))
	return results[0].UUID.GoUUID
}

// RemoveFIP deletes a NAT entry by UUID and removes it from any router's
// nat column. Best-effort if the entry has already been removed.
func RemoveFIP(t *testing.T, ctx context.Context, nb client.Client, router RouterRef, natUUID string) {
	t.Helper()
	mutateOp := ovsdb.Operation{
		Op:    "mutate",
		Table: "Logical_Router",
		Where: []ovsdb.Condition{{
			Column:   "_uuid",
			Function: ovsdb.ConditionEqual,
			Value:    realUUID(router.RouterUUID),
		}},
		Mutations: []ovsdb.Mutation{{
			Column:  "nat",
			Mutator: ovsdb.MutateOperationDelete,
			Value:   ovsdb.OvsSet{GoSet: []any{realUUID(natUUID)}},
		}},
	}
	deleteOps, err := nb.Where(&NBNAT{UUID: natUUID}).Delete()
	if err != nil {
		t.Fatalf("delete nat op: %v", err)
	}
	Transact(t, ctx, nb, append([]ovsdb.Operation{mutateOp}, deleteOps...))
}

// peerEncapIPCounter hands out unique loopback IPs for peer-chassis Encap
// rows. SB Encap has a unique index on (type, ip), and ovn-controller's
// local chassis already owns (geneve, 127.0.0.1) — so every additional
// peer chassis must use a distinct IP or the insert fails the index
// constraint.
var peerEncapIPCounter atomic.Int32

// MakeChassis inserts an SB Chassis row representing a peer host. Used by the
// stale-chassis cleanup scenario.
//
// SB Chassis.encaps has a min=1 constraint in the schema, so we also insert
// a minimal Encap row in the same transaction and reference it via a named
// UUID. Encap is non-root in SB and gets cascade-GC'd when the owning
// Chassis is deleted.
func MakeChassis(t *testing.T, ctx context.Context, sb client.Client, hostname string) string {
	t.Helper()
	const encapUUIDName = "encap_named"
	const chassisUUIDName = "ch_named"
	// Allocate a unique 127.0.0.X IP. The local chassis sits on .1, so we
	// start at .2 and increment per call. Wraps at .254 (more than enough
	// for any single test run).
	idx := peerEncapIPCounter.Add(1)
	encapIP := fmt.Sprintf("127.0.0.%d", 2+(idx%253))
	ops := []ovsdb.Operation{
		{
			Op:       ovsdb.OperationInsert,
			Table:    "Encap",
			UUIDName: encapUUIDName,
			Row: ovsdb.Row{
				"type":         "geneve",
				"ip":           encapIP,
				"chassis_name": hostname,
			},
		},
		{
			Op:       ovsdb.OperationInsert,
			Table:    "Chassis",
			UUIDName: chassisUUIDName,
			Row: ovsdb.Row{
				"name":     hostname, // production code matches by both name and hostname
				"hostname": hostname,
				"encaps":   ovsdb.OvsSet{GoSet: []any{nameUUID(encapUUIDName)}},
			},
		},
	}
	results := Transact(t, ctx, sb, ops)
	// Layout: [Encap, Chassis]
	return results[1].UUID.GoUUID
}

// DeleteChassis removes an SB Chassis by UUID.
func DeleteChassis(t *testing.T, ctx context.Context, sb client.Client, uuid string) {
	t.Helper()
	ops, err := sb.Where(&SBChassis{UUID: uuid}).Delete()
	if err != nil {
		t.Fatalf("delete chassis op: %v", err)
	}
	Transact(t, ctx, sb, ops)
}

// FindGatewayChassis returns the Gateway_Chassis row whose Name matches.
// Returns the zero value and false if not present.
func FindGatewayChassis(t *testing.T, ctx context.Context, nb client.Client, name string) (NBGatewayChassis, bool) {
	t.Helper()
	for _, gc := range MustList[NBGatewayChassis](t, ctx, nb) {
		if gc.Name == name {
			return gc, true
		}
	}
	return NBGatewayChassis{}, false
}

// SetCRPortChassis rebinds an existing chassisredirect Port_Binding to a new
// chassis UUID. Used by the failover scenario to simulate ovn-northd moving
// the gateway after Gateway_Chassis priorities change.
func SetCRPortChassis(t *testing.T, ctx context.Context, sb client.Client, pbUUID string, newChassisUUID *string) {
	t.Helper()
	pb := &SBPortBinding{UUID: pbUUID, Chassis: newChassisUUID}
	ops, err := sb.Where(pb).Update(pb, &pb.Chassis)
	if err != nil {
		t.Fatalf("build update op: %v", err)
	}
	Transact(t, ctx, sb, ops)
}

// CountManagedRoutes returns the number of NB Logical_Router_Static_Route rows
// tagged as agent-managed for the given chassis, or any chassis if "" is passed.
func CountManagedRoutes(t *testing.T, ctx context.Context, nb client.Client, chassis string) int {
	t.Helper()
	count := 0
	for _, r := range MustList[NBLogicalRouterStaticRoute](t, ctx, nb) {
		if r.ExternalIDs["ovn-network-agent"] != "managed" {
			continue
		}
		if chassis != "" && r.ExternalIDs["ovn-network-agent-chassis"] != chassis {
			continue
		}
		count++
	}
	return count
}

// FindMACBinding returns the Static_MAC_Binding for (lrp, ip), or false.
func FindMACBinding(t *testing.T, ctx context.Context, nb client.Client, lrp, ip string) (NBStaticMACBinding, bool) {
	t.Helper()
	for _, b := range MustList[NBStaticMACBinding](t, ctx, nb) {
		if b.LogicalPort == lrp && b.IP == ip {
			return b, true
		}
	}
	return NBStaticMACBinding{}, false
}

// FindStaticRoute returns the matching managed default route on router (or any
// router if routerUUID is ""), and true if present.
func FindStaticRoute(t *testing.T, ctx context.Context, nb client.Client, routerUUID, prefix string) (NBLogicalRouterStaticRoute, bool) {
	t.Helper()
	owners := make(map[string]string)
	if routerUUID != "" {
		for _, lr := range MustList[NBLogicalRouter](t, ctx, nb) {
			if lr.UUID != routerUUID {
				continue
			}
			for _, ru := range lr.StaticRoutes {
				owners[ru] = lr.UUID
			}
		}
	}
	for _, r := range MustList[NBLogicalRouterStaticRoute](t, ctx, nb) {
		if r.IPPrefix != prefix {
			continue
		}
		if routerUUID != "" {
			if _, ok := owners[r.UUID]; !ok {
				continue
			}
		}
		return r, true
	}
	return NBLogicalRouterStaticRoute{}, false
}

// SeedManagedRoute inserts a Logical_Router_Static_Route tagged for chassis
// owner — used by stale-chassis cleanup tests to plant entries that look like
// they were left behind by another agent.
func SeedManagedRoute(t *testing.T, ctx context.Context, nb client.Client, router RouterRef, prefix, nexthop, chassis string) string {
	t.Helper()
	rRow := &NBLogicalRouterStaticRoute{
		UUID:       "rt_named",
		IPPrefix:   prefix,
		Nexthop:    nexthop,
		OutputPort: strRef(router.LRPName),
		ExternalIDs: map[string]string{
			"ovn-network-agent":         "managed",
			"ovn-network-agent-chassis": chassis,
		},
	}
	rOps, err := nb.Create(rRow)
	if err != nil {
		t.Fatalf("create route op: %v", err)
	}
	mutateOp := ovsdb.Operation{
		Op:    "mutate",
		Table: "Logical_Router",
		Where: []ovsdb.Condition{{
			Column:   "_uuid",
			Function: ovsdb.ConditionEqual,
			Value:    realUUID(router.RouterUUID),
		}},
		Mutations: []ovsdb.Mutation{{
			Column:  "static_routes",
			Mutator: ovsdb.MutateOperationInsert,
			Value:   ovsdb.OvsSet{GoSet: []any{nameUUID("rt_named")}},
		}},
	}
	results := Transact(t, ctx, nb, append(rOps, mutateOp))
	return results[0].UUID.GoUUID
}
