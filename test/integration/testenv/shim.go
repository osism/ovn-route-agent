//go:build integration

package testenv

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// FailingToolShim is the handle returned by WithFailingTool. The test holds it
// to flip the shim between pass-through and fail-N-times mode at the right
// moment in the scenario.
//
// The shim is disarmed at construction time: the agent's startup invocations
// of the shimmed tool (e.g. SetupPortForward calling nft) succeed normally,
// so the failure window can be scoped to a single point in the test.
type FailingToolShim struct {
	tool        string
	dir         string
	armPath     string
	counterPath string
	matchPath   string
	invokePath  string
	failCount   int
	t           *testing.T
}

// WithFailingTool installs a shim for tool (e.g. "vtysh", "nft", "ovs-ofctl")
// at the front of the agent's PATH. The shim starts disarmed (every call
// chains through to the real binary). After Arm() the next failCount
// invocations exit non-zero; subsequent invocations chain through again.
//
// Usage:
//
//	shim := testenv.WithFailingTool(t, "vtysh", 3)
//	cfg := testenv.FastDefaults()
//	cfg.ExtraEnv = append(cfg.ExtraEnv, shim.Env())
//	shim.Arm()
//	a := testenv.RunAgent(t, cfg)
//
// The helper is the issue #88 acceptance criterion #2 entry point: it lives
// in testenv/ so all three sub-scenarios of failure injection (vtysh, nft,
// ovs-ofctl) share one implementation.
func WithFailingTool(t *testing.T, tool string, failCount int) *FailingToolShim {
	t.Helper()
	if tool == "" {
		t.Fatal("WithFailingTool: tool name required")
	}
	if failCount < 0 {
		t.Fatalf("WithFailingTool: failCount must be >= 0, got %d", failCount)
	}

	realPath, err := exec.LookPath(tool)
	if err != nil {
		t.Skipf("WithFailingTool: real %s not found in PATH: %v", tool, err)
	}

	dir := t.TempDir()
	shim := &FailingToolShim{
		tool:        tool,
		dir:         dir,
		armPath:     filepath.Join(dir, tool+".armed"),
		counterPath: filepath.Join(dir, tool+".remaining"),
		matchPath:   filepath.Join(dir, tool+".match"),
		invokePath:  filepath.Join(dir, tool+".invocations"),
		failCount:   failCount,
		t:           t,
	}

	// The shim is a small POSIX shell script. Each invocation:
	//   1. If the arm file is absent, chain through to the real tool.
	//   2. If a match file is present, only fail when the joined args
	//      contain that substring; pass through otherwise.
	//   3. Otherwise read the counter; if positive, decrement and exit 1.
	//   4. If counter has hit zero, chain through.
	//
	// The shim strips its own directory from PATH before exec so the inner
	// real-binary lookup cannot recurse into the shim — a recursion bug
	// here would loop until kernel TASK_MAX with no useful diagnostic.
	script := fmt.Sprintf(`#!/bin/sh
set -e
ARM=%q
COUNTER=%q
MATCH=%q
REAL=%q
SHIM_DIR=%q
INVOKE=%q
# Append one line per invocation so the test can verify the shim was on
# the agent's PATH even when no failure was injected. Best-effort; loss
# of this line never breaks the pass-through path.
printf '%%s\n' "invoked: $*" >>"$INVOKE" 2>/dev/null || true
strip_path() {
    # Strip the shim directory from PATH so the real binary lookup
    # resolves to the system install, not back to us.
    printf '%%s' "$PATH" | awk -v RS=: -v ORS=: -v skip="$SHIM_DIR" '$0!=skip{print}' | sed 's/:$//'
}
if [ ! -e "$ARM" ]; then
    PATH="$(strip_path)" exec "$REAL" "$@"
fi
if [ -s "$MATCH" ]; then
    pattern=$(cat "$MATCH")
    joined="$*"
    case "$joined" in
        *"$pattern"*) ;;
        *)
            PATH="$(strip_path)" exec "$REAL" "$@"
            ;;
    esac
fi
remaining=$(cat "$COUNTER" 2>/dev/null || echo 0)
case "$remaining" in
    ''|*[!0-9]*) remaining=0 ;;
esac
if [ "$remaining" -gt 0 ]; then
    new=$((remaining - 1))
    printf '%%s\n' "$new" >"$COUNTER"
    echo "ovn-network-agent test shim: forced failure of %s (remaining=$new)" >&2
    exit 1
fi
PATH="$(strip_path)" exec "$REAL" "$@"
`, shim.armPath, shim.counterPath, shim.matchPath, realPath, dir, shim.invokePath, tool)
	if err := os.WriteFile(filepath.Join(dir, tool), []byte(script), 0o755); err != nil {
		t.Fatalf("WithFailingTool: write shim: %v", err)
	}

	return shim
}

