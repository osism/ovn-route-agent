package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

// transactOps executes OVSDB operations and checks both the transport-level
// error and the per-operation results for OVSDB errors (constraint violations, etc.).
// libovsdb v0.7.0's Transact does NOT check OperationResult errors itself.
func (o *OVNClient) transactOps(ctx context.Context, ops []ovsdb.Operation) error {
	results, err := o.nbClient.Transact(ctx, ops...)
	if err != nil {
		return err
	}
	opErrors, err := ovsdb.CheckOperationResults(results, ops)
	if err != nil {
		for i, opErr := range opErrors {
			slog.Error("OVSDB operation error",
				"index", i,
				"op", ops[i].Op,
				"table", ops[i].Table,
				"error", opErr.Error(),
			)
		}
		return err
	}
	return nil
}

// virtualGatewayIP computes the virtual gateway IP for a router's external network.
// It takes the last usable IP in the subnet (e.g., .254 for a /24).
func virtualGatewayIP(lrpNetworks []string) (net.IP, error) {
	for _, cidr := range lrpNetworks {
		ip, ipNet, err := net.ParseCIDR(cidr)
		if err != nil || ip.To4() == nil {
			continue
		}
		// Compute last usable IP: broadcast - 1.
		ones, bits := ipNet.Mask.Size()
		if ones == 32 || bits != 32 {
			continue
		}
		netIP := ipNet.IP.To4()
		hostBits := uint32(1)<<(32-ones) - 1
		netInt := binary.BigEndian.Uint32(netIP)
		lastUsable := netInt | uint32(hostBits) - 1 // broadcast - 1
		result := make(net.IP, 4)
		binary.BigEndian.PutUint32(result, lastUsable)
		return result, nil
	}
	return nil, fmt.Errorf("no valid IPv4 CIDR in LRP networks: %v", lrpNetworks)
}

// EnsureGatewayRouting ensures that each locally-active router has a default
// route and a static MAC binding pointing to the local br-ex interface.
// This allows OVN to route reply traffic out the external port and into
// the kernel for further routing via the VRF.
func (o *OVNClient) EnsureGatewayRouting(ctx context.Context, localRouters []LocalRouterInfo, bridgeMAC string) error {
	for _, lr := range localRouters {
		vgwIP, err := virtualGatewayIP(lr.LRPNetworks)
		if err != nil {
			slog.Error("cannot compute virtual gateway IP", "router", lr.RouterName, "error", err)
			continue
		}
		vgwStr := vgwIP.String()

		if err := o.ensureDefaultRoute(ctx, lr, vgwStr); err != nil {
			slog.Error("failed to ensure default route", "router", lr.RouterName, "vgw", vgwStr, "error", err)
			continue
		}
		if err := o.ensureStaticMACBinding(ctx, lr.LRPName, vgwStr, bridgeMAC); err != nil {
			slog.Error("failed to ensure static MAC binding", "router", lr.RouterName, "lrp", lr.LRPName, "error", err)
		}
	}
	return nil
}

// minActivePriority is the minimum priority the active chassis should
// maintain. Drained gateways restore to priority 1, so the active chassis
// must stay at least at 2 to prevent priority ties (and the resulting
// tiebreak flapping) when a peer restarts.
const minActivePriority = 2

