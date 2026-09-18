# ACH Transactions end to end

An ACH deposit or withdrawal is originated from GraphQL, run by Go as a two-leg Transaction, and observed back
in Ruby through the read-model consumer. The vocabulary is in [`ruby/CONTEXT.md`](../ruby/CONTEXT.md). The
consumer's decisions are in [`ruby/docs/adr/0001`](../ruby/docs/adr/0001-read-model-consumer-and-follow-on-write-back.md)
and [`0002`](../ruby/docs/adr/0002-projection-consumer-topology-and-halt-policy.md).

```
GraphQL ──► Services::Ach::Initiate ──► Go: StartInitializingTransaction   (real leg stops, staged)
                    │                    Go: ConfirmStagedTransfer         (after provider submission)
                    ▼
            ach_transactions (intent, ids)

settleAch ──► Go: PostPendingTransfer, ResumeTransaction   (shadow leg runs; completed)
returnAch ──► Go: CancelStagedTransfer, ResumeTransaction  (rolled back)

Go events ──► money_flow_dev.events ──► Debezium ──► transfer-events / transaction-events
          ──► ruby/bin/consumer ──► transaction_projections / transfer_projections ──► GraphQL state
```

There is no real ACH provider yet. `Services::Ach::FakeProvider` accepts every submission, and the
`settleAch` / `returnAch` mutations stand in for the provider's settlement and return notices.

## Running it

The consumer only sees events once `money_flow_dev` is published, which is what `make cdc-up` does: it
registers the Debezium connector (`docker/cdc/events-connector.json`). Publication starts from the current end
of the log, so earlier events are never published. The Go orchestrator reads the same topics and can run
alongside (`make orchestrator-up`); it resumes Transactions on its own, which `settleAch` / `returnAch` also
do, idempotently.

```bash
docker compose up -d
make cdc-up
make consumer-up
make consumer-logs
```

Drive it over GraphQL at `http://localhost:9292/graphql`:

```graphql
mutation { onboardEntity(name: "Acme") { entity { id } } }
mutation { initiateAchDeposit(entityId: "1", amountMinorUnits: "12500") { achTransaction { id } } }
query    { achTransactions(entityId: "1") { nodes { id direction state reason realLegState } } }
mutation { settleAch(achTransactionId: "…") { achTransaction { id } } }
mutation { returnAch(achTransactionId: "…", reason: "R01 insufficient funds") { achTransaction { id } } }
```

A freshly initiated deposit shows `state: null` until the consumer catches up. After that it shows `STARTED`
with the real leg `PENDING`. `settleAch` takes it to `COMPLETED`; `returnAch` takes it to `ROLLED_BACK`.

Tear down in this order, before `docker compose down`. The replication slot outlives the containers and pins
WAL if left behind.

```bash
make consumer-down
make cdc-down
```

## Known gap: withdrawals need cleared cash

A withdrawal's shadow leg moves `cleared_cash → bank_control`, but a deposit leaves its shadow leg's money
in `uncleared_cash`. Nothing moves uncleared cash to cleared yet. So on an entity funded only by deposits, a
settled withdrawal's shadow leg fails for lack of balance. Go then rolls the Transaction back, reversing the
committed real leg, and the Transaction waits in `ROLLBACK_STARTED` on that staged Reversal (see
[`go/docs/adr/0002`](../go/docs/adr/0002-asynchronous-rollback-and-reversal-reconciliation.md)). This was
observed end to end. It needs a clearing step, which is not part of this change.
