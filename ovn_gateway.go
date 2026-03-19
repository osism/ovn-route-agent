package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"

	"github.com/ovn-org/libovsdb/ovsdb"
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

			if route.Nexthop == vgwIP {
				slog.Debug("default route already exists", "router", lr.RouterName, "vgw", vgwIP)
				return nil
			}

			// Agent-managed route with wrong next-hop — update it.
			slog.Info("updating default route next-hop", "router", lr.RouterName, "old", route.Nexthop, "new", vgwIP)
			route.Nexthop = vgwIP
			outputPort := lr.LRPName
			route.OutputPort = &outputPort
			ops, err := o.nbClient.Where(&route).Update(&route, &route.Nexthop, &route.OutputPort)
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
			"ovn-route-agent": "managed",
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