// EnsureActivePriorityLead ensures that for each locally-active router the
// local Gateway_Chassis entry has a strictly higher priority than all peers
// in the same HA group, and at least minActivePriority. The minimum
// prevents reverse failovers when a drained chassis restores to priority 1
// and the active chassis is also at 1 (because it never boosted while the
// peer was at 0).
func (o *OVNClient) EnsureActivePriorityLead(ctx context.Context, localRouters []LocalRouterInfo, localChassisName string) error {
	var gwChassisList []NBGatewayChassis
	if err := o.nbClient.List(ctx, &gwChassisList); err != nil {
		return fmt.Errorf("list gateway chassis: %w", err)
	}

	// Build set of locally-active LRP names.
	activeLRPs := make(map[string]bool, len(localRouters))
	for _, lr := range localRouters {
		activeLRPs[lr.LRPName] = true
	}

	// Group Gateway_Chassis entries by LRP, tracking local entry and max peer priority.
	type haGroup struct {
		local   *NBGatewayChassis
		maxPeer int
	}
	groups := make(map[string]*haGroup)

	for i := range gwChassisList {
		gwc := &gwChassisList[i]
		// Gateway_Chassis name format: {lrp_name}_{chassis_name}
		lrpName := strings.TrimSuffix(gwc.Name, "_"+gwc.ChassisName)
		if !activeLRPs[lrpName] {
			continue
		}

		g, ok := groups[lrpName]
		if !ok {
			g = &haGroup{maxPeer: -1}
			groups[lrpName] = g
		}

		if gwc.ChassisName == localChassisName {
			g.local = gwc
		} else if gwc.Priority > g.maxPeer {
			g.maxPeer = gwc.Priority
		}
	}

	var allOps []ovsdb.Operation
	for lrpName, g := range groups {
		if g.local == nil || g.maxPeer < 0 {
			continue // No local entry or no peers
		}
		if g.local.Priority > g.maxPeer && g.local.Priority >= minActivePriority {
			continue // Already has the lead with safe margin
		}
		newPriority := g.maxPeer + 1
		if newPriority < minActivePriority {
			newPriority = minActivePriority
		}
		slog.Info("boosting gateway chassis priority to maintain active lead",
			"lrp", lrpName, "chassis", localChassisName,
			"old_priority", g.local.Priority, "new_priority", newPriority)
		g.local.Priority = newPriority
		ops, err := o.nbClient.Where(g.local).Update(g.local, &g.local.Priority)
		if err != nil {
			slog.Error("failed to build priority boost op", "lrp", lrpName, "error", err)
			continue
		}
		allOps = append(allOps, ops...)
	}

	if len(allOps) == 0 {
		return nil
	}
	return o.transactOps(ctx, allOps)
}

