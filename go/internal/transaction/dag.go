package transaction

import (
	"fmt"

	pb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
)

// decodeSpec reads the DAG (transfers + transfer_dependency) recorded on
// TransactionInitialized — this Transaction's first event whenever it
// exists at all, the same way a Transfer's Accepted event is always its
// first. events must be non-empty.
func decodeSpec(events []eventstore.Event) (map[string]*pb.Transfer, map[string]*pb.TransferIdList, error) {
	msg, err := events[0].Decode()
	if err != nil {
		return nil, nil, err
	}
	initialized, ok := msg.(*pb.TransactionInitialized)
	if !ok {
		return nil, nil, fmt.Errorf("transaction: stream starts with %T, want TransactionInitialized", msg)
	}
	return initialized.GetTransfers(), initialized.GetTransferDependency(), nil
}

// validateDAG rejects a malformed spec before TransactionInitialized is
// ever written: an empty transfer set, a dangling reference (either side —
// transfer_dependency naming a transfer_id not present in transfers), or a
// cycle. A direct self-dependency is just a 1-cycle and needs no special
// case. Uses Kahn's algorithm: build in-degree per node from deps, then
// repeatedly process zero-in-degree nodes, decrementing their children's
// in-degree as they're processed; if fewer nodes were processed than exist
// once the queue empties, a cycle exists among whatever's left.
func validateDAG(transfers map[string]*pb.Transfer, deps map[string]*pb.TransferIdList) error {
	if len(transfers) == 0 {
		return fmt.Errorf("transaction: transfers must not be empty")
	}
	for key, spec := range transfers {
		if spec.GetId() != key {
			return fmt.Errorf("transaction: transfers[%q].id = %q, want it to match its own map key", key, spec.GetId())
		}
	}

	inDegree := make(map[string]int, len(transfers))
	children := make(map[string][]string, len(transfers))
	for id := range transfers {
		inDegree[id] = 0
	}
	for childID, parents := range deps {
		if _, ok := transfers[childID]; !ok {
			return fmt.Errorf("transaction: transfer_dependency references unknown child %q", childID)
		}
		for _, parentID := range parents.GetTransferId() {
			if _, ok := transfers[parentID]; !ok {
				return fmt.Errorf("transaction: transfer %q depends on unknown transfer %q", childID, parentID)
			}
			inDegree[childID]++
			children[parentID] = append(children[parentID], childID)
		}
	}

	var queue []string
	for id, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, id)
		}
	}

	processed := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		processed++
		for _, child := range children[id] {
			inDegree[child]--
			if inDegree[child] == 0 {
				queue = append(queue, child)
			}
		}
	}

	if processed != len(transfers) {
		return fmt.Errorf("transaction: transfer_dependency contains a cycle (a direct self-dependency counts)")
	}
	return nil
}

// readyToRun returns every child id that (a) has not yet been touched (no
// entry in touched — nothing dispatched, gated, completed, or failed for it
// yet) and (b) has every parent listed in deps[id] already in completed. A
// child with no entry in deps is a DAG root, ready immediately. Called on
// every runSaga iteration rather than precomputing a single execution plan,
// which would be meaningless once children can settle over real days at
// different times.
func readyToRun(transfers map[string]*pb.Transfer, deps map[string]*pb.TransferIdList, touched, completed map[string]bool) []string {
	var ready []string
	for id := range transfers {
		if touched[id] {
			continue
		}
		parentsDone := true
		if parents, ok := deps[id]; ok {
			for _, parentID := range parents.GetTransferId() {
				if !completed[parentID] {
					parentsDone = false
					break
				}
			}
		}
		if parentsDone {
			ready = append(ready, id)
		}
	}
	return ready
}

// readyToRollback returns every started-or-gated child (touched, not yet
// rolled back) that has no remaining *active* dependent — every child that
// lists it as a parent is either untouched (never started, so trivially
// resolved) or already rolled back. This is readyToRun's mirror, walking
// edges backward: it's what makes reverse-topological rollback an ordinary
// incremental fold instead of a second static plan, and is what makes
// intra-transaction rollback safe with zero new locking — a downstream
// consumer's debit against an upstream producer's Token is always undone
// before the upstream producer's own reversal runs.
func readyToRollback(transfers map[string]*pb.Transfer, deps map[string]*pb.TransferIdList, touched, rolledBack, inFlight map[string]bool) []string {
	var ready []string
	for id := range transfers {
		if !touched[id] || rolledBack[id] {
			continue
		}
		if inFlight[id] {
			continue // its Reversal is already running; picking it again re-requests
		}
		blocked := false
		for childID, parents := range deps {
			if !touched[childID] || rolledBack[childID] {
				continue // never started, or already resolved — doesn't block
			}
			for _, parentID := range parents.GetTransferId() {
				if parentID == id {
					blocked = true
					break
				}
			}
			if blocked {
				break
			}
		}
		if !blocked {
			ready = append(ready, id)
		}
	}
	return ready
}
