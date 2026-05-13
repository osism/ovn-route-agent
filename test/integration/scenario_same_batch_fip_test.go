//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// TestScenario_FailureInjection_SameBatchFIPAddRemove (#88 item 5):
//
// Issue a single OVSDB transaction that simultaneously adds one FIP and
// removes another. The libovsdb monitor delivers both row changes in the
// same update; the agent's debounced reconcile loop must observe both ends
// of the change.
//
// TestImmediateStateRefreshCoalesces (in ovn_test.go) covers the unit-level
// debounce. This scenario is the end-to-end variant: kernel routes for both
// FIPs must converge to "A present, B absent" after the next reconcile.
func TestScenario_FailureInjection_SameBatchFIPAddRemove(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "fipbatch",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.FastDefaults()
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const (
		fipA = "198.51.100.42"
		fipB = "198.51.100.43"
	)

	// Pre-install FIP B and wait for it to land — that is the "old" state
	// the same-batch transaction is going to undo.
	natBUUID := testenv.AddFIP(t, ctx, nb, router, fipB, "10.0.0.43")
	testenv.AssertKernelRoute(t, fipB, 15*time.Second)

	// Build a single transaction that:
	//   1. Inserts the new FIP A row.
	//   2. Mutates the router to append A.
	//   3. Mutates the router to delete B (parent → child reference).
	//   4. Deletes the FIP B row itself.
	//
	// Steps 2-3 are separate mutate operations on the same Logical_Router;
	// OVSDB processes them atomically in one transaction, so the monitor
	// emits a single update with both the new and the removed NAT.
	natA := &testenv.NBNAT{
		UUID:       "nat_a",
		Type:       "dnat_and_snat",
		ExternalIP: fipA,
		LogicalIP:  "10.0.0.42",
	}
	createOps, err := nb.Create(natA)
	if err != nil {
		t.Fatalf("build create op for FIP A: %v", err)
	}

	addOp := ovsdb.Operation{
		Op:    "mutate",
		Table: "Logical_Router",
		Where: []ovsdb.Condition{{
			Column:   "_uuid",
			Function: ovsdb.ConditionEqual,
			Value:    ovsdb.UUID{GoUUID: router.RouterUUID},
		}},
		Mutations: []ovsdb.Mutation{{
			Column:  "nat",
			Mutator: ovsdb.MutateOperationInsert,
			Value:   ovsdb.OvsSet{GoSet: []any{ovsdb.UUID{GoUUID: "nat_a"}}},
		}},
	}
	delOp := ovsdb.Operation{
		Op:    "mutate",
		Table: "Logical_Router",
		Where: []ovsdb.Condition{{
			Column:   "_uuid",
			Function: ovsdb.ConditionEqual,
			Value:    ovsdb.UUID{GoUUID: router.RouterUUID},
		}},
		Mutations: []ovsdb.Mutation{{
			Column:  "nat",
			Mutator: ovsdb.MutateOperationDelete,
			Value:   ovsdb.OvsSet{GoSet: []any{ovsdb.UUID{GoUUID: natBUUID}}},
		}},
	}
	deleteRowOps, err := nb.Where(&testenv.NBNAT{UUID: natBUUID}).Delete()
	if err != nil {
		t.Fatalf("build delete op for FIP B: %v", err)
	}

	ops := append([]ovsdb.Operation{}, createOps...)
	ops = append(ops, addOp, delOp)
	ops = append(ops, deleteRowOps...)
	testenv.Transact(t, ctx, nb, ops)

	// Convergence: A's route is installed, B's route is withdrawn. Both
	// assertions poll independently — the ordering between the add and
	// the remove inside the agent's reconcile is not under test, only
	// the post-convergence steady state.
	testenv.AssertKernelRoute(t, fipA, 15*time.Second)
	testenv.AssertNoKernelRoute(t, fipB, 15*time.Second)

	// FRR plane mirrors the kernel plane — pin it down too so a future
	// regression that drops the OVSDB-batch update only on one of the
	// two planes cannot pass silently.
	testenv.AssertFRRRoute(t, fipA, 15*time.Second)
	testenv.AssertNoFRRRoute(t, fipB, 15*time.Second)
}