// ensureDefaultRoute adds 0.0.0.0/0 via <vgwIP> on the router if not already present.
func (o *OVNClient) ensureDefaultRoute(ctx context.Context, lr LocalRouterInfo, vgwIP string) error {
	// Check if route already exists.
	var routes []NBLogicalRouterStaticRoute
	if err := o.nbClient.List(ctx, &routes); err != nil {
		return fmt.Errorf("list static routes: %w", err)
	}

	// Find the router to get its static_routes UUIDs.
	var routers []NBLogicalRouter
	if err := o.nbClient.List(ctx, &routers); err != nil {
		return fmt.Errorf("list routers: %w", err)
	}
	var router *NBLogicalRouter
	for i, r := range routers {
		if r.UUID == lr.RouterUUID {
			router = &routers[i]
			break
		}
	}
	if router == nil {
		return fmt.Errorf("router %s not found", lr.RouterUUID)
	}

	routeUUIDs := make(map[string]bool, len(router.StaticRoutes))
	for _, uuid := range router.StaticRoutes {
		routeUUIDs[uuid] = true
	}

	for _, route := range routes {
		if !routeUUIDs[route.UUID] {
			continue
		}
		if route.IPPrefix == "0.0.0.0/0" {
			// Check if this route was created by the agent.
			isManaged := route.ExternalIDs != nil && route.ExternalIDs["ovn-route-agent"] == "managed"

			if !isManaged {
				// A default route exists that was NOT created by the agent
				// (e.g., OpenStack configured a real gateway). Leave it alone.
				slog.Debug("default route exists (not managed by agent, skipping)",
					"router", lr.RouterName, "nexthop", route.Nexthop)
				return nil
			}

			localChassis := o.state.LocalChassisName

			if route.Nexthop == vgwIP {
				// Nexthop correct — check if chassis tag needs updating (migration or failover).
				if route.ExternalIDs["ovn-route-agent-chassis"] != localChassis {
					slog.Info("updating default route chassis tag",
						"router", lr.RouterName, "old_chassis", route.ExternalIDs["ovn-route-agent-chassis"], "new_chassis", localChassis)
					route.ExternalIDs["ovn-route-agent-chassis"] = localChassis
					ops, err := o.nbClient.Where(&route).Update(&route, &route.ExternalIDs)
					if err != nil {
						return fmt.Errorf("build update op: %w", err)
					}
					return o.transactOps(ctx, ops)
				}
				slog.Debug("default route already exists", "router", lr.RouterName, "vgw", vgwIP)
				return nil
			}

			// Agent-managed route with wrong next-hop — update it.
			slog.Info("updating default route next-hop", "router", lr.RouterName, "old", route.Nexthop, "new", vgwIP)
			route.Nexthop = vgwIP
			outputPort := lr.LRPName
			route.OutputPort = &outputPort
			route.ExternalIDs["ovn-route-agent-chassis"] = localChassis
			ops, err := o.nbClient.Where(&route).Update(&route, &route.Nexthop, &route.OutputPort, &route.ExternalIDs)
			if err != nil {
				return fmt.Errorf("build update op: %w", err)
			}
			return o.transactOps(ctx, ops)
		}
	}

	// Route doesn't exist — create it.
	outputPort := lr.LRPName
	newRoute := &NBLogicalRouterStaticRoute{
		UUID:       "new_route",
		IPPrefix:   "0.0.0.0/0",
		Nexthop:    vgwIP,
		OutputPort: &outputPort,
		Options:    map[string]string{},
		ExternalIDs: map[string]string{
			"ovn-route-agent":         "managed",
			"ovn-route-agent-chassis": o.state.LocalChassisName,
		},
	}

	createOps, err := o.nbClient.Create(newRoute)
	if err != nil {
		return fmt.Errorf("build create op: %w", err)
	}

	// Add the route to the router's static_routes.
	mutateOp := ovsdb.Operation{
		Op:    "mutate",
		Table: "Logical_Router",
		Where: []ovsdb.Condition{{
			Column:   "_uuid",
			Function: ovsdb.ConditionEqual,
			Value:    ovsdb.UUID{GoUUID: lr.RouterUUID},
		}},
		Mutations: []ovsdb.Mutation{{
			Column:  "static_routes",
			Mutator: ovsdb.MutateOperationInsert,
			Value:   ovsdb.UUID{GoUUID: "new_route"},
		}},
	}

	allOps := append(createOps, mutateOp)
	if err := o.transactOps(ctx, allOps); err != nil {
		return fmt.Errorf("transact create route: %w", err)
	}

	slog.Info("default route created", "router", lr.RouterName, "vgw", vgwIP, "output_port", lr.LRPName)
	return nil
}

// ensureStaticMACBinding ensures a static MAC binding exists for the virtual
// gateway IP on the given logical router port, pointing to the local br-ex MAC.
func (o *OVNClient) ensureStaticMACBinding(ctx context.Context, lrpName, ip, mac string) error {
	var bindings []NBStaticMACBinding
	if err := o.nbClient.List(ctx, &bindings); err != nil {
		return fmt.Errorf("list static MAC bindings: %w", err)
	}

	for _, b := range bindings {
		if b.LogicalPort == lrpName && b.IP == ip {
			if b.MAC == mac {
				slog.Debug("static MAC binding already correct", "lrp", lrpName, "ip", ip, "mac", mac)
				return nil
			}
			// MAC changed (failover) — update.
			slog.Info("updating static MAC binding", "lrp", lrpName, "ip", ip, "old_mac", b.MAC, "new_mac", mac)
			b.MAC = mac
			ops, err := o.nbClient.Where(&b).Update(&b, &b.MAC)
			if err != nil {
				return fmt.Errorf("build update op: %w", err)
			}
			return o.transactOps(ctx, ops)
		}
	}

	// Create new binding.
	newBinding := &NBStaticMACBinding{
		UUID:        "new_mac_binding",
		LogicalPort: lrpName,
		IP:          ip,
		MAC:         mac,
	}

	ops, err := o.nbClient.Create(newBinding)
	if err != nil {
		return fmt.Errorf("build create op: %w", err)
	}
	if err := o.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("transact create MAC binding: %w", err)
	}

	slog.Info("static MAC binding created", "lrp", lrpName, "ip", ip, "mac", mac)
	return nil
}