// Env returns the AgentConfig.ExtraEnv entry that prepends the shim
// directory to PATH. Pass this into cfg.ExtraEnv before starting the agent.
func (s *FailingToolShim) Env() string {
	current := os.Getenv("PATH")
	if current == "" {
		return "PATH=" + s.dir
	}
	return "PATH=" + s.dir + string(os.PathListSeparator) + current
}

// Arm enables the failure counter. The next failCount invocations of the
// shimmed tool will exit non-zero with a "test shim: forced failure" line
// on stderr.
//
// Idempotent: calling Arm() on an already-armed shim resets the counter so
// the test can re-inject failures for a follow-up reconcile.
func (s *FailingToolShim) Arm() {
	s.t.Helper()
	if err := os.WriteFile(s.counterPath, []byte(fmt.Sprintf("%d\n", s.failCount)), 0o600); err != nil {
		s.t.Fatalf("FailingToolShim.Arm: write counter: %v", err)
	}
	if err := os.WriteFile(s.armPath, []byte("1\n"), 0o600); err != nil {
		s.t.Fatalf("FailingToolShim.Arm: write arm file: %v", err)
	}
}

// MatchArg narrows the shim's failure window to invocations whose joined
// argv contains substring. The shim chains through unconditionally for
// every call that does not match — useful when a scenario wants to fail
// only one *kind* of invocation of a tool (e.g. vtysh's add-route call but
// not its show-routes call).
//
// Returns the receiver to support fluent chaining at the call site.
func (s *FailingToolShim) MatchArg(substring string) *FailingToolShim {
	s.t.Helper()
	if err := os.WriteFile(s.matchPath, []byte(substring), 0o600); err != nil {
		s.t.Fatalf("FailingToolShim.MatchArg: %v", err)
	}
	return s
}

// Disarm removes the arm file so subsequent invocations chain through
// unconditionally. Tests do not normally need to call this — the shim
// disarms itself once the counter reaches zero — but it is useful when a
// scenario wants to guarantee no further failures before exit.
func (s *FailingToolShim) Disarm() {
	s.t.Helper()
	if err := os.Remove(s.armPath); err != nil && !os.IsNotExist(err) {
		s.t.Fatalf("FailingToolShim.Disarm: %v", err)
	}
}

// InvocationLog returns the contents of the shim's invocation log — one line
// per call ("invoked: <argv>") the agent made through the shim. Tests use
// this as a diagnostic when a forced-failure assertion times out: a non-empty
// log proves the shim is on PATH and being reached, narrowing the failure
// down to the arm/counter logic rather than a missing PATH override.
func (s *FailingToolShim) InvocationLog() string {
	s.t.Helper()
	data, err := os.ReadFile(s.invokePath)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		s.t.Logf("FailingToolShim.InvocationLog: read %s: %v", s.invokePath, err)
		return ""
	}
	return string(data)
}

// Remaining returns the number of failures left in the current arm window.
// Useful for diagnostics — tests can log this to see whether the agent
// consumed all injected failures before recovery.
func (s *FailingToolShim) Remaining() int {
	s.t.Helper()
	data, err := os.ReadFile(s.counterPath)
	if err != nil {
		return 0
	}
	n := 0
	for _, c := range strings.TrimSpace(string(data)) {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
