.PHONY: up down restart migrate ssl proto cdc-up cdc-down orchestrator-up orchestrator-down orchestrator-logs \
	consumer-up consumer-down consumer-logs events resume resume-open jobs-up jobs-down jobs-logs clear-ach-now submit-ach-now \
	disburse-now simulate simulate-lending tla-check tla-trace

up:
	docker compose up -d

down:
	docker compose down

restart:
	docker compose restart

# Runs both migrators against the postgres container out-of-band, e.g. after
# adding a new migration file without wanting to restart ruby/go (which also
# migrate automatically on boot, see docker/ruby/entrypoint.sh and
# docker/go/entrypoint.sh).
migrate:
	docker compose exec go go run ./cmd/migrate up
	docker compose exec ruby bin/migrate up

# Generates a browser-trusted cert for *.local.namelessnotion.com (see
# bin/setup_local_ssl.sh) and restarts the proxy container to pick it up.
ssl:
	bin/setup_local_ssl.sh
	-docker compose restart proxy

# Regenerates the Go and Ruby bindings for the Published Language in proto/
# (see docs/agents/domain.md). Run after editing anything under proto/ and
# commit the regenerated output alongside the .proto change.
#
# Deliberately protoc rather than `buf generate`: buf reports the protoc
# version as "(unknown)" and its remote Ruby plugin emits descriptors with JSON
# names, so either would rewrite every generated file in the repo.
#
# Ruby needs BOTH outputs. --ruby_out is protoc's own and emits the messages;
# the Twirp service and client stubs come from protoc-gen-twirp_ruby, which is
# a separate binary and was missing from this target — so ruby/gen's *_twirp.rb
# quietly kept whatever it last held while everything else regenerated. Install
# it with:
#
#	go install github.com/arthurnn/twirp-ruby/protoc-gen-twirp_ruby@v1.14.1
#
# That second pass is guarded on the file declaring a service, because the
# plugin emits an empty module for one that does not — shared/v1's money and
# allows would each gain a stub containing nothing.
#
# One protoc invocation PER FILE, not one for all of them: protoc-gen-twirp
# emits its shared boilerplate (TwirpServer and friends) once per invocation,
# so batching every proto into a single call strips those definitions from all
# but one generated file and the rest stop compiling.
#
# CAVEAT: .twirp.go embeds a gzipped FileDescriptorProto whose bytes vary with
# the protoc-gen-twirp / Go build doing the compressing, so a full run can
# rewrite .twirp.go files whose .proto did not change, with no semantic
# difference. Until the toolchain is pinned, check `git status` afterwards and
# revert anything you did not mean to touch.
proto:
	@for f in $$(find proto -name '*.proto'); do \
	  echo "protoc $$f"; \
	  protoc -I proto \
	    --go_out=go/gen/proto --go_opt=paths=source_relative \
	    --twirp_out=go/gen/proto --twirp_opt=paths=source_relative \
	    --ruby_out=ruby/gen/proto \
	    $$f || exit 1; \
	  if grep -q '^service ' $$f; then \
	    protoc -I proto --twirp_ruby_out=ruby/gen/proto $$f || exit 1; \
	  fi; \
	done

# Publish money_flow_dev to Kafka (docs/adr/0001): cdc-up registers the
# Debezium connector; cdc-down removes it, its publication and the
# replication slot that `docker compose down` leaves behind. Run cdc-down
# BEFORE bringing the stack down — it needs Connect and Postgres up.
cdc-up:
	docker/cdc/setup.sh

cdc-down:
	docker/cdc/teardown.sh

# Event-triggered saga orchestrator (docs/saga-orchestrator.md). It consumes
# the topics the CDC connector publishes, so run `make cdc-up` first; without
# the connector there is nothing on them and it simply idles.
orchestrator-up:
	docker compose up -d orchestrator

orchestrator-down:
	docker compose stop orchestrator

orchestrator-logs:
	docker compose logs -f orchestrator

# Prints Go's event log as it is written (go/cmd/events), e.g. to watch an ACH
# Transaction's saga. Reads Postgres directly, so it needs neither CDC nor
# Kafka. Runs on the host: it does not link TigerBeetle. Pass flags via ARGS:
#   make events ARGS="-types transaction,transfer"
#   make events ARGS="-last 50"
events:
	cd go && go run ./cmd/events $(ARGS)

# Drives every saga still in flight by hand (go/cmd/resume). Since the async
# cutover nothing in the RPC surface advances a saga, so an aggregate whose
# trigger was never published — anything written before `make cdc-up`, or while
# the connector was down — has no wake-up coming. This is how one is given.
# Run it once after deploying the cutover, and whenever the orchestrator has
# halted and been repaired. Needs TigerBeetle, so it runs in the go container.
resume-open:
	docker compose exec go go run ./cmd/resume -open

