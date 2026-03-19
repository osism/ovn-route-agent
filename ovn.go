package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	backoff "github.com/cenkalti/backoff/v4"
	"github.com/ovn-org/libovsdb/cache"
	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
)

const eventDebounceInterval = 500 * time.Millisecond

// reconcileDebounceInterval coalesces multiple state refreshes (SB + NB)
// into a single reconciliation trigger.
const reconcileDebounceInterval = 100 * time.Millisecond

// =============================================================================
// OVN Southbound DB models
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

// =============================================================================
// OVN Northbound DB models
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
	Networks []string `ovsdb:"networks"`
}

type NBLogicalRouterStaticRoute struct {
	UUID       string            `ovsdb:"_uuid"`
	IPPrefix   string            `ovsdb:"ip_prefix"`
	Nexthop    string            `ovsdb:"nexthop"`
	OutputPort *string           `ovsdb:"output_port"`
	Policy     *string           `ovsdb:"policy"`
	Options    map[string]string `ovsdb:"options"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}

type NBStaticMACBinding struct {
	UUID        string `ovsdb:"_uuid"`
	LogicalPort string `ovsdb:"logical_port"`
	IP          string `ovsdb:"ip"`
	MAC         string `ovsdb:"mac"`
}

// =============================================================================
// OVN Client wrapper
// =============================================================================

// ovsdbClient is the interface for OVSDB clients.
// The SB client is read-only; the NB client also performs writes
// (static routes, MAC bindings).
type ovsdbClient interface {
	Connect(context.Context) error
	Close()
	Cache() *cache.TableCache
	NewMonitor(...client.MonitorOption) *client.Monitor
	Monitor(context.Context, *client.Monitor) (client.MonitorCookie, error)
	List(ctx context.Context, result interface{}) error
	Create(models ...model.Model) ([]ovsdb.Operation, error)
	Where(models ...model.Model) client.ConditionalAPI
	Transact(ctx context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error)
}

// LocalRouterInfo describes a logical router whose gateway is active on this chassis.
type LocalRouterInfo struct {
	RouterName  string // NB Logical_Router name
	RouterUUID  string // NB Logical_Router UUID
	LRPName     string // NB Logical_Router_Port name (e.g. "lrp-abc123")
	LRPUUID     string // NB Logical_Router_Port UUID
	LRPNetworks []string // NB Logical_Router_Port networks (e.g. ["198.51.100.11/24"])
	CRPort      string // SB chassisredirect logical_port (e.g. "cr-lrp-abc123")
}

type OVNState struct {
	mu sync.RWMutex

	// Derived state
	LocalChassisName string

	// Multi-router gateway state: routers whose chassisredirect port
	// is active on this chassis.
	LocalRouters    []LocalRouterInfo
	HasLocalRouters bool

	// NAT entries from NB, filtered to only locally-active routers.
	FIPs    []string // dnat_and_snat external IPs
	SNATIPs []string // snat external IPs

	// Networks auto-discovered from Logical_Router_Port.Networks of locally-active routers.
	DiscoveredNetworks []*net.IPNet
}

type OVNClient struct {
	sbClient ovsdbClient
	nbClient ovsdbClient
	state    *OVNState
	cfg      Config
	ctx      context.Context

	onChange func() // callback when state changes

	// ready is set to true after Connect() completes initial setup.
	// Event handlers check this to avoid calling refreshState before
	// both databases are connected and monitored.
	ready bool

	// Debounce timers for event-triggered refreshes
	debounceMu     sync.Mutex
	stateTimer     *time.Timer
	reconcileTimer *time.Timer
}

func NewOVNClient(cfg Config, onChange func()) *OVNClient {
	return &OVNClient{
		cfg:      cfg,
		state:    &OVNState{},
		onChange: onChange,
	}
}

func (o *OVNClient) Connect(ctx context.Context) error {
	o.ctx = ctx

	hostname, err := getHostname()
	if err != nil {
		return fmt.Errorf("get hostname: %w", err)
	}
	o.state.mu.Lock()
	o.state.LocalChassisName = hostname
	o.state.mu.Unlock()

	slog.Info("connecting to OVN databases", "hostname", hostname)

	reconnectBackoff := backoff.NewExponentialBackOff()

	// Connect to Southbound DB
	sbDBModel, err := sbDatabaseModel()
	if err != nil {
		return fmt.Errorf("create SB database model: %w", err)
	}

	sbOpts := []client.Option{
		client.WithReconnect(5*time.Second, reconnectBackoff),
		client.WithInactivityCheck(30*time.Second, 10*time.Second, reconnectBackoff),
	}
	for _, ep := range ovsdbEndpoints(o.cfg.OVNSBRemote) {
		sbOpts = append(sbOpts, client.WithEndpoint(ep))
	}
	o.sbClient, err = client.NewOVSDBClient(sbDBModel, sbOpts...)
	if err != nil {
		return fmt.Errorf("create SB client: %w", err)
	}

	if err := o.sbClient.Connect(ctx); err != nil {
		return fmt.Errorf("connect SB: %w", err)
	}
	slog.Info("connected to OVN SB", "remote", o.cfg.OVNSBRemote)

	// Monitor SB tables
	o.sbClient.Cache().AddEventHandler(&sbEventHandler{ovn: o})

	sbMon := o.sbClient.NewMonitor(
		client.WithTable(&SBPortBinding{}),
		client.WithTable(&SBChassis{}),
	)
	if _, err := o.sbClient.Monitor(ctx, sbMon); err != nil {
		return fmt.Errorf("monitor SB: %w", err)
	}

	// Connect to Northbound DB
	nbDBModel, err := nbDatabaseModel()
	if err != nil {
		return fmt.Errorf("create NB database model: %w", err)
	}

	nbReconnectBackoff := backoff.NewExponentialBackOff()

	nbOpts := []client.Option{
		client.WithReconnect(5*time.Second, nbReconnectBackoff),
		client.WithInactivityCheck(30*time.Second, 10*time.Second, nbReconnectBackoff),
	}
	for _, ep := range ovsdbEndpoints(o.cfg.OVNNBRemote) {
		nbOpts = append(nbOpts, client.WithEndpoint(ep))
	}
	o.nbClient, err = client.NewOVSDBClient(nbDBModel, nbOpts...)
	if err != nil {
		return fmt.Errorf("create NB client: %w", err)
	}

	if err := o.nbClient.Connect(ctx); err != nil {
		return fmt.Errorf("connect NB: %w", err)
	}
	slog.Info("connected to OVN NB", "remote", o.cfg.OVNNBRemote)

	// Monitor NB tables
	o.nbClient.Cache().AddEventHandler(&nbEventHandler{ovn: o})

	nbMon := o.nbClient.NewMonitor(
		client.WithTable(&NBNAT{}),
		client.WithTable(&NBLogicalRouter{}),
		client.WithTable(&NBLogicalRouterPort{}),
		client.WithTable(&NBLogicalRouterStaticRoute{}),
		client.WithTable(&NBStaticMACBinding{}),
	)
	if _, err := o.nbClient.Monitor(ctx, nbMon); err != nil {
		return fmt.Errorf("monitor NB: %w", err)
	}

	// Mark as ready so event handlers can now trigger refreshes.
	o.ready = true

	// Initial state refresh
	o.refreshState(ctx)

	// Cancel debounce timers started by initial monitor events —
	// refreshState already captured the current state, so these would
	// only cause redundant reconciliations.
	o.cancelPendingTimers()

	return nil
}

func (o *OVNClient) Close() {
	o.cancelPendingTimers()

	if o.sbClient != nil {
		o.sbClient.Close()
	}
	if o.nbClient != nil {
		o.nbClient.Close()
	}
}

// cancelPendingTimers stops all pending debounce and reconcile timers.
func (o *OVNClient) cancelPendingTimers() {
	o.debounceMu.Lock()
	defer o.debounceMu.Unlock()
	if o.stateTimer != nil {
		o.stateTimer.Stop()
		o.stateTimer = nil
	}
	if o.reconcileTimer != nil {
		o.reconcileTimer.Stop()
		o.reconcileTimer = nil
	}
}

// GetState returns a snapshot of the current OVN state.
func (o *OVNClient) GetState() OVNState {
	o.state.mu.RLock()
	defer o.state.mu.RUnlock()
	localRouters := make([]LocalRouterInfo, len(o.state.LocalRouters))
	copy(localRouters, o.state.LocalRouters)
	discoveredNets := make([]*net.IPNet, len(o.state.DiscoveredNetworks))
	copy(discoveredNets, o.state.DiscoveredNetworks)
	return OVNState{
		LocalChassisName:   o.state.LocalChassisName,
		LocalRouters:       localRouters,
		HasLocalRouters:    o.state.HasLocalRouters,
		FIPs:               append([]string{}, o.state.FIPs...),
		SNATIPs:            append([]string{}, o.state.SNATIPs...),
		DiscoveredNetworks: discoveredNets,
	}
}

// refreshState performs a unified state refresh from both OVN databases.
// It determines which routers are locally active and collects their NAT entries.
//
// Mapping chain:
//
//	SB chassisredirect (logical_port="cr-lrp-X", chassis=local?)
//	  → NB Logical_Router_Port (name="lrp-X")
//	  → NB Logical_Router (ports ⊇ {LRP UUID}, nat = {NAT UUIDs})
//	  → NB NAT (external_ip)
func (o *OVNClient) refreshState(ctx context.Context) {
	// Step 1: Find all chassisredirect port bindings and resolve chassis hostnames.
	var portBindings []SBPortBinding
	if err := o.sbClient.List(ctx, &portBindings); err != nil {
		slog.Error("failed to list port bindings", "error", err)
		return
	}

	var chassis []SBChassis
	if err := o.sbClient.List(ctx, &chassis); err != nil {
		slog.Error("failed to list chassis", "error", err)
		return
	}

	chassisHostname := make(map[string]string, len(chassis))
	for _, ch := range chassis {
		chassisHostname[ch.UUID] = ch.Hostname
		chassisHostname[ch.Name] = ch.Hostname
	}

	localChassisName := o.state.LocalChassisName

	// Collect LRP names from chassisredirect ports active on this chassis.
	// A chassisredirect logical_port has the form "cr-lrp-<LRP_NAME>".
	// localLRPNames maps: LRP name → CR port logical_port name.
	localLRPNames := make(map[string]string)
	for _, pb := range portBindings {
		if pb.Type != "chassisredirect" || pb.Chassis == nil || *pb.Chassis == "" {
			continue
		}
		// If gateway_port is configured, restrict to that single port.
		if o.cfg.GatewayPort != "" && pb.LogicalPort != o.cfg.GatewayPort {
			continue
		}
		hostname := chassisHostname[*pb.Chassis]
		if hostname == localChassisName {
			lrpName := strings.TrimPrefix(pb.LogicalPort, "cr-")
			localLRPNames[lrpName] = pb.LogicalPort
		}
	}

	// Step 2: Build NB Logical_Router_Port name → UUID map.
	var lrps []NBLogicalRouterPort
	if err := o.nbClient.List(ctx, &lrps); err != nil {
		slog.Error("failed to list logical router ports", "error", err)
		return
	}
	lrpNameToUUID := make(map[string]string, len(lrps))
	lrpByUUID := make(map[string]NBLogicalRouterPort, len(lrps))
	for _, lrp := range lrps {
		lrpNameToUUID[lrp.Name] = lrp.UUID
		lrpByUUID[lrp.UUID] = lrp
	}

	// Resolve local LRP names → UUIDs.
	localLRPUUIDs := make(map[string]bool)
	for lrpName := range localLRPNames {
		if uuid, ok := lrpNameToUUID[lrpName]; ok {
			localLRPUUIDs[uuid] = true
		}
	}

	// Step 3: Find routers that own a locally-active LRP. Collect their NAT UUIDs.
	var routers []NBLogicalRouter
	if err := o.nbClient.List(ctx, &routers); err != nil {
		slog.Error("failed to list logical routers", "error", err)
		return
	}

	localNATUUIDs := make(map[string]bool)
	var localRouters []LocalRouterInfo
	for _, router := range routers {
		var matchedLRP *NBLogicalRouterPort
		for _, portUUID := range router.Ports {
			if localLRPUUIDs[portUUID] {
				lrp := lrpByUUID[portUUID]
				matchedLRP = &lrp
				break
			}
		}
		if matchedLRP == nil {
			continue
		}
		localRouters = append(localRouters, LocalRouterInfo{
			RouterName:  router.Name,
			RouterUUID:  router.UUID,
			LRPName:     matchedLRP.Name,
			LRPUUID:     matchedLRP.UUID,
			LRPNetworks: matchedLRP.Networks,
			CRPort:      localLRPNames[matchedLRP.Name],
		})
		for _, natUUID := range router.Nat {
			localNATUUIDs[natUUID] = true
		}
	}

	// Step 4: Extract network CIDRs from locally-active LRPs.
	var discoveredNets []*net.IPNet
	for _, lr := range localRouters {
		for _, ns := range lr.LRPNetworks {
			_, cidr, err := net.ParseCIDR(ns)
			if err != nil {
				slog.Warn("failed to parse LRP network", "network", ns, "router", lr.RouterName, "error", err)
				continue
			}
			discoveredNets = append(discoveredNets, cidr)
		}
	}
	discoveredNets = uniqueNetworks(discoveredNets)

	// Determine effective network filters: manual config takes precedence over auto-discovery.
	effectiveFilters := effectiveNetworkFilters(o.cfg.NetworkFilters, discoveredNets)

	// Step 5: Filter NAT entries to only those belonging to locally-active routers.
	var nats []NBNAT
	if err := o.nbClient.List(ctx, &nats); err != nil {
		slog.Error("failed to list NAT entries", "error", err)
		return
	}

	var fips, snatIPs []string
	for _, nat := range nats {
		if !localNATUUIDs[nat.UUID] {
			continue
		}
		ip := nat.ExternalIP
		if len(effectiveFilters) > 0 {
			parsedIP := net.ParseIP(ip)
			if parsedIP == nil || !containedInAny(parsedIP, effectiveFilters) {
				continue
			}
		}
		switch nat.Type {
		case "dnat_and_snat":
			fips = append(fips, ip)
		case "snat":
			snatIPs = append(snatIPs, ip)
		}
	}

	// Step 6: Update state atomically.
	o.state.mu.Lock()
	o.state.LocalRouters = localRouters
	o.state.HasLocalRouters = len(localRouters) > 0
	o.state.FIPs = fips
	o.state.SNATIPs = snatIPs
	o.state.DiscoveredNetworks = discoveredNets
	o.state.mu.Unlock()

	slog.Info("state updated",
		"local_routers", len(localRouters),
		"fips", len(fips),
		"snat_ips", len(snatIPs),
		"discovered_networks", len(discoveredNets),
	)
	for _, lr := range localRouters {
		slog.Debug("locally active router",
			"router", lr.RouterName,
			"lrp", lr.LRPName,
			"cr_port", lr.CRPort,
		)
	}
}

// debounceStateRefresh schedules a coalescing state refresh.
// Unlike a resetting debounce, this does not extend the delay when new events
// arrive — it fires at most eventDebounceInterval after the first event.
func (o *OVNClient) debounceStateRefresh() {
	if !o.ready {
		return // not fully connected yet
	}
	o.debounceMu.Lock()
	defer o.debounceMu.Unlock()
	if o.stateTimer != nil {
		return // timer already pending, coalesce
	}
	o.stateTimer = time.AfterFunc(eventDebounceInterval, func() {
		o.debounceMu.Lock()
		o.stateTimer = nil
		o.debounceMu.Unlock()
		if o.ctx.Err() != nil {
			return
		}
		o.refreshState(o.ctx)
		o.scheduleReconcile()
	})
}

// immediateStateRefresh bypasses debouncing for chassisredirect changes
// to minimise failover reaction time.
func (o *OVNClient) immediateStateRefresh() {
	if !o.ready {
		return // not fully connected yet
	}
	o.debounceMu.Lock()
	if o.stateTimer != nil {
		o.stateTimer.Stop()
		o.stateTimer = nil
	}
	o.debounceMu.Unlock()

	go func() {
		if o.ctx.Err() != nil {
			return
		}
		o.refreshState(o.ctx)
		// Bypass reconcile debounce for fast failover.
		if o.onChange != nil {
			o.onChange()
		}
	}()
}

// scheduleReconcile coalesces reconciliation triggers from state refreshes
// into a single onChange callback.
func (o *OVNClient) scheduleReconcile() {
	o.debounceMu.Lock()
	defer o.debounceMu.Unlock()
	if o.reconcileTimer != nil {
		return // already scheduled
	}
	o.reconcileTimer = time.AfterFunc(reconcileDebounceInterval, func() {
		o.debounceMu.Lock()
		o.reconcileTimer = nil
		o.debounceMu.Unlock()
		if o.onChange != nil {
			o.onChange()
		}
	})
}

// =============================================================================
// SB event handler (implements cache.EventHandler)
// =============================================================================

type sbEventHandler struct {
	ovn *OVNClient
}

func (h *sbEventHandler) OnAdd(table string, m model.Model) {
	if h.isChassisRedirect(table, m) {
		slog.Debug("chassisredirect port added, immediate refresh")
		h.ovn.immediateStateRefresh()
		return
	}
	h.handleChange(table)
}

func (h *sbEventHandler) OnUpdate(table string, old, new model.Model) {
	if h.isChassisRedirect(table, new) {
		slog.Debug("chassisredirect port updated, immediate refresh")
		h.ovn.immediateStateRefresh()
		return
	}
	h.handleChange(table)
}

func (h *sbEventHandler) OnDelete(table string, m model.Model) {
	if h.isChassisRedirect(table, m) {
		slog.Debug("chassisredirect port deleted, immediate refresh")
		h.ovn.immediateStateRefresh()
		return
	}
	h.handleChange(table)
}

func (h *sbEventHandler) isChassisRedirect(table string, m model.Model) bool {
	if table != "Port_Binding" {
		return false
	}
	pb, ok := m.(*SBPortBinding)
	return ok && pb.Type == "chassisredirect"
}

func (h *sbEventHandler) handleChange(table string) {
	slog.Debug("SB change detected", "table", table)
	if table == "Port_Binding" || table == "Chassis" {
		h.ovn.debounceStateRefresh()
	}
}

// =============================================================================
// NB event handler (implements cache.EventHandler)
// =============================================================================

type nbEventHandler struct {
	ovn *OVNClient
}

func (h *nbEventHandler) OnAdd(table string, m model.Model) {
	h.handleChange(table)
}

func (h *nbEventHandler) OnUpdate(table string, old, new model.Model) {
	h.handleChange(table)
}

func (h *nbEventHandler) OnDelete(table string, m model.Model) {
	h.handleChange(table)
}

func (h *nbEventHandler) handleChange(table string) {
	slog.Debug("NB change detected", "table", table)
	switch table {
	case "NAT", "Logical_Router", "Logical_Router_Port":
		h.ovn.debounceStateRefresh()
	}
}

// =============================================================================
// Helpers
// =============================================================================

func sbDatabaseModel() (model.ClientDBModel, error) {
	return model.NewClientDBModel("OVN_Southbound", map[string]model.Model{
		"Port_Binding": &SBPortBinding{},
		"Chassis":      &SBChassis{},
	})
}

func nbDatabaseModel() (model.ClientDBModel, error) {
	return model.NewClientDBModel("OVN_Northbound", map[string]model.Model{
		"NAT":                          &NBNAT{},
		"Logical_Router":               &NBLogicalRouter{},
		"Logical_Router_Port":          &NBLogicalRouterPort{},
		"Logical_Router_Static_Route":  &NBLogicalRouterStaticRoute{},
		"Static_MAC_Binding":           &NBStaticMACBinding{},
	})
}

func ovsdbEndpoints(remote string) []string {
	parts := strings.Split(remote, ",")
	var endpoints []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			endpoints = append(endpoints, p)
		}
	}
	return endpoints
}

// uniqueNetworks deduplicates and sorts a list of *net.IPNet by string representation.
func uniqueNetworks(nets []*net.IPNet) []*net.IPNet {
	seen := make(map[string]bool, len(nets))
	var result []*net.IPNet
	for _, n := range nets {
		key := n.String()
		if !seen[key] {
			seen[key] = true
			result = append(result, n)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].String() < result[j].String()
	})
	return result
}

func getHostname() (string, error) {
	h, err := os.Hostname()
	if err != nil {
		return "", err
	}
	if idx := strings.IndexByte(h, '.'); idx != -1 {
		h = h[:idx]
	}
	return h, nil
}
