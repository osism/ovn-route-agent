//go:build integration

package testenv

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// NftRule is a parsed `nft -j list ruleset` rule with the expression list
// preserved as raw JSON-decoded values. Tests compose matchers over Expr
// rather than regex-matching the textual rule output.
type NftRule struct {
	Family string
	Table  string
	Chain  string
	Handle int
	Expr   []map[string]any
	Raw    json.RawMessage // the entire {"rule":{...}} object
}

// NftChain is a parsed chain header (one per `chain` element in the JSON
// output). Anonymous (unhooked) chains are not used by the agent, but Hook
// may be empty for them.
type NftChain struct {
	Family string
	Table  string
	Name   string
	Type   string
	Hook   string
	Prio   any // number or string, depending on nft version
	Policy string
}

// NftDump is the full parsed ruleset.
type NftDump struct {
	Tables []string
	Chains []NftChain
	Rules  []NftRule
}

// DumpNftRuleset shells out to `nft -j list ruleset` and parses the result.
// Fails the test on parse errors. Empty ruleset is not an error — the dump is
// returned with zero-length slices.
func DumpNftRuleset(t *testing.T) NftDump {
	t.Helper()
	out, err := exec.Command("nft", "-j", "list", "ruleset").CombinedOutput()
	if err != nil {
		t.Fatalf("nft -j list ruleset: %v (output: %s)", err, strings.TrimSpace(string(out)))
	}
	dump, err := parseNftDump(out)
	if err != nil {
		t.Fatalf("parse nft json: %v (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return dump
}

// parseNftDump is split out from DumpNftRuleset so unit tests (if added later)
// can exercise the parser without needing nft on the host.
func parseNftDump(data []byte) (NftDump, error) {
	var top struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		return NftDump{}, fmt.Errorf("decode top-level: %w", err)
	}
	var dump NftDump
	for _, item := range top.Nftables {
		// Each element has exactly one key (table, chain, rule, set, …).
		for kind, raw := range item {
			switch kind {
			case "table":
				var t struct {
					Family string `json:"family"`
					Name   string `json:"name"`
				}
				if err := json.Unmarshal(raw, &t); err != nil {
					return NftDump{}, fmt.Errorf("decode table: %w", err)
				}
				dump.Tables = append(dump.Tables, t.Family+":"+t.Name)
			case "chain":
				var c struct {
					Family string `json:"family"`
					Table  string `json:"table"`
					Name   string `json:"name"`
					Type   string `json:"type"`
					Hook   string `json:"hook"`
					Prio   any    `json:"prio"`
					Policy string `json:"policy"`
				}
				if err := json.Unmarshal(raw, &c); err != nil {
					return NftDump{}, fmt.Errorf("decode chain: %w", err)
				}
				dump.Chains = append(dump.Chains, NftChain{
					Family: c.Family, Table: c.Table, Name: c.Name,
					Type: c.Type, Hook: c.Hook, Prio: c.Prio, Policy: c.Policy,
				})
			case "rule":
				var r struct {
					Family string           `json:"family"`
					Table  string           `json:"table"`
					Chain  string           `json:"chain"`
					Handle int              `json:"handle"`
					Expr   []map[string]any `json:"expr"`
				}
				if err := json.Unmarshal(raw, &r); err != nil {
					return NftDump{}, fmt.Errorf("decode rule: %w", err)
				}
				dump.Rules = append(dump.Rules, NftRule{
					Family: r.Family, Table: r.Table, Chain: r.Chain,
					Handle: r.Handle, Expr: r.Expr, Raw: raw,
				})
			}
		}
	}
	return dump, nil
}

// HasTable returns true if the dump contains an `ip`-family table named name.
func (d NftDump) HasTable(name string) bool {
	for _, t := range d.Tables {
		if t == "ip:"+name {
			return true
		}
	}
	return false
}

// Chain returns the named (table, chain) header. ok=false if missing.
func (d NftDump) Chain(table, name string) (NftChain, bool) {
	for _, c := range d.Chains {
		if c.Table == table && c.Name == name {
			return c, true
		}
	}
	return NftChain{}, false
}

// RulesIn returns rules in the (table, chain), in their nft load order.
func (d NftDump) RulesIn(table, chain string) []NftRule {
	var out []NftRule
	for _, r := range d.Rules {
		if r.Table == table && r.Chain == chain {
			out = append(out, r)
		}
	}
	return out
}

// =============================================================================
// Rule-level matchers — composed over the parsed expression tree.
// All numeric values from the JSON arrive as float64 (encoding/json default);
// the matchers normalise both sides before comparing.
// =============================================================================

// HasMatch returns true iff the rule contains a {"match": {"op": "==",
// "left": {"payload": {"protocol": <protocol>, "field": <field>}},
// "right": <value>}} expression.
func (r NftRule) HasMatch(protocol, field string, value any) bool {
	for _, stmt := range r.Expr {
		m, ok := stmt["match"].(map[string]any)
		if !ok {
			continue
		}
		if op, _ := m["op"].(string); op != "==" {
			continue
		}
		left, _ := m["left"].(map[string]any)
		payload, _ := left["payload"].(map[string]any)
		if p, _ := payload["protocol"].(string); p != protocol {
			continue
		}
		if f, _ := payload["field"].(string); f != field {
			continue
		}
		if jsonEq(m["right"], value) {
			return true
		}
	}
	return false
}

// HasMatchPrefix asserts the rule has an `ip saddr <addr>/<len>`-style match
// where the right-hand side is a CIDR. nft renders these as {"match": {"op":
// "==", "left": {"payload": {...}}, "right": {"prefix": {"addr": addr,
// "len": len}}}}.
func (r NftRule) HasMatchPrefix(protocol, field, cidr string) bool {
	addr, prefixLen, ok := splitCIDR(cidr)
	if !ok {
		return false
	}
	for _, stmt := range r.Expr {
		m, ok := stmt["match"].(map[string]any)
		if !ok {
			continue
		}
		if op, _ := m["op"].(string); op != "==" {
			continue
		}
		left, _ := m["left"].(map[string]any)
		payload, _ := left["payload"].(map[string]any)
		if p, _ := payload["protocol"].(string); p != protocol {
			continue
		}
		if f, _ := payload["field"].(string); f != field {
			continue
		}
		right, _ := m["right"].(map[string]any)
		prefix, _ := right["prefix"].(map[string]any)
		if a, _ := prefix["addr"].(string); a != addr {
			continue
		}
		if !jsonEq(prefix["len"], prefixLen) {
			continue
		}
		return true
	}
	return false
}

// HasDNATTo asserts the rule has a single-target {"dnat": {"addr": addr,
// "port": port, "family": "ip"}} statement. Pass port=0 to skip the port check.
func (r NftRule) HasDNATTo(addr string, port int) bool {
	for _, stmt := range r.Expr {
		d, ok := stmt["dnat"].(map[string]any)
		if !ok {
			continue
		}
		if a, _ := d["addr"].(string); a != addr {
			continue
		}
		if port != 0 {
			if !jsonEq(d["port"], port) {
				continue
			}
		}
		return true
	}
	return false
}

// HasDNATMap asserts the rule has a jhash-based DNAT map containing exactly
// the given backends (ordered, indexed 0..n-1). Implementation note: nft
// renders multi-backend rules as `dnat to jhash ip saddr mod N map { ... }`,
// which decodes as a {"dnat":{"addr":{"map":{"key":{"jhash":...},"data":{"set":[...]}}}}}
// expression. We walk the structure and verify the `set` payload pairs
// `[index, [addr, port]]` against the expected list.
func (r NftRule) HasDNATMap(backends []NftBackend) bool {
	for _, stmt := range r.Expr {
		d, ok := stmt["dnat"].(map[string]any)
		if !ok {
			continue
		}
		addr, ok := d["addr"].(map[string]any)
		if !ok {
			continue
		}
		mp, ok := addr["map"].(map[string]any)
		if !ok {
			continue
		}
		// data is `{"set": [[idx, [addr, port]], ...]}`.
		data, ok := mp["data"].(map[string]any)
		if !ok {
			continue
		}
		set, ok := data["set"].([]any)
		if !ok || len(set) != len(backends) {
			continue
		}
		ok = true
		for i, entry := range set {
			pair, _ := entry.([]any)
			if len(pair) != 2 {
				ok = false
				break
			}
			if !jsonEq(pair[0], i) {
				ok = false
				break
			}
			target, _ := pair[1].(map[string]any)
			concat, _ := target["concat"].([]any)
			if len(concat) != 2 {
				ok = false
				break
			}
			if a, _ := concat[0].(string); a != backends[i].Addr {
				ok = false
				break
			}
			if !jsonEq(concat[1], backends[i].Port) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// NftBackend is one entry in a multi-backend DNAT map.
type NftBackend struct {
	Addr string
	Port int
}

// HasCTZoneSet asserts the rule has a {"mangle": {"key": {"ct": {"key":
// "zone"}}, "value": <zone>}} statement (nft renders `ct zone set N` as a
// mangle on the ct zone key).
func (r NftRule) HasCTZoneSet(zone int) bool {
	for _, stmt := range r.Expr {
		m, ok := stmt["mangle"].(map[string]any)
		if !ok {
			continue
		}
		key, _ := m["key"].(map[string]any)
		ct, _ := key["ct"].(map[string]any)
		if k, _ := ct["key"].(string); k != "zone" {
			continue
		}
		if jsonEq(m["value"], zone) {
			return true
		}
	}
	return false
}

// HasMarkSet asserts the rule has a {"mangle": {"key": {"meta": {"key":
// "mark"}}, "value": <mark>}} statement (nft renders `meta mark set N` as a
// mangle on meta mark).
func (r NftRule) HasMarkSet(mark int) bool {
	for _, stmt := range r.Expr {
		m, ok := stmt["mangle"].(map[string]any)
		if !ok {
			continue
		}
		key, _ := m["key"].(map[string]any)
		meta, _ := key["meta"].(map[string]any)
		if k, _ := meta["key"].(string); k != "mark" {
			continue
		}
		if jsonEq(m["value"], mark) {
			return true
		}
	}
	return false
}

// HasMasquerade asserts the rule contains a {"masquerade": null} (or empty
// map) statement.
func (r NftRule) HasMasquerade() bool {
	for _, stmt := range r.Expr {
		if _, ok := stmt["masquerade"]; ok {
			return true
		}
	}
	return false
}

// HasCTStatusDNAT asserts the rule matches `ct status dnat`. nft renders this
// as {"match": {"op": "in", "left": {"ct": {"key": "status"}}, "right": "dnat"}}.
func (r NftRule) HasCTStatusDNAT() bool {
	for _, stmt := range r.Expr {
		m, ok := stmt["match"].(map[string]any)
		if !ok {
			continue
		}
		left, _ := m["left"].(map[string]any)
		ct, _ := left["ct"].(map[string]any)
		if k, _ := ct["key"].(string); k != "status" {
			continue
		}
		if right, _ := m["right"].(string); right == "dnat" {
			return true
		}
	}
	return false
}

// =============================================================================
// Polling wrappers
// =============================================================================

// EventuallyNft polls DumpNftRuleset every 200ms until predicate returns
// true or timeout expires. msg is included in the failure message.
func EventuallyNft(t *testing.T, predicate func(NftDump) bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		// Allow nft to be empty-ruleset transient — that's not an error.
		out, err := exec.Command("nft", "-j", "list", "ruleset").CombinedOutput()
		if err == nil {
			if dump, perr := parseNftDump(out); perr == nil && predicate(dump) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("EventuallyNft timed out after %s: %s", timeout, msg)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// AssertNftTableAbsent fails the test if the named ip-family table persists
// past timeout. Used to verify cleanup on SIGTERM.
func AssertNftTableAbsent(t *testing.T, table string, timeout time.Duration) {
	t.Helper()
	EventuallyNft(t, func(d NftDump) bool { return !d.HasTable(table) }, timeout,
		fmt.Sprintf("table ip %s should be absent", table))
}

// AssertNftRuleInChain fails the test if no rule in (table, chain) satisfies
// match within timeout. msg is included in the failure message.
func AssertNftRuleInChain(t *testing.T, table, chain string, match func(NftRule) bool, timeout time.Duration, msg string) {
	t.Helper()
	EventuallyNft(t, func(d NftDump) bool {
		for _, r := range d.RulesIn(table, chain) {
			if match(r) {
				return true
			}
		}
		return false
	}, timeout, fmt.Sprintf("chain %s/%s: %s", table, chain, msg))
}

// AssertNftChainExists fails the test if the named chain is missing within
// timeout. The chain is identified by (table, name).
func AssertNftChainExists(t *testing.T, table, chain string, timeout time.Duration) {
	t.Helper()
	EventuallyNft(t, func(d NftDump) bool {
		_, ok := d.Chain(table, chain)
		return ok
	}, timeout, fmt.Sprintf("chain %s/%s should exist", table, chain))
}

// =============================================================================
// JSON value comparison helpers
// =============================================================================

// jsonEq compares a JSON-decoded value (typically float64 for numbers) against
// a Go literal. It treats numeric types as equal if they have the same value.
func jsonEq(decoded, expected any) bool {
	if decoded == nil {
		return expected == nil
	}
	// Numeric: encoding/json decodes JSON numbers to float64, but tests
	// pass int/int64 literals.
	if df, ok := numeric(decoded); ok {
		if ef, ok := numeric(expected); ok {
			return df == ef
		}
	}
	// Strings, bools, slices, maps fall through to deep equality.
	return fmt.Sprintf("%v", decoded) == fmt.Sprintf("%v", expected)
}

func numeric(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	}
	return 0, false
}

// splitCIDR parses "addr/len" into its components without bringing in net.
// Returns false on a malformed CIDR; tests catch this as a "no match" rather
// than a hard fail because matchers are designed to be composable.
func splitCIDR(cidr string) (string, int, bool) {
	for i := len(cidr) - 1; i >= 0; i-- {
		if cidr[i] == '/' {
			n, err := parsePositiveInt(cidr[i+1:])
			if err != nil {
				return "", 0, false
			}
			return cidr[:i], n, true
		}
	}
	return "", 0, false
}

func parsePositiveInt(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("non-digit")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