// RemoveManagedNBEntries removes OVN NB entries created by this agent instance.
// Only entries belonging to locally-active routers are removed, so that agents
// running on other network nodes are not affected.
func (o *OVNClient) RemoveManagedNBEntries(ctx context.Context) error {
	state := o.GetState()

	// Build set of locally-active router UUIDs and LRP names.
	localRouterUUIDs := make(map[string]bool, len(state.LocalRouters))
	localPorts := make(map[string]bool, len(state.LocalRouters))
	for _, lr := range state.LocalRouters {
		localRouterUUIDs[lr.RouterUUID] = true
		localPorts[lr.LRPName] = true
	}

	if len(localRouterUUIDs) == 0 {
		slog.Debug("no locally-active routers, skipping OVN NB cleanup")
		return nil
	}

	// Remove managed static routes on locally-active routers.
	var routes []NBLogicalRouterStaticRoute
	if err := o.nbClient.List(ctx, &routes); err != nil {
		return fmt.Errorf("list static routes: %w", err)
	}

	var routers []NBLogicalRouter
	if err := o.nbClient.List(ctx, &routers); err != nil {
		return fmt.Errorf("list routers: %w", err)
	}

	for _, route := range routes {
		if route.ExternalIDs == nil || route.ExternalIDs["ovn-route-agent"] != "managed" {
			continue
		}

		// Find the router that owns this route — only process local routers.
	nextRoute:
		for _, router := range routers {
			if !localRouterUUIDs[router.UUID] {
				continue
			}
			for _, ruuid := range router.StaticRoutes {
				if ruuid != route.UUID {
					continue
				}
				mutateOp := ovsdb.Operation{
					Op:    "mutate",
					Table: "Logical_Router",
					Where: []ovsdb.Condition{{
						Column:   "_uuid",
						Function: ovsdb.ConditionEqual,
						Value:    ovsdb.UUID{GoUUID: router.UUID},
					}},
					Mutations: []ovsdb.Mutation{{
						Column:  "static_routes",
						Mutator: ovsdb.MutateOperationDelete,
						Value:   ovsdb.UUID{GoUUID: route.UUID},
					}},
				}
				deleteOps, err := o.nbClient.Where(&route).Delete()
				if err != nil {
					slog.Error("failed to build delete op for managed route", "uuid", route.UUID, "error", err)
					break nextRoute
				}
				allOps := append([]ovsdb.Operation{mutateOp}, deleteOps...)
				if err := o.transactOps(ctx, allOps); err != nil {
					slog.Error("failed to remove managed route", "router", router.Name, "prefix", route.IPPrefix, "error", err)
				} else {
					slog.Info("managed OVN route removed", "router", router.Name, "prefix", route.IPPrefix, "nexthop", route.Nexthop)
				}
				break nextRoute
			}
		}
	}

	// Remove static MAC bindings on locally-active router ports.
	var bindings []NBStaticMACBinding
	if err := o.nbClient.List(ctx, &bindings); err != nil {
		return fmt.Errorf("list static MAC bindings: %w", err)
	}

	for _, b := range bindings {
		if !localPorts[b.LogicalPort] {
			continue
		}
		ops, err := o.nbClient.Where(&b).Delete()
		if err != nil {
			slog.Error("failed to build delete op for MAC binding", "lrp", b.LogicalPort, "ip", b.IP, "error", err)
			continue
		}
		if err := o.transactOps(ctx, ops); err != nil {
			slog.Error("failed to remove static MAC binding", "lrp", b.LogicalPort, "ip", b.IP, "error", err)
		} else {
			slog.Info("managed static MAC binding removed", "lrp", b.LogicalPort, "ip", b.IP, "mac", b.MAC)
		}
	}

	return nil
}

