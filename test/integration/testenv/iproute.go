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

// ipRuleEntry mirrors one element of `ip -j rule` output. iproute2 renders
// numbers as JSON numbers but a few fields (fwmark, fwmask, table) come back
// as strings — table may be a name ("main") or a numeric string ("201"), and
// fwmark/fwmask are hex strings like "0x100". We keep them as `any` and
// normalise in the matcher.
type ipRuleEntry struct {
	Priority int `json:"priority"`
	Fwmark   any `json:"fwmark"`
	Fwmask   any `json:"fwmask"`
	Table    any `json:"table"`
}

// ipRouteEntry mirrors one element of `ip -j route show table N` output.
// `dst` is "default" for the unbounded route; otherwise it is a CIDR string.
// `dev` may be absent for some route types — empty string treated as "any".
type ipRouteEntry struct {
	Dst     string `json:"dst"`
	Gateway string `json:"gateway"`
	Dev     string `json:"dev"`
	Table   any    `json:"table"`
}

// AssertIPRulePriority polls `ip -j rule show priority <prio>` and fails the
// test if no entry matches the (mark, table) pair within timeout.
//
// mark is compared against fwmark as a numeric value (hex strings from
// iproute2 are normalised before comparison). table is compared as a string:
// pass "main" / "201" / a configured name; the JSON value is coerced to its
// string form before comparison so callers do not need to know whether
// iproute2 emitted a name or a number.
//
// Why a dedicated helper: scenario tests previously checked these rules by
// grepping the text output of `ip rule show`, which is fragile against
// version-dependent column layout and benign whitespace changes. The JSON
// form has stable keys.
func AssertIPRulePriority(t *testing.T, prio int, mark int, table string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastOut string
	var lastErr error
	for {
		out, err := exec.Command("ip", "-j", "-4", "rule", "show", "priority", fmt.Sprintf("%d", prio)).CombinedOutput()
		lastOut = strings.TrimSpace(string(out))
		lastErr = err
		if err == nil {
			var entries []ipRuleEntry
			if jerr := json.Unmarshal(out, &entries); jerr == nil {
				for _, e := range entries {
					if e.Priority != prio {
						continue
					}
					if !markEquals(e.Fwmark, mark) {
						continue
					}
					if asString(e.Table) != table {
						continue
					}
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("ip rule priority %d with mark 0x%x table %s not present after %s (last output: %q, err: %v)",
				prio, mark, table, timeout, lastOut, lastErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// AssertRouteInTable polls `ip -j route show table <table>` and fails the
// test if no entry matches (dst, dev) within timeout.
//
//   - dst="" matches any destination; dst="default" matches the unbounded
//     route; otherwise dst must equal the JSON `dst` field verbatim
//     (typically a CIDR like "10.0.0.0/24" or "1.2.3.4").
//   - dev="" matches any device; otherwise dev must equal the JSON `dev` field.
//
// Why pass table as a string rather than int: ip(8) accepts both numeric
// IDs ("201") and named tables ("main"), and the JSON output mirrors the
// caller's spelling. Keeping the helper string-typed lets tests that
// customise port_forward_table_id pass the same value they configured.
func AssertRouteInTable(t *testing.T, table, dst, dev string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastOut string
	var lastErr error
	for {
		out, err := exec.Command("ip", "-j", "-4", "route", "show", "table", table).CombinedOutput()
		lastOut = strings.TrimSpace(string(out))
		lastErr = err
		if err == nil {
			var entries []ipRouteEntry
			if jerr := json.Unmarshal(out, &entries); jerr == nil {
				for _, e := range entries {
					if dst != "" && e.Dst != dst {
						continue
					}
					if dev != "" && e.Dev != dev {
						continue
					}
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("ip route in table %s with dst=%q dev=%q not present after %s (last output: %q, err: %v)",
				table, dst, dev, timeout, lastOut, lastErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// markEquals compares an iproute2-rendered fwmark (hex string like "0x100"
// or, for older versions, a JSON number) against an int. Returns false on
// any parse failure — the assertion loop will retry.
func markEquals(decoded any, want int) bool {
	switch v := decoded.(type) {
	case string:
		s := strings.TrimSpace(v)
		s = strings.TrimPrefix(s, "0x")
		s = strings.TrimPrefix(s, "0X")
		n := 0
		for _, c := range s {
			d := -1
			switch {
			case c >= '0' && c <= '9':
				d = int(c - '0')
			case c >= 'a' && c <= 'f':
				d = int(c-'a') + 10
			case c >= 'A' && c <= 'F':
				d = int(c-'A') + 10
			}
			if d < 0 {
				return false
			}
			n = n*16 + d
		}
		return n == want
	case float64:
		return int(v) == want
	case int:
		return v == want
	}
	return false
}

// asString coerces a JSON-decoded scalar to its string form. Used because
// `ip -j rule` emits the table field as either a name ("main") or a number
// (201), depending on whether the table has an /etc/iproute2/rt_tables entry.
func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		// Integer numbers come back as float64 from encoding/json; format
		// without a trailing ".0".
		return fmt.Sprintf("%d", int(x))
	case int:
		return fmt.Sprintf("%d", x)
	case nil:
		return ""
	}
	return fmt.Sprintf("%v", v)
}