# Drives named aggregates instead of everything in flight:
#   make resume ARGS="01a0... 01a1..."
resume:
	docker compose exec go go run ./cmd/resume $(ARGS)

# Ruby read-model consumer (ruby/bin/consumer). Reads what cdc-up publishes
# into the projection tables of money_flow_dev.
consumer-up:
	docker compose up -d ruby-consumer

consumer-down:
	docker compose stop ruby-consumer

consumer-logs:
	docker compose logs -f ruby-consumer

# Resque worker + scheduler (ruby/config/resque_schedule.yml) and their Redis.
# The scheduler enqueues the recurring sweeps; the worker runs them. The *-now
# targets below enqueue one sweep immediately.
jobs-up:
	docker compose up -d redis resque-worker resque-scheduler

jobs-down:
	docker compose stop resque-scheduler resque-worker redis

jobs-logs:
	docker compose logs -f resque-scheduler resque-worker

# Runs the ACH submission sweep immediately rather than waiting for the next
# minute. Since the async cutover this is what hands entries to the provider;
# Initiate only accepts the Transaction (ruby/docs/adr/0008).
submit-ach-now:
	docker compose exec resque-worker bundle exec ruby -e \
	  'require "./lib/resque_boot"; ResqueBoot.load!; Resque.enqueue(Jobs::SubmitAchEntries); puts "enqueued"'

clear-ach-now:
	docker compose exec resque-worker bundle exec ruby -e \
	  'require "./lib/resque_boot"; ResqueBoot.load!; Resque.enqueue(Jobs::ClearAchDeposits); puts "enqueued"'

# Runs the repayment disbursement sweep immediately rather than waiting up to
# five minutes. Pays every completed Repayment not yet disbursed; to disburse
# one Repayment, use the disburseRepaymentNow mutation (ruby/docs/adr/0007).
disburse-now:
	docker compose exec resque-worker bundle exec ruby -e \
	  'require "./lib/resque_boot"; ResqueBoot.load!; Resque.enqueue(Jobs::DisburseRepayments); puts "enqueued"'

# Benchmarks the Go backend under load and then checks the ledger balances
# (go/cmd/simulate): needs the orchestrator and CDC up, since nothing settles
# without them. Runs in the go container, as it links TigerBeetle's native
# client. Pass flags through ARGS, e.g.
#   make simulate ARGS="-entities 150 -transactions 2000 -concurrency 32 -seed 42"
simulate:
	docker compose exec -T go go run ./cmd/simulate $(ARGS)

# Plays out a Groundfloor-like lending market through the real stack
# (ruby/lib/lending_simulation.rb): needs Go, the orchestrator, CDC and the
# consumer up. Pass options through ARGS, e.g.
#   make simulate-lending ARGS="--seed 7 --investors 20"
# then open /money-flow?run=<tag it prints> in the client.
simulate-lending:
	docker compose exec -T ruby bin/simulate_lending $(ARGS)

# TLC, vendored so every run checks the specs with the same model checker
# (see spec/tools/README.md). The tla-* targets need Java on the host.
TLA2TOOLS := $(CURDIR)/spec/tools/tla2tools.jar
TLC := java -XX:+UseParallelGC -cp $(TLA2TOOLS) tlc2.TLC -nowarning -noGenerateSpecTE -workers auto

# Model-checks the event log's contract (spec/eventlog.tla) and that the
# handlers' design refines it (spec/eventstore.tla).
tla-check:
	cd spec && $(TLC) -metadir "$$(mktemp -d)" -config MCeventlog.cfg MCeventlog.tla
	cd spec && $(TLC) -metadir "$$(mktemp -d)" -config MCeventstore.cfg MCeventstore.tla

# Records traces of concurrent RequestTransfer/RequestReversal calls against
# the real Postgres store (go/internal/transfer/tlatrace_test.go) and checks
# each is a behavior of spec/eventstore.tla, along with the fixtures that test
# the trace spec itself. The harness runs in the go container, as it links
# TigerBeetle's native client. Replay a workload with TLA_TRACE_SEED=<seed it
# printed>.
tla-trace:
	rm -rf go/.tla-traces && mkdir -p go/.tla-traces
	docker compose exec -T -e TLA_TRACE_DIR=/app/.tla-traces -e TLA_TRACE_SEED=$(TLA_TRACE_SEED) \
		go go test ./internal/transfer -run '^TestTLATrace$$' -count=1 -v
	TLA2TOOLS=$(TLA2TOOLS) spec/trace/validate.sh spec/trace/fixtures go/.tla-traces
