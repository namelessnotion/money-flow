package transaction

import (
	"context"
	"errors"
	"fmt"

	"github.com/twitchtv/twirp"

	pb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/money"
)

// maxTransfersPerTransaction caps how wide one Transaction may be. It is a
// circuit breaker, not a design constraint: every shape this system builds is
// one or two legs, so a request anywhere near this limit is a caller that has
// lost track of what it is assembling. Two independent reasons, either
// sufficient on its own:
//
//   - Rollback blast radius. The Transaction is the unit of rollback, so an
//     N-child Transaction failing is N reversals, each a whole Transfer running
//     its own saga (ADR 0002). TransactionRollbackFailed is a terminal that
//     requires a person, and bounding N bounds how much that person has to
//     reconcile by hand.
//   - TigerBeetle's batch ceiling is unguarded below this point.
//     ledger.CreateTransfers passes the caller's whole slice straight through
//     with no chunking, and a linked chain cannot span batches. lookupBatchMax
//     exists, but only for reads.
//
// It does NOT make wide DAGs cheap — maxDispatchPerStep in saga.go is what
// bounds the work of any one saga run. This bounds what may exist at all.
const maxTransfersPerTransaction = 64

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
// ever written: an empty transfer set, more transfers than one Transaction
// may hold, a leg whose amount no Transfer would accept, a dangling
// reference (either side — transfer_dependency naming a transfer_id not
// present in transfers), or a cycle. A direct self-dependency is just a
// 1-cycle and needs no special case. Uses Kahn's algorithm: build in-degree
// per node from deps, then
// repeatedly process zero-in-degree nodes, decrementing their children's
// in-degree as they're processed; if fewer nodes were processed than exist
// once the queue empties, a cycle exists among whatever's left.
func validateDAG(transfers map[string]*pb.Transfer, deps map[string]*pb.TransferIdList) error {
	if len(transfers) == 0 {
		return fmt.Errorf("transaction: transfers must not be empty")
	}
	// Checked before anything that walks the graph, so a pathological request
	// is refused at its cheapest, and before wouldAcceptReadyChildren so an
	// over-wide DAG never costs one balance read per child.
	if len(transfers) > maxTransfersPerTransaction {
		return fmt.Errorf("transaction: %d transfers exceeds the limit of %d per Transaction",
			len(transfers), maxTransfersPerTransaction)
	}
	for key, spec := range transfers {
		if spec.GetId() != key {
			return fmt.Errorf("transaction: transfers[%q].id = %q, want it to match its own map key", key, spec.GetId())
		}
		if err := validateChildAmount(key, spec); err != nil {
			return err
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

// validateChildAmount refuses a leg whose amount transfer.RequestTransfer
// would refuse anyway — a nil Money, a missing currency, or zero minor units.
//
// Catching it here is not tidiness. Refused at dispatch, that refusal is a
// twirp error rather than a domain rejection: requestChildTransfer returns
// it, runSaga returns it, and the caller never sees it — leaving the
// Transaction in Started with no event on its stream to explain why it will
// never move again. Refused here it is one TransactionRejected, written
// before anything else exists, which the read model shows like any other
// rejection. ruby/docs/adr/0006 asks for exactly this.
//
// money.Validate stays the one owner of what a well-formed amount is (see its
// package doc); only its message is unwrapped, so the recorded reason reads
// as domain prose rather than a transport error string.
func validateChildAmount(key string, spec *pb.Transfer) error {
	err := money.Validate("amount", spec.GetAmount())
	if err == nil {
		return nil
	}
	var twerr twirp.Error
	if errors.As(err, &twerr) {
		return fmt.Errorf("transaction: transfers[%q].amount %s", key, twerr.Msg())
	}
	return fmt.Errorf("transaction: transfers[%q].amount is invalid: %w", key, err)
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

// wouldAcceptReadyChildren pre-flight-checks every child that would be
// dispatched immediately were req accepted (auto_process=true, ready at time
// zero per readyToRun, mint_source=false) against transfer's own accept-time
// decision, before TransactionInitialized is ever written. mint_source
// children are excluded: they have no balance constraint at all (they mint
// their own source Token — see transfer.validateMintSource), and checking
// one here would spuriously reject via TransactionExistsChecker, since this
// Transaction doesn't exist yet — a real circular dependency, not just a
// convenient exclusion. Returns the reason for the first ready child that
// would be rejected right now, or "" if every ready child would be
// accepted. Fast, best-effort: the real, unchanged dispatch inside runSaga
// still runs afterward and remains the sole authority — see go/docs/adr/0004.
func (s *Server) wouldAcceptReadyChildren(
	ctx context.Context, transactionID string, transfers map[string]*pb.Transfer, deps map[string]*pb.TransferIdList,
) (string, error) {
	for _, childID := range readyToRun(transfers, deps, map[string]bool{}, map[string]bool{}) {
		spec := transfers[childID]
		if !spec.GetAutoProcess() || spec.GetMintSource() {
			continue
		}
		rejection, err := s.transfer.WouldAcceptTransfer(ctx, spec.GetFromWalletId(), spec.GetAmount(), transactionID)
		if err != nil {
			return "", err
		}
		if rejection != nil {
			return fmt.Sprintf("transfer %q: %s", childID, rejection.GetReason()), nil
		}
	}
	return "", nil
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
