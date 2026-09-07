.PHONY: up down restart migrate ssl proto

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
# names, so either would rewrite every generated file in the repo. Ruby uses
# protoc's built-in --ruby_out; no plugin needed.
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
	done
