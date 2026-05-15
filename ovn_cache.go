package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

// =============================================================================
// Monitor-cache consistency guard
// =============================================================================
//
// refreshState builds the agent's entire view of FIPs and locally-active
// routers from the libovsdb monitor cache. That cache occasionally drops
// table INSERTs delivered shortly after a fresh monitor subscription (issue
// #115): the cache then stays permanently short of rows for the lifetime of
// the connection, and a cache-backed List() cannot see its own gap.
//
// A short cache is silently dangerous here. A missing NAT/router row shrinks
// the desired-IP set, and ensureRoutes then deletes the corresponding FRR
// /32 as "orphaned" — the FIP stops being advertised even though OVN itself
// is perfectly healthy. The drain and active-lead paths already guard against
// this for Gateway_Chassis via a direct NB select; the helpers below extend
// the same defence to every table refreshState reads.
//
// The cache stays the primary, fast path. On every refresh the row set is
// cross-checked against a cheap server-side _uuid select; only on a genuine
// mismatch is the slice rebuilt from a full direct select.

// cachedList lists a table from the monitor cache and then cross-checks the
// row set against the OVN server. It returns a server-authoritative slice
// whenever the monitor cache turns out to be incomplete. uuidOf extracts the
// UUID of a cached model; decode rebuilds a model from a raw select row.
func cachedList[T any](
	ctx context.Context,
	c ovsdbClient,
	table string,
	uuidOf func(T) string,
	decode func(ovsdb.Row) T,
) ([]T, error) {
	var cached []T
	if err := c.List(ctx, &cached); err != nil {
		return nil, err
	}
	return authoritativeList(ctx, c, table, cached, uuidOf, decode), nil
}

// authoritativeList returns the rows of table that refreshState should act on.
// It trusts the monitor-cache snapshot (cached) unless a direct server select
// proves it incomplete, in which case it logs the gap and rebuilds the slice
// from a full select. Any error reading from the server degrades gracefully to
// the cache: a transient OVSDB blip must not stall reconciliation.
func authoritativeList[T any](
	ctx context.Context,
	c ovsdbClient,
	table string,
	cached []T,
	uuidOf func(T) string,
	decode func(ovsdb.Row) T,
) []T {
	serverUUIDs, err := serverUUIDSet(ctx, c, table)
	if err != nil {
		slog.Warn("cache consistency check skipped: direct select failed, trusting monitor cache",
			"table", table, "error", err)
		return cached
	}

	cacheUUIDs := make(map[string]bool, len(cached))
	for _, m := range cached {
		cacheUUIDs[uuidOf(m)] = true
	}
	if uuidSetsEqual(cacheUUIDs, serverUUIDs) {
		return cached
	}

	slog.Error("OVN monitor cache out of sync with the server, rebuilding from a direct select",
		"table", table,
		"cache_rows", len(cacheUUIDs),
		"server_rows", len(serverUUIDs))

	rows, err := directSelect(ctx, c, table, nil)
	if err != nil {
		slog.Error("cache recovery select failed, falling back to the monitor cache",
			"table", table, "error", err)
		return cached
	}
	out := make([]T, 0, len(rows))
	for _, row := range rows {
		out = append(out, decode(row))
	}
	return out
}

// serverUUIDSet returns the set of _uuid values currently in table on the OVN
// server. Only the _uuid column is selected so the common (in-sync) path stays
// cheap.
func serverUUIDSet(ctx context.Context, c ovsdbClient, table string) (map[string]bool, error) {
	rows, err := directSelect(ctx, c, table, []string{"_uuid"})
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(rows))
	for _, row := range rows {
		if u := rowUUID(row); u != "" {
			set[u] = true
		}
	}
	return set, nil
}

// directSelect runs an OVSDB select on table, bypassing the monitor cache.
// With columns nil every column is returned; otherwise only the named ones.
// Where is left empty — Operation.MarshalJSON encodes a conditionless select
// as the match-everything form.
func directSelect(ctx context.Context, c ovsdbClient, table string, columns []string) ([]ovsdb.Row, error) {
	op := ovsdb.Operation{
		Op:      ovsdb.OperationSelect,
		Table:   table,
		Columns: columns,
	}
	results, err := c.Transact(ctx, op)
	if err != nil {
		return nil, fmt.Errorf("select from %s: %w", table, err)
	}
	if _, err := ovsdb.CheckOperationResults(results, []ovsdb.Operation{op}); err != nil {
		return nil, fmt.Errorf("select from %s: %w", table, err)
	}
	if len(results) == 0 {
		return nil, nil
	}
	return results[0].Rows, nil
}

