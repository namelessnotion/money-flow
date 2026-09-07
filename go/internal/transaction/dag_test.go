package transaction

import (
	"testing"

	pb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
)

func deps(m map[string][]string) map[string]*pb.TransferIdList {
	out := make(map[string]*pb.TransferIdList, len(m))
	for id, parents := range m {
		out[id] = &pb.TransferIdList{TransferId: parents}
	}
	return out
}

func transfers(ids ...string) map[string]*pb.Transfer {
	out := make(map[string]*pb.Transfer, len(ids))
	for _, id := range ids {
		out[id] = &pb.Transfer{Id: id, AutoProcess: true}
	}
	return out
}

func TestValidateDAG_RejectsEmptyTransfers(t *testing.T) {
	t.Parallel()
	if err := validateDAG(map[string]*pb.Transfer{}, nil); err == nil {
		t.Fatal("validateDAG() error = nil, want an error for an empty transfers map")
	}
}

func TestValidateDAG_AcceptsAcyclicGraph(t *testing.T) {
	t.Parallel()
	// D depends on B and C; B and C each depend on A.
	xfers := transfers("A", "B", "C", "D")
	d := deps(map[string][]string{
		"B": {"A"},
		"C": {"A"},
		"D": {"B", "C"},
	})
	if err := validateDAG(xfers, d); err != nil {
		t.Errorf("validateDAG() error = %v, want nil for a valid DAG", err)
	}
}

func TestValidateDAG_RejectsSelfDependency(t *testing.T) {
	t.Parallel()
	xfers := transfers("A")
	d := deps(map[string][]string{"A": {"A"}})
	if err := validateDAG(xfers, d); err == nil {
		t.Fatal("validateDAG() error = nil, want an error for a self-dependency")
	}
}

func TestValidateDAG_RejectsIndirectCycle(t *testing.T) {
	t.Parallel()
	// A -> B -> C -> A
	xfers := transfers("A", "B", "C")
	d := deps(map[string][]string{
		"A": {"C"},
		"B": {"A"},
		"C": {"B"},
	})
	if err := validateDAG(xfers, d); err == nil {
		t.Fatal("validateDAG() error = nil, want an error for an indirect cycle")
	}
}

func TestValidateDAG_RejectsDanglingParentReference(t *testing.T) {
	t.Parallel()
	xfers := transfers("A", "B")
	d := deps(map[string][]string{"B": {"does-not-exist"}})
	if err := validateDAG(xfers, d); err == nil {
		t.Fatal("validateDAG() error = nil, want an error for a dangling parent reference")
	}
}

func TestValidateDAG_RejectsMismatchedTransferID(t *testing.T) {
	t.Parallel()
	xfers := map[string]*pb.Transfer{"A": {Id: "not-a", AutoProcess: true}}
	if err := validateDAG(xfers, nil); err == nil {
		t.Fatal("validateDAG() error = nil, want an error when a Transfer's own id doesn't match its map key")
	}
}

func TestValidateDAG_RejectsDanglingChildReference(t *testing.T) {
	t.Parallel()
	xfers := transfers("A")
	d := deps(map[string][]string{"does-not-exist": {"A"}})
	if err := validateDAG(xfers, d); err == nil {
		t.Fatal("validateDAG() error = nil, want an error for a dependency entry naming an unknown child")
	}
}

func TestReadyToRun_RootsAreReadyImmediately(t *testing.T) {
	t.Parallel()
	xfers := transfers("A", "B")
	d := deps(map[string][]string{"B": {"A"}})

	ready := readyToRun(xfers, d, map[string]bool{}, map[string]bool{})
	if len(ready) != 1 || ready[0] != "A" {
		t.Fatalf("readyToRun() = %v, want just [A] (B depends on A, not yet complete)", ready)
	}
}

func TestReadyToRun_TouchedChildIsNeverReadyAgain(t *testing.T) {
	t.Parallel()
	xfers := transfers("A")
	ready := readyToRun(xfers, deps(nil), map[string]bool{"A": true}, map[string]bool{})
	if len(ready) != 0 {
		t.Fatalf("readyToRun() = %v, want empty (A already touched)", ready)
	}
}

func TestReadyToRun_ANDJoinOnlyReadyOnceBothParentsComplete(t *testing.T) {
	t.Parallel()
	// D depends on both A and B (an AND-join).
	xfers := transfers("A", "B", "D")
	d := deps(map[string][]string{"D": {"A", "B"}})

	touched := map[string]bool{"A": true, "B": true}

	// Only A complete: D not ready yet.
	ready := readyToRun(xfers, d, touched, map[string]bool{"A": true})
	if len(ready) != 0 {
		t.Fatalf("readyToRun() = %v, want empty (only one of two parents complete)", ready)
	}

	// Both complete: D ready.
	ready = readyToRun(xfers, d, touched, map[string]bool{"A": true, "B": true})
	if len(ready) != 1 || ready[0] != "D" {
		t.Fatalf("readyToRun() = %v, want just [D] (both parents now complete)", ready)
	}
}

func TestReadyToRollback_LeafWithNoDependentsIsReadyFirst(t *testing.T) {
	t.Parallel()
	// B depends on A. Both touched, neither rolled back yet.
	xfers := transfers("A", "B")
	d := deps(map[string][]string{"B": {"A"}})
	touched := map[string]bool{"A": true, "B": true}

	ready := readyToRollback(xfers, d, touched, map[string]bool{}, nil)
	if len(ready) != 1 || ready[0] != "B" {
		t.Fatalf("readyToRollback() = %v, want just [B] (A is blocked by its still-active dependent B)", ready)
	}
}

func TestReadyToRollback_ParentReadyOnceDependentRolledBack(t *testing.T) {
	t.Parallel()
	xfers := transfers("A", "B")
	d := deps(map[string][]string{"B": {"A"}})
	touched := map[string]bool{"A": true, "B": true}

	ready := readyToRollback(xfers, d, touched, map[string]bool{"B": true}, nil)
	if len(ready) != 1 || ready[0] != "A" {
		t.Fatalf("readyToRollback() = %v, want just [A] (B already rolled back)", ready)
	}
}

func TestReadyToRollback_UntouchedChildNeverBlocksItsParent(t *testing.T) {
	t.Parallel()
	// B depends on A but was never touched (e.g. abandoned before ever
	// being requested) — it must not block A's rollback.
	xfers := transfers("A", "B")
	d := deps(map[string][]string{"B": {"A"}})
	touched := map[string]bool{"A": true}

	ready := readyToRollback(xfers, d, touched, map[string]bool{}, nil)
	if len(ready) != 1 || ready[0] != "A" {
		t.Fatalf("readyToRollback() = %v, want just [A] (untouched B never blocks)", ready)
	}
}

func TestReadyToRollback_AlreadyRolledBackChildIsNeverReadyAgain(t *testing.T) {
	t.Parallel()
	xfers := transfers("A")
	ready := readyToRollback(xfers, deps(nil), map[string]bool{"A": true}, map[string]bool{"A": true}, nil)
	if len(ready) != 0 {
		t.Fatalf("readyToRollback() = %v, want empty (A already rolled back)", ready)
	}
}
