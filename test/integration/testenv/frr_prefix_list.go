//go:build integration

package testenv

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// FRRPrefixListEntry mirrors a single line in `vtysh -c "show ip prefix-list <name>"`
// output: `seq <N> permit <network> [ge X] [le Y]`.
type FRRPrefixListEntry struct {
	Seq    int
	Permit bool   // true for "permit", false for "deny"
	Prefix string // e.g. "198.51.100.0/24"
	Suffix string // everything after the prefix (e.g. "ge 32 le 32"), empty if none
	Raw    string // original trimmed line
}

// fetchFRRPrefixListEntries shells out to vtysh and parses the entries of a
// prefix-list. Returns nil entries (without erroring) if the list does not
// exist yet — FRR auto-creates lists on first entry, so a non-existent list
// is simply "empty" from the test's perspective.
func fetchFRRPrefixListEntries(name string) ([]FRRPrefixListEntry, string, error) {
	out, err := exec.Command("vtysh",
		"-c", "show ip prefix-list "+name,
	).CombinedOutput()
	raw := string(out)
	if err != nil {
		return nil, raw, fmt.Errorf("vtysh show ip prefix-list %s: %w (%s)",
			name, err, strings.TrimSpace(raw))
	}
	if strings.Contains(raw, "Can't find") || strings.TrimSpace(raw) == "" {
		return nil, raw, nil
	}

	var entries []FRRPrefixListEntry
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		fields := strings.Fields(line)
		// Need at least: seq <N> permit|deny <prefix>
		if len(fields) < 4 || fields[0] != "seq" {
			continue
		}
		seq, serr := strconv.Atoi(fields[1])
		if serr != nil {
			continue
		}
		var permit bool
		switch fields[2] {
		case "permit":
			permit = true
		case "deny":
			permit = false
		default:
			continue
		}
		entry := FRRPrefixListEntry{
			Seq:    seq,
			Permit: permit,
			Prefix: fields[3],
			Suffix: strings.Join(fields[4:], " "),
			Raw:    line,
		}
		entries = append(entries, entry)
	}
	return entries, raw, nil
}

// requireVtysh skips the test if vtysh is not on PATH. Setup() already gates
// on this for scenario tests, but this defensive check keeps the helpers
// reusable from contexts that did not go through Setup().
func requireVtysh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("vtysh"); err != nil {
		t.Skipf("vtysh not in PATH (run test/integration/setup.sh first): %v", err)
	}
}

// AssertFRRPrefixListContains polls `vtysh -c "show ip prefix-list <name>"` until
// it contains a `permit <cidr> ge 32 le 32` entry (the format the agent installs
// for discovered networks). Fails the test if the entry never appears.
func AssertFRRPrefixListContains(t *testing.T, name, cidr string, timeout time.Duration) {
	t.Helper()
	requireVtysh(t)
	deadline := time.Now().Add(timeout)
	var lastRaw string
	var lastErr error
	for {
		entries, raw, err := fetchFRRPrefixListEntries(name)
		lastRaw = raw
		lastErr = err
		if err == nil {
			for _, e := range entries {
				if e.Permit && e.Prefix == cidr && e.Suffix == "ge 32 le 32" {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("FRR prefix-list %q does not contain `permit %s ge 32 le 32` after %s (last output: %q, err: %v)",
				name, cidr, timeout, strings.TrimSpace(lastRaw), lastErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// AssertNoFRRPrefixListEntry polls until the prefix-list <name> no longer
// contains a `permit <cidr> ge 32 le 32` entry. Used to verify removal.
func AssertNoFRRPrefixListEntry(t *testing.T, name, cidr string, timeout time.Duration) {
	t.Helper()
	requireVtysh(t)
	deadline := time.Now().Add(timeout)
	var lastRaw string
	for {
		entries, raw, err := fetchFRRPrefixListEntries(name)
		lastRaw = raw
		if err == nil {
			present := false
			for _, e := range entries {
				if e.Permit && e.Prefix == cidr && e.Suffix == "ge 32 le 32" {
					present = true
					break
				}
			}
			if !present {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("FRR prefix-list %q still contains `permit %s ge 32 le 32` after %s (last output: %q)",
				name, cidr, timeout, strings.TrimSpace(lastRaw))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// AssertFRRPrefixListEmpty polls until the prefix-list <name> contains no
// managed entries (`permit ... ge 32 le 32`). Non-managed entries (different
// mask/length specifiers, or deny entries) are ignored — they are outside the
// agent's reconciliation surface. Used to verify scenarios 3 and 4 where the
// agent must clear out everything it owns.
func AssertFRRPrefixListEmpty(t *testing.T, name string, timeout time.Duration) {
	t.Helper()
	requireVtysh(t)
	deadline := time.Now().Add(timeout)
	var lastRaw string
	for {
		entries, raw, err := fetchFRRPrefixListEntries(name)
		lastRaw = raw
		if err == nil {
			managed := 0
			for _, e := range entries {
				if e.Permit && e.Suffix == "ge 32 le 32" {
					managed++
				}
			}
			if managed == 0 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("FRR prefix-list %q still has managed entries after %s (last output: %q)",
				name, timeout, strings.TrimSpace(lastRaw))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// AssertFRRPrefixListLineContains fails if no line in the prefix-list contains
// the given substring after timeout. Used by scenario 5 to verify a manually-
// seeded entry survives across the agent's reconcile cycles.
func AssertFRRPrefixListLineContains(t *testing.T, name, substring string, timeout time.Duration) {
	t.Helper()
	requireVtysh(t)
	deadline := time.Now().Add(timeout)
	var lastRaw string
	for {
		_, raw, err := fetchFRRPrefixListEntries(name)
		lastRaw = raw
		if err == nil && strings.Contains(raw, substring) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("FRR prefix-list %q does not contain %q after %s (last output: %q)",
				name, substring, timeout, strings.TrimSpace(lastRaw))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// SeedFRRPrefixListEntry adds a raw `ip prefix-list <name> seq <seq> <expr>`
// line via vtysh. expr is everything after the seq number — typically
// `permit 10.0.0.0/8` (no `ge`/`le` clauses, so the agent's reconciler
// ignores it). Fails the test if vtysh returns an error.
func SeedFRRPrefixListEntry(t *testing.T, name string, seq int, expr string) {
	t.Helper()
	requireVtysh(t)
	args := []string{
		"-c", "conf t",
		"-c", fmt.Sprintf("ip prefix-list %s seq %d %s", name, seq, expr),
		"-c", "end",
	}
	if out, err := exec.Command("vtysh", args...).CombinedOutput(); err != nil {
		t.Fatalf("vtysh seed prefix-list entry %q seq %d %q: %v (%s)",
			name, seq, expr, err, strings.TrimSpace(string(out)))
	}
}

// RemoveFRRPrefixList removes every entry in the named prefix-list, then drops
// the list itself via `no ip prefix-list <name>`. Best-effort and idempotent —
// safe to call from t.Cleanup even if the list never came into existence.
func RemoveFRRPrefixList(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath("vtysh"); err != nil {
		return
	}
	// `no ip prefix-list <name>` removes the entire list with all entries in
	// one shot. FRR is permissive: the command succeeds even if the list does
	// not exist. Best-effort; we only log on failure.
	args := []string{
		"-c", "conf t",
		"-c", "no ip prefix-list " + name,
		"-c", "end",
	}
	if out, err := exec.Command("vtysh", args...).CombinedOutput(); err != nil {
		t.Logf("RemoveFRRPrefixList %q: %v (%s)", name, err, strings.TrimSpace(string(out)))
	}
}