// uuidSetsEqual reports whether two UUID sets contain exactly the same members.
func uuidSetsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// =============================================================================
// Raw OVSDB row decoders
// =============================================================================
//
// These rebuild the typed models from the raw rows returned by directSelect.
// Only the columns refreshState actually consumes are decoded; everything else
// is left at its zero value. OVSDB encodes a set of exactly one element as the
// bare element and larger sets as an OvsSet, so the helpers normalise both.

func decodeSBPortBinding(row ovsdb.Row) SBPortBinding {
	return SBPortBinding{
		UUID:         rowUUID(row),
		Type:         rowString(row, "type"),
		LogicalPort:  rowString(row, "logical_port"),
		Chassis:      rowOptString(row, "chassis"),
		Options:      rowStringMap(row, "options"),
		ExternalIDs:  rowStringMap(row, "external_ids"),
		NatAddresses: rowStringSet(row, "nat_addresses"),
	}
}

func decodeSBChassis(row ovsdb.Row) SBChassis {
	return SBChassis{
		UUID:     rowUUID(row),
		Name:     rowString(row, "name"),
		Hostname: rowString(row, "hostname"),
	}
}

func decodeNBNAT(row ovsdb.Row) NBNAT {
	return NBNAT{
		UUID:       rowUUID(row),
		Type:       rowString(row, "type"),
		ExternalIP: rowString(row, "external_ip"),
	}
}

func decodeNBLogicalRouter(row ovsdb.Row) NBLogicalRouter {
	return NBLogicalRouter{
		UUID:  rowUUID(row),
		Name:  rowString(row, "name"),
		Ports: rowStringSet(row, "ports"),
		Nat:   rowStringSet(row, "nat"),
	}
}

func decodeNBLogicalRouterPort(row ovsdb.Row) NBLogicalRouterPort {
	return NBLogicalRouterPort{
		UUID:     rowUUID(row),
		Name:     rowString(row, "name"),
		MAC:      rowString(row, "mac"),
		Networks: rowStringSet(row, "networks"),
	}
}

// rowUUID extracts the _uuid column from a raw OVSDB row.
func rowUUID(row ovsdb.Row) string {
	if u, ok := row["_uuid"].(ovsdb.UUID); ok {
		return u.GoUUID
	}
	return ""
}

// rowString returns a scalar string column, or "" when absent.
func rowString(row ovsdb.Row, col string) string {
	s, _ := row[col].(string)
	return s
}

// rowOptString returns an optional (set-of-max-1) column as a pointer. OVSDB
// sends an empty optional as an empty set and a present one as the bare value;
// UUID references are flattened to their GoUUID string.
func rowOptString(row ovsdb.Row, col string) *string {
	switch v := row[col].(type) {
	case string:
		return &v
	case ovsdb.UUID:
		s := v.GoUUID
		return &s
	case ovsdb.OvsSet:
		if vals := ovsSetToStrings(v); len(vals) > 0 {
			return &vals[0]
		}
	}
	return nil
}

// rowStringSet returns a set-valued column as a Go slice.
func rowStringSet(row ovsdb.Row, col string) []string {
	return ovsSetToStrings(row[col])
}

// ovsSetToStrings flattens an OVSDB set value into a string slice, handling the
// bare-single-element encoding and flattening UUID references to GoUUID.
func ovsSetToStrings(v any) []string {
	switch s := v.(type) {
	case string:
		return []string{s}
	case ovsdb.UUID:
		return []string{s.GoUUID}
	case ovsdb.OvsSet:
		out := make([]string, 0, len(s.GoSet))
		for _, e := range s.GoSet {
			switch ev := e.(type) {
			case string:
				out = append(out, ev)
			case ovsdb.UUID:
				out = append(out, ev.GoUUID)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	return nil
}

// rowStringMap returns a string→string map column, or nil when absent.
func rowStringMap(row ovsdb.Row, col string) map[string]string {
	m, ok := row[col].(ovsdb.OvsMap)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m.GoMap))
	for k, v := range m.GoMap {
		ks, kok := k.(string)
		vs, vok := v.(string)
		if kok && vok {
			out[ks] = vs
		}
	}
	return out
}