// ListManagedRouteChassis returns the set of chassis names referenced in
// ExternalIDs["ovn-route-agent-chassis"] of all managed static routes.
func (o *OVNClient) ListManagedRouteChassis(ctx context.Context) map[string]bool {
	var routes []NBLogicalRouterStaticRoute
	if err := o.nbClient.List(ctx, &routes); err != nil {
		slog.Error("failed to list static routes for chassis scan", "error", err)
		return nil
	}
	result := make(map[string]bool)
	for _, route := range routes {
		if route.ExternalIDs != nil && route.ExternalIDs["ovn-route-agent"] == "managed" {
			if ch := route.ExternalIDs["ovn-route-agent-chassis"]; ch != "" {
				result[ch] = true
			}
		}
	}
	return result
}

// staleMACBindingKey identifies a MAC binding created by the agent, derived
// from the managed route's OutputPort (= LRP name) and Nexthop (= VGW IP).
type staleMACBindingKey struct {
	LogicalPort string
	IP          string
}

// CleanupStaleChassisManagedEntries removes OVN NB entries (static routes and
// their corresponding MAC bindings) that were created by agents on chassis
// that no longer exist in the SB Chassis table.
// Only entries with ExternalIDs["ovn-route-agent"] == "managed" and a matching
// chassis tag are removed. Neutron-created entries are never touched.
func (o *OVNClient) CleanupStaleChassisManagedEntries(ctx context.Context, staleChassis map[string]bool) error {
	var routes []NBLogicalRouterStaticRoute
	if err := o.nbClient.List(ctx, &routes); err != nil {
		return fmt.Errorf("list static routes: %w", err)
	}

	var routers []NBLogicalRouter
	if err := o.nbClient.List(ctx, &routers); err != nil {
		return fmt.Errorf("list routers: %w", err)
	}

	// Build route UUID → router mapping.
	routeToRouter := make(map[string]*NBLogicalRouter)
	for i, router := range routers {
		for _, ruuid := range router.StaticRoutes {
			routeToRouter[ruuid] = &routers[i]
		}
	}

	// Collect (OutputPort, Nexthop) pairs for MAC binding correlation, and
	// track which output ports still have a live chassis owner.
	staleBindingKeys := make(map[staleMACBindingKey]bool)
	liveOutputPorts := make(map[string]bool)

	// First pass: identify which output ports are still owned by live chassis.
	for _, route := range routes {
		if route.ExternalIDs == nil || route.ExternalIDs["ovn-route-agent"] != "managed" {
			continue
		}
		ch := route.ExternalIDs["ovn-route-agent-chassis"]
		if ch != "" && !staleChassis[ch] && route.OutputPort != nil {
			liveOutputPorts[*route.OutputPort] = true
		}
	}

	// Second pass: find and delete stale routes.
	for _, route := range routes {
		if route.ExternalIDs == nil || route.ExternalIDs["ovn-route-agent"] != "managed" {
			continue
		}
		chassisName := route.ExternalIDs["ovn-route-agent-chassis"]
		if chassisName == "" {
			// Legacy route without chassis tag — skip, will be tagged on next reconcile.
			continue
		}
		if !staleChassis[chassisName] {
			continue
		}

		router := routeToRouter[route.UUID]
		if router == nil {
			continue
		}

		// Record (OutputPort, Nexthop) for MAC binding cleanup, but only if
		// no live chassis also has a managed route on the same output port.
		if route.OutputPort != nil && !liveOutputPorts[*route.OutputPort] {
			staleBindingKeys[staleMACBindingKey{
				LogicalPort: *route.OutputPort,
				IP:          route.Nexthop,
			}] = true
		}

		// Delete route: remove from router's static_routes, then delete the row.
		mutateOp := ovsdb.Operation{
			Op:    "mutate",
			Table: "Logical_Router",
			Where: []ovsdb.Condition{{
				Column:   "_uuid",
				Function: ovsdb.ConditionEqual,
				Value:    ovsdb.UUID{GoUUID: router.UUID},
			}},
			Mutations: []ovsdb.Mutation{{
				Column:  "static_routes",
				Mutator: ovsdb.MutateOperationDelete,
				Value:   ovsdb.UUID{GoUUID: route.UUID},
			}},
		}
		deleteOps, err := o.nbClient.Where(&route).Delete()
		if err != nil {
			slog.Error("failed to build delete op for stale route", "uuid", route.UUID, "error", err)
			continue
		}
		allOps := append([]ovsdb.Operation{mutateOp}, deleteOps...)
		if err := o.transactOps(ctx, allOps); err != nil {
			// Warn instead of Error: another agent may have already cleaned this up.
			slog.Warn("failed to remove stale route (may already be removed by another agent)",
				"chassis", chassisName, "router", router.Name, "prefix", route.IPPrefix, "error", err)
		} else {
			slog.Info("stale chassis route removed",
				"chassis", chassisName, "router", router.Name, "prefix", route.IPPrefix, "nexthop", route.Nexthop)
		}
	}

	// Delete MAC bindings that match the exact (LogicalPort, IP) pairs from stale routes.
	if len(staleBindingKeys) > 0 {
		var bindings []NBStaticMACBinding
		if err := o.nbClient.List(ctx, &bindings); err != nil {
			return fmt.Errorf("list static MAC bindings: %w", err)
		}
		for _, b := range bindings {
			key := staleMACBindingKey{LogicalPort: b.LogicalPort, IP: b.IP}
			if !staleBindingKeys[key] {
				continue
			}
			ops, err := o.nbClient.Where(&b).Delete()
			if err != nil {
				slog.Error("failed to build delete op for stale MAC binding", "lrp", b.LogicalPort, "ip", b.IP, "error", err)
				continue
			}
			if err := o.transactOps(ctx, ops); err != nil {
				// Warn instead of Error: another agent may have already cleaned this up.
				slog.Warn("failed to remove stale MAC binding (may already be removed by another agent)",
					"lrp", b.LogicalPort, "ip", b.IP, "error", err)
			} else {
				slog.Info("stale chassis MAC binding removed", "lrp", b.LogicalPort, "ip", b.IP, "mac", b.MAC)
			}
		}
	}

	return nil
}

