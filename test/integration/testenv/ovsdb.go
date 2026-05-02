//go:build integration

package testenv

import (
	"context"
	"testing"
	"time"

	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

// Default OVN endpoints provisioned by setup.sh.
const (
	NBRemote = "tcp:127.0.0.1:6641"
	SBRemote = "tcp:127.0.0.1:6642"
)

// =============================================================================
// NB models — mirrors of the production types in ovn.go (the integration
// package cannot import from `main`). Field tags must match the OVSDB schema.
// =============================================================================

type NBNAT struct {
	UUID              string            `ovsdb:"_uuid"`
	Type              string            `ovsdb:"type"`
	ExternalIP        string            `ovsdb:"external_ip"`
	ExternalMAC       *string           `ovsdb:"external_mac"`
	ExternalPortRange string            `ovsdb:"external_port_range"`
	LogicalIP         string            `ovsdb:"logical_ip"`
	LogicalPort       *string           `ovsdb:"logical_port"`
	GatewayPort       *string           `ovsdb:"gateway_port"`
	Match             string            `ovsdb:"match"`
	Priority          int               `ovsdb:"priority"`
	Options           map[string]string `ovsdb:"options"`
	AllowedExtIPs     *string           `ovsdb:"allowed_ext_ips"`
	ExemptedExtIPs    *string           `ovsdb:"exempted_ext_ips"`
	ExternalIDs       map[string]string `ovsdb:"external_ids"`
}

type NBLogicalRouter struct {
	UUID         string            `ovsdb:"_uuid"`
	Name         string            `ovsdb:"name"`
	Ports        []string          `ovsdb:"ports"`
	Nat          []string          `ovsdb:"nat"`
	StaticRoutes []string          `ovsdb:"static_routes"`
	ExternalIDs  map[string]string `ovsdb:"external_ids"`
}

type NBLogicalRouterPort struct {
	UUID     string   `ovsdb:"_uuid"`
	Name     string   `ovsdb:"name"`
	MAC      string   `ovsdb:"mac"`
	Networks []string `ovsdb:"networks"`
}

type NBLogicalRouterStaticRoute struct {
	UUID        string            `ovsdb:"_uuid"`
	IPPrefix    string            `ovsdb:"ip_prefix"`
	Nexthop     string            `ovsdb:"nexthop"`
	OutputPort  *string           `ovsdb:"output_port"`
	Policy      *string           `ovsdb:"policy"`
	Options     map[string]string `ovsdb:"options"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}

type NBStaticMACBinding struct {
	UUID        string `ovsdb:"_uuid"`
	LogicalPort string `ovsdb:"logical_port"`
	IP          string `ovsdb:"ip"`
	MAC         string `ovsdb:"mac"`
}

type NBGatewayChassis struct {
	UUID        string            `ovsdb:"_uuid"`
	ChassisName string            `ovsdb:"chassis_name"`
	Name        string            `ovsdb:"name"`
	Priority    int               `ovsdb:"priority"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
	Options     map[string]string `ovsdb:"options"`
}

// =============================================================================
// SB models — mirrors of the production types in ovn.go plus SBDatapathBinding,
// which the production agent does not need but the integration harness does in
// order to insert chassisredirect Port_Bindings without ovn-northd.
// =============================================================================

type SBPortBinding struct {
	UUID                       string            `ovsdb:"_uuid"`
	Datapath                   string            `ovsdb:"datapath"`
	TunnelKey                  int               `ovsdb:"tunnel_key"`
	LogicalPort                string            `ovsdb:"logical_port"`
	Type                       string            `ovsdb:"type"`
	Chassis                    *string           `ovsdb:"chassis"`
	AdditionalChassis          []string          `ovsdb:"additional_chassis"`
	Encap                      *string           `ovsdb:"encap"`
	AdditionalEncap            []string          `ovsdb:"additional_encap"`
	Options                    map[string]string `ovsdb:"options"`
	ParentPort                 *string           `ovsdb:"parent_port"`
	Tag                        *int              `ovsdb:"tag"`
	Mac                        []string          `ovsdb:"mac"`
	NatAddresses               []string          `ovsdb:"nat_addresses"`
	Up                         *bool             `ovsdb:"up"`
	ExternalIDs                map[string]string `ovsdb:"external_ids"`
	GatewayChassis             []string          `ovsdb:"gateway_chassis"`
	HaChassisGroup             *string           `ovsdb:"ha_chassis_group"`
	VirtualParent              *string           `ovsdb:"virtual_parent"`
	RequestedChassis           *string           `ovsdb:"requested_chassis"`
	RequestedAdditionalChassis []string          `ovsdb:"requested_additional_chassis"`
	MirrorRules                []string          `ovsdb:"mirror_rules"`
}

type SBChassis struct {
	UUID        string            `ovsdb:"_uuid"`
	Name        string            `ovsdb:"name"`
	Hostname    string            `ovsdb:"hostname"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}

type SBDatapathBinding struct {
	UUID          string            `ovsdb:"_uuid"`
	TunnelKey     int               `ovsdb:"tunnel_key"`
	LoadBalancers []string          `ovsdb:"load_balancers"`
	ExternalIDs   map[string]string `ovsdb:"external_ids"`
}

// =============================================================================
// Database models
// =============================================================================

func nbDatabaseModel(t *testing.T) model.ClientDBModel {
	t.Helper()
	dbm, err := model.NewClientDBModel("OVN_Northbound", map[string]model.Model{
		"NAT":                         &NBNAT{},
		"Logical_Router":              &NBLogicalRouter{},
		"Logical_Router_Port":         &NBLogicalRouterPort{},
		"Logical_Router_Static_Route": &NBLogicalRouterStaticRoute{},
		"Static_MAC_Binding":          &NBStaticMACBinding{},
		"Gateway_Chassis":             &NBGatewayChassis{},
	})
	if err != nil {
		t.Fatalf("nbDatabaseModel: %v", err)
	}
	return dbm
}

func sbDatabaseModel(t *testing.T) model.ClientDBModel {
	t.Helper()
	dbm, err := model.NewClientDBModel("OVN_Southbound", map[string]model.Model{
		"Port_Binding":     &SBPortBinding{},
		"Chassis":          &SBChassis{},
		"Datapath_Binding": &SBDatapathBinding{},
	})
	if err != nil {
		t.Fatalf("sbDatabaseModel: %v", err)
	}
	return dbm
}

// =============================================================================
// Connected client helpers
// =============================================================================

// NewNBClient returns a libovsdb client connected to the local NB DB with all
// monitored tables already populated. The client is closed on test cleanup.
func NewNBClient(t *testing.T, ctx context.Context) client.Client {
	t.Helper()
	c, err := client.NewOVSDBClient(nbDatabaseModel(t), client.WithEndpoint(NBRemote))
	if err != nil {
		t.Fatalf("create NB client: %v", err)
	}
	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.Connect(connectCtx); err != nil {
		t.Fatalf("connect NB: %v", err)
	}
	mon := c.NewMonitor(
		client.WithTable(&NBNAT{}),
		client.WithTable(&NBLogicalRouter{}),
		client.WithTable(&NBLogicalRouterPort{}),
		client.WithTable(&NBLogicalRouterStaticRoute{}),
		client.WithTable(&NBStaticMACBinding{}),
		client.WithTable(&NBGatewayChassis{}),
	)
	if _, err := c.Monitor(connectCtx, mon); err != nil {
		c.Close()
		t.Fatalf("monitor NB: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// NewSBClient returns a libovsdb client connected to the local SB DB.
func NewSBClient(t *testing.T, ctx context.Context) client.Client {
	t.Helper()
	c, err := client.NewOVSDBClient(sbDatabaseModel(t), client.WithEndpoint(SBRemote))
	if err != nil {
		t.Fatalf("create SB client: %v", err)
	}
	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.Connect(connectCtx); err != nil {
		t.Fatalf("connect SB: %v", err)
	}
	mon := c.NewMonitor(
		client.WithTable(&SBPortBinding{}),
		client.WithTable(&SBChassis{}),
		client.WithTable(&SBDatapathBinding{}),
	)
	if _, err := c.Monitor(connectCtx, mon); err != nil {
		c.Close()
		t.Fatalf("monitor SB: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// Transact runs ops and asserts that no per-op errors occurred. Tests should
// almost always use this rather than calling client.Transact directly so that
// constraint violations surface as test failures with context.
func Transact(t *testing.T, ctx context.Context, c client.Client, ops []ovsdb.Operation) []ovsdb.OperationResult {
	t.Helper()
	if len(ops) == 0 {
		return nil
	}
	results, err := c.Transact(ctx, ops...)
	if err != nil {
		t.Fatalf("transact: %v (ops=%+v)", err, ops)
	}
	if opErrs, err := ovsdb.CheckOperationResults(results, ops); err != nil {
		t.Fatalf("transact op errors: %v (per-op=%+v ops=%+v)", err, opErrs, ops)
	}
	return results
}

// MustList runs c.List and fails the test on error. Returns the populated slice.
func MustList[T any](t *testing.T, ctx context.Context, c client.Client) []T {
	t.Helper()
	var out []T
	if err := c.List(ctx, &out); err != nil {
		t.Fatalf("list: %v", err)
	}
	return out
}

// =============================================================================
// State scrub between tests
// =============================================================================

// ResetOVNState removes all rows from the NB and SB tables the integration
// harness writes to, plus any agent-installed kernel/OVS/FRR state. Call at
// the start of each scenario so cases do not leak into one another.
func ResetOVNState(t *testing.T, ctx context.Context, nb, sb client.Client) {
	t.Helper()

	// Best-effort cleanup of agent residue first so that NB deletes do not
	// race with the agent (no agent should be running at this point).
	scrubLocalState(t)

	// NB ----------------------------------------------------------------
	// Delete the root tables first; OVSDB's referential-integrity GC then
	// removes the non-root rows that the root tables held references to.
	// Specifically, deleting a Logical_Router cascade-frees its NAT,
	// Logical_Router_Port, Gateway_Chassis (via LRP), and
	// Logical_Router_Static_Route children.
	//
	// Doing it in the other order — deleting NAT/static-routes/LRP first —
	// fails with "cannot delete X because of N remaining reference(s)" as
	// long as the parent Logical_Router still references them.
	for _, lr := range MustList[NBLogicalRouter](t, ctx, nb) {
		ops, err := nb.Where(&NBLogicalRouter{UUID: lr.UUID}).Delete()
		if err != nil {
			t.Fatalf("delete router op: %v", err)
		}
		Transact(t, ctx, nb, ops)
	}
	// Static_MAC_Binding is a root table on its own — nothing references it
	// from the test fixtures, but a previous run may have left rows.
	for _, mb := range MustList[NBStaticMACBinding](t, ctx, nb) {
		ops, err := nb.Where(&NBStaticMACBinding{UUID: mb.UUID}).Delete()
		if err != nil {
			t.Fatalf("delete static mac binding op: %v", err)
		}
		Transact(t, ctx, nb, ops)
	}
	// Defensive sweep of non-root tables in case the cache is briefly stale
	// or a future fixture inserts rows without an owning router. List+delete
	// is idempotent — an already-GC'd row simply isn't returned.
	for _, sr := range MustList[NBLogicalRouterStaticRoute](t, ctx, nb) {
		ops, err := nb.Where(&NBLogicalRouterStaticRoute{UUID: sr.UUID}).Delete()
		if err != nil {
			t.Fatalf("delete static route op: %v", err)
		}
		Transact(t, ctx, nb, ops)
	}
	for _, n := range MustList[NBNAT](t, ctx, nb) {
		ops, err := nb.Where(&NBNAT{UUID: n.UUID}).Delete()
		if err != nil {
			t.Fatalf("delete nat op: %v", err)
		}
		Transact(t, ctx, nb, ops)
	}
	for _, lrp := range MustList[NBLogicalRouterPort](t, ctx, nb) {
		ops, err := nb.Where(&NBLogicalRouterPort{UUID: lrp.UUID}).Delete()
		if err != nil {
			t.Fatalf("delete lrp op: %v", err)
		}
		Transact(t, ctx, nb, ops)
	}
	for _, gc := range MustList[NBGatewayChassis](t, ctx, nb) {
		ops, err := nb.Where(&NBGatewayChassis{UUID: gc.UUID}).Delete()
		if err != nil {
			t.Fatalf("delete gw chassis op: %v", err)
		}
		Transact(t, ctx, nb, ops)
	}

	// SB ----------------------------------------------------------------
	// Port_Bindings before Datapath_Bindings (FK).
	for _, pb := range MustList[SBPortBinding](t, ctx, sb) {
		ops, err := sb.Where(&SBPortBinding{UUID: pb.UUID}).Delete()
		if err != nil {
			t.Fatalf("delete port binding op: %v", err)
		}
		Transact(t, ctx, sb, ops)
	}
	for _, dp := range MustList[SBDatapathBinding](t, ctx, sb) {
		ops, err := sb.Where(&SBDatapathBinding{UUID: dp.UUID}).Delete()
		if err != nil {
			t.Fatalf("delete datapath op: %v", err)
		}
		Transact(t, ctx, sb, ops)
	}
	// Chassis: leave the local one alone (auto-created by ovn-controller),
	// remove any extras inserted by previous tests. Match by Name OR Hostname
	// because OVN derives Chassis.hostname from gethostname(2), which on some
	// hosts returns an FQDN that LocalHostname (short form) won't match. The
	// Name column is set by setup.sh from the OVS external_ids:system-id and
	// is guaranteed to equal the short hostname.
	localName := LocalHostname(t)
	for _, ch := range MustList[SBChassis](t, ctx, sb) {
		if ch.Name == localName || ch.Hostname == localName {
			continue
		}
		ops, err := sb.Where(&SBChassis{UUID: ch.UUID}).Delete()
		if err != nil {
			t.Fatalf("delete chassis op: %v", err)
		}
		Transact(t, ctx, sb, ops)
	}
}

// nameUUID returns an ovsdb-named UUID handle suitable for building cross-row
// references inside a single transaction (e.g. inserting a Logical_Router and
// pointing its ports column at a freshly-inserted LRP).
func nameUUID(name string) ovsdb.UUID { return ovsdb.UUID{GoUUID: name} }

// realUUID returns an ovsdb.UUID that references an already-existing row.
func realUUID(uuid string) ovsdb.UUID { return ovsdb.UUID{GoUUID: uuid} }

// strRef returns a pointer to the given string.
func strRef(s string) *string { return &s }
