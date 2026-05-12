//go:build integration

package testenv

import (
	"testing"
	"time"
)

// Eventually polls condition every tick until it returns true or the timeout
// expires. On timeout the test is failed with msg. It is the integration
// harness's standard polling primitive — prefer it to ad-hoc time.After loops.
func Eventually(t *testing.T, condition func() bool, timeout, tick time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Eventually timed out after %s: %s", timeout, msg)
		}
		time.Sleep(tick)
	}
}

// EventuallyValue polls fn every tick until it returns (value, true) or the
// timeout expires. The captured value is returned to the caller. Useful when
// the test needs to record *when* a condition first held (e.g. drain ordering).
func EventuallyValue[T any](t *testing.T, fn func() (T, bool), timeout, tick time.Duration, msg string) T {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var zero T
	for {
		v, ok := fn()
		if ok {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("EventuallyValue timed out after %s: %s", timeout, msg)
			return zero
		}
		time.Sleep(tick)
	}
}

// AssertEventually is Eventually with an attached lazy dump. Same polling
// shape; on timeout, the supplied dump callback is invoked once and its
// return value is appended to the failure message. dump is *only* called on
// the timeout branch, so a passing AssertEventually pays nothing beyond the
// extra closure parameter.
//
// Use it where a generic "did not happen" timeout would force the operator
// to ssh in and grep — e.g. polling for an NB row count where the dump can
// render the current per-key counts, or polling for a kernel/FRR state where
// the dump can attach the relevant `ip route show` / `vtysh show` output. A
// nil dump degrades to a plain Eventually-style failure.
func AssertEventually(t *testing.T, condition func() bool, timeout, tick time.Duration, msg string, dump func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			if dump != nil {
				t.Fatalf("AssertEventually timed out after %s: %s\nstate dump:\n%s", timeout, msg, dump())
			}
			t.Fatalf("AssertEventually timed out after %s: %s", timeout, msg)
		}
		time.Sleep(tick)
	}
}