// DrainGateways sets the Gateway_Chassis priority to 0 for this chassis on
// all locally-active router ports, causing OVN to migrate chassisredirect
// ports to standby chassis (which have priority >= 1). It then polls the
// SB Port_Binding table until all gateways have moved away or the context
// deadline is exceeded.
//
// On the next startup, RestoreDrainedGateways sets drained entries back to
// priority 1 (standby level) so the chassis rejoins the HA group.
// EnsureActivePriorityLead prevents reverse failover by ensuring the
// active chassis always has priority >= minActivePriority (currently 2),
// which is strictly above the restore level of 1.
func (o *OVNClient) DrainGateways(ctx context.Context, localChassisName string) error {
	// Step 1: Find all Gateway_Chassis entries for this chassis with priority > 0.
	var gwChassisList []NBGatewayChassis
	if err := o.nbClient.List(ctx, &gwChassisList); err != nil {
		return fmt.Errorf("list gateway chassis: %w", err)
	}

	var toDrain []NBGatewayChassis
	for _, gwc := range gwChassisList {
		if gwc.ChassisName == localChassisName && gwc.Priority > 0 {
			toDrain = append(toDrain, gwc)
		}
	}

	if len(toDrain) == 0 {
		slog.Info("drain: no gateway chassis entries to drain on this chassis")
		return nil
	}

	// Step 2: Set priority to 0 in a single batched transaction.
	var allOps []ovsdb.Operation
	for i := range toDrain {
		gwc := toDrain[i]
		oldPriority := gwc.Priority
		gwc.Priority = 0
		ops, err := o.nbClient.Where(&gwc).Update(&gwc, &gwc.Priority)
		if err != nil {
			slog.Error("drain: failed to build priority update", "name", gwc.Name, "error", err)
			continue
		}
		allOps = append(allOps, ops...)
		slog.Info("drain: gateway chassis priority lowered",
			"name", gwc.Name, "chassis", gwc.ChassisName,
			"old_priority", oldPriority, "new_priority", 0)
	}
	if len(allOps) > 0 {
		if err := o.transactOps(ctx, allOps); err != nil {
			return fmt.Errorf("drain: failed to lower gateway chassis priorities: %w", err)
		}
	}

	// Step 3: Poll SB until no chassisredirect ports remain on this chassis.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		remaining, err := o.countLocalCRPorts(ctx, localChassisName)
		if err != nil {
			return fmt.Errorf("drain: failed to query port bindings: %w", err)
		}
		if remaining == 0 {
			slog.Info("drain: complete, all gateways migrated away")
			return nil
		}
		slog.Info("drain: waiting for gateway migration", "remaining_gateways", remaining)

		select {
		case <-ctx.Done():
			slog.Warn("drain: timeout exceeded, proceeding with shutdown", "remaining_gateways", remaining)
			return nil
		case <-ticker.C:
		}
	}
}

