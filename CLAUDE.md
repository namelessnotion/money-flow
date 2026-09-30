@AGENTS.MD

# Money Flow Project Guidelines

## Project Structure

MoneyFlow is `go/` plus its published contract in `proto/`. `ruby/` and `client/` are **not part of MoneyFlow**. They are an example application showing how a business builds on it, and they stay in the repo as the reference integration. Don't put MoneyFlow behavior in the example. If the example needs something MoneyFlow can't do, change `go/`/`proto/` so any integrator gets it.

- `go/` - "MoneyFlow" - The tokenized transaction system, written in Go. An event sourced architecture is used to handle the intent of money flow, backed by TigerBeetle for the low-level accounting at the Token level. The event log is append-only and immutable, stored in a single PostgreSQL table. Its only interfaces are the Twirp services and the events it publishes (via CDC to Kafka).

- `proto/` - MoneyFlow's published language: the Protobuf domain command and event messages and the Twirp services. It is the contract an integrator builds against. It generates Go code, and Ruby code for the example.

- `ruby/` - "example business backend" - An example of building on MoneyFlow, written in Ruby: a marketplace with ACH and a securities lending market, exposed via GraphQL. It starts work only through MoneyFlow's Twirp services and learns outcomes only from its published events, the way any integrator would. Sequel is used as the ORM against PostgreSQL.

- `client/` - "example front end" - The Vue front end for the Ruby example, talking to it via GraphQL. VueJS 3 with Apollo Client 4 and its composables for reactive queries, mutations and state; TailwindCSS for styling.

## Coding

Write tests first, then write code to pass the tests.
After writing code, run linters on your code to ensure it adheres to style guidelines.
Never disable a linter/cop rule to silence a violation — no inline disable comments (e.g. `# rubocop:disable`) and no adding exclusions to linter config files (e.g. `.rubocop.yml`). If a rule's suggestion is genuinely wrong for the code (rename would break behavior, etc.), restructure the code so the rule no longer fires, or ask before overriding it.
In Sorbet-typed Ruby code, best efforts should be taken to avoid `T.untyped` — it opts a value out of static checking entirely. Before reaching for it, look for a real type: a generated RBI (e.g. via `tapioca`), a `T.let`/`T.must` narrowing, a custom shim, or a small typed wrapper around an untyped boundary. Reserve `T.untyped` for boundaries that are genuinely untypeable (e.g. dynamically-defined methods with no RBI, values from a library with no type information) — and leave a short comment explaining why a real type isn't possible there.
Every new Ruby file should start `# typed: strict`. Only drop to a weaker sigil for the same kind of narrow, genuinely-unmodelable case as the existing exceptions — boot/environment setup (`lib/boot.rb`, `lib/environment.rb`, both `# typed: false`) and generated Sequel migrations (`# typed: ignore`, matching the template in `Rakefile`'s `db:new_migration` task) — and leave a comment explaining why `strict` doesn't fit.

- Ruby
- - Testing: Rspec
    Linting/Format: Rubocop
    LSP: ruby-lsp
    Type Checking: Sorbet

- Go
- - Testing: Go test
    Linting/Format: Golangci-lint
    LSP: gopls

- Client (VueJS)
- - Testing: Vue Test Utils
    Linting/Format: ESLint, Prettier
    LSP: TS Server, Vue Language Server

## Agent skills

### Issue tracker

Issues are tracked as GitHub issues on `namelessnotion/money_flow` (the `origin` remote); the `gitlab` remote is a mirror, not the tracker. See `docs/agents/issue-tracker.md`.

### Triage labels

Default label vocabulary: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Multi-context layout: `CONTEXT-MAP.md` at the root, with a `CONTEXT.md` + `docs/adr/` per context (`go/`, `ruby/`, `client/`). `go/` is MoneyFlow. `ruby/` and `client/` are the example application built on it. `proto/` is MoneyFlow's published language, which the example consumes; it is not its own context. See `docs/agents/domain.md`.
