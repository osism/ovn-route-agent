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