// RestoreDrainedGateways sets Gateway_Chassis entries that were drained
// (priority 0) back to priority 1 (standby level). This re-adds the
// chassis to the HA group as a standby. The active chassis maintains a
// strictly higher priority via EnsureActivePriorityLead, which prevents
// reverse failover even when both chassis would otherwise have the same
// priority. This should be called on startup before the first reconciliation.
func (o *OVNClient) RestoreDrainedGateways(ctx context.Context, localChassisName string) {
	var gwChassisList []NBGatewayChassis
	if err := o.nbClient.List(ctx, &gwChassisList); err != nil {
		slog.Error("restore-drain: failed to list gateway chassis", "error", err)
		return
	}

	var allOps []ovsdb.Operation
	for _, gwc := range gwChassisList {
		if gwc.ChassisName != localChassisName || gwc.Priority != 0 {
			continue
		}

		gwc.Priority = 1
		ops, err := o.nbClient.Where(&gwc).Update(&gwc, &gwc.Priority)
		if err != nil {
			slog.Error("restore-drain: failed to build restore op", "name", gwc.Name, "error", err)
			continue
		}
		allOps = append(allOps, ops...)
		slog.Info("restore-drain: gateway chassis priority restored to standby",
			"name", gwc.Name, "chassis", gwc.ChassisName, "priority", 1)
	}

	if len(allOps) == 0 {
		return
	}
	if err := o.transactOps(ctx, allOps); err != nil {
		slog.Error("restore-drain: failed to restore gateway chassis priorities", "error", err)
	}
}

// countLocalCRPorts returns the number of chassisredirect ports currently
// bound to the given chassis hostname in the SB Port_Binding table.
func (o *OVNClient) countLocalCRPorts(ctx context.Context, localChassisName string) (int, error) {
	var portBindings []SBPortBinding
	if err := o.sbClient.List(ctx, &portBindings); err != nil {
		return 0, fmt.Errorf("list port bindings: %w", err)
	}

	var chassis []SBChassis
	if err := o.sbClient.List(ctx, &chassis); err != nil {
		return 0, fmt.Errorf("list chassis: %w", err)
	}

	chassisHostname := make(map[string]string, len(chassis))
	for _, ch := range chassis {
		chassisHostname[ch.UUID] = ch.Hostname
		chassisHostname[ch.Name] = ch.Hostname
	}

	count := 0
	for _, pb := range portBindings {
		if pb.Type != "chassisredirect" || pb.Chassis == nil || *pb.Chassis == "" {
			continue
		}
		if o.cfg.GatewayPort != "" && pb.LogicalPort != o.cfg.GatewayPort {
			continue
		}
		hostname := chassisHostname[*pb.Chassis]
		if strings.EqualFold(hostname, localChassisName) {
			count++
		}
	}
	return count, nil
}

