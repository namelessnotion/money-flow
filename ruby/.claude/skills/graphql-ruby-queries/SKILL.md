---
name: graphql-ruby-queries
description: >
  How to add or change a GraphQL query field or object type in the money_flow
  ruby app (graphql-ruby + Sequel + Sorbet). Covers where files go, the
  Relay-connection pagination pattern, selecting only requested DB columns via
  GraphQL::Execution::Lookahead, avoiding N+1s with Sequel's `.eager`, a real
  gotcha where eager-loading silently breaks under graphql-ruby's connection
  wrapper, Sorbet typing for untyped Sequel dataset methods, and the SQL-log
  testing pattern for proving a query is actually batched. Use this whenever
  touching anything under ruby/app/graphql — adding a new query, adding a
  field to an existing GraphQL type, adding an association to expose, or
  debugging why a GraphQL query is slow or issuing more SQL than expected.
  Triggers on mentions of GraphQL queries/types/resolvers, Sequel eager
  loading, N+1 queries, or pagination in the ruby/ app, even if the user
  doesn't name this skill directly.
---

# GraphQL queries in the money_flow ruby app

This app pairs graphql-ruby with Sequel (not ActiveRecord) and Sorbet. All
three interact in ways that aren't obvious from generic GraphQL-Ruby docs —
this skill exists because a straightforward-looking implementation silently
issued one SQL query per row instead of one query total. Read the "Eager
loading gotcha" section below before trusting `.eager` inside a connection
field.

Treat [app/graphql/types/objects/entity.rb](../../../app/graphql/types/objects/entity.rb),
[app/graphql/types/objects/account.rb](../../../app/graphql/types/objects/account.rb),
and [app/graphql/types/query_type.rb](../../../app/graphql/types/query_type.rb)
as the reference implementation. When in doubt about a detail this file
doesn't spell out, read those files rather than guessing — they're real,
tested, passing code, and paths quoted here will drift less than prose would.

## Where things go

- Query fields (the `Query` root type) live in `app/graphql/types/query_type.rb`.
- One object type per model in `app/graphql/types/objects/<model>.rb`, named
  `Types::<Model>` and inheriting `Types::BaseObject`.
- Each type file `require_relative`s the files it directly depends on (e.g.
  `entity.rb` requires `account.rb` because it embeds `Types::Account`) even
  though `Environment.load_app!` eventually `Dir.glob`s everything — the
  explicit require documents the real dependency and keeps the file loadable
  on its own.
- After changing anything schema-visible (new field, new type, changed
  description), regenerate the committed SDL dump:

  ```bash
  bundle exec rake graphql:schema:dump
  ```

  `spec/app/graphql/schemas/money_flow_schema_spec.rb` fails the build if
  `app/graphql/schemas/money_flow_schema.graphql` is stale, so this isn't
  optional — it's how schema drift gets caught in review.

## Pagination: Relay connections, not plain lists

A query that returns "all the X" should return a connection, not `[Types::X]`:

```ruby
field :entities, Types::Entity.connection_type, null: false,
                                                default_page_size: 100, max_page_size: 100,
                                                extras: [:lookahead],
                                                description: 'Onboarded entities, paginated at 100 per page.'
```

`Types::X.connection_type` builds the Relay `edges`/`nodes`/`pageInfo` wrapper
type on demand. graphql-ruby auto-detects a field as a connection when the
type name ends in `Connection` and, from that, automatically adds the
`first`/`after`/`last`/`before` arguments and pagination handling — you don't
declare those arguments yourself.

`default_page_size:` and `max_page_size:` aren't decoration: without
`default_page_size`, a client that omits `first`/`last` gets an *unbounded*
result set (or whatever `max_page_size` allows), which is exactly the
unbounded-query problem pagination exists to prevent. Set both explicitly on
every list-returning field, matching whatever page size the field's docs
promise.

`extras: [:lookahead]` gives the resolver method a `lookahead:` keyword arg —
see the next section for what to do with it.

Sequel note: when a resolver returns a `Sequel::Dataset`, graphql-ruby's
`GraphQL::Pagination::SequelDatasetConnection` handles the slicing (LIMIT/
OFFSET, cursor math, `pageInfo`) for you automatically, because `Sequel::Dataset`
is registered as a connection-wrappable type out of the box. Give the
connection a deterministic base ordering (e.g. `Models::Entity.dataset.order(:id)`)
before it's sliced — cursors over an unordered dataset aren't meaningful.

## Selecting only requested columns

Fetching every column when a query only asked for `id` and `name` is wasted
I/O that scales with row count. Each object type declares a field→column map
and a class method that walks the field's `Lookahead` to build a `.select`
list:

```ruby
COLUMNS_BY_FIELD = T.let({
  id: :id,
  name: :name,
  holder_uuid: :holder_uuid,
  created_at: :created_at,
  updated_at: :updated_at
}.freeze, T::Hash[Symbol, Symbol])
```

- **Nested / non-connection types** (e.g. `Types::Account`, reached through
  `entity.accounts`) expose `self.selected_columns(lookahead)`, returning an
  array of column symbols with the primary key always included (see
  `app/graphql/types/objects/account.rb`).
- **The type at the top of a connection field** (e.g. `Types::Entity`, reached
  through `Query#entities`) exposes `self.scope(lookahead, dataset)`, which
  both selects its own columns *and* decides whether to eager-load
  associations (see `app/graphql/types/objects/entity.rb`).

The one wrinkle specific to connections: a client can ask for the page either
as `nodes { ... }` or as `edges { node { ... } }`, and both are valid Relay
usage. The field's own `lookahead` is for the connection wrapper itself, not
for either of those — you have to explicitly look under both paths and treat
whichever ones are actually selected as "the real field selections":

```ruby
def node_selections(lookahead)
  [lookahead.selection(:nodes), lookahead.selection(:edges).selection(:node)].select(&:selected?)
end
```

Don't assume a client uses only one style — a query using `edges { node { name } }`
should get exactly the same column selection as one using `nodes { name }`.

Keep the scope-building method itself small. It's tempting to write one method
that selects entity columns, checks for the accounts sub-selection, and builds
the eager proc all in one place — but that reads as one big conditional and
trips `Metrics/AbcSize` / `Metrics/CyclomaticComplexity` / `Metrics/PerceivedComplexity`
in Rubocop. Split it into `scope` (orchestrates), `selected_columns` (just the
column list), and `eager_<association>` (just the eager-load decision), as
`entity.rb` does.

## Avoiding N+1s: Sequel `.eager`, correctly

When a requested field pulls in an association (e.g. `entity.accounts`), the
naive fix — GraphQL resolving `entity.accounts` once per entity — issues one
query per parent row. Sequel's `.eager` batches this into one extra query
total, but only if used correctly:

```ruby
foreign_key = Models::Entity.association_reflection(:accounts).fetch(:key)
dataset.eager(accounts: proc { |ds| ds.select(*(account_columns | [foreign_key])) })
```

The foreign key line is not optional. Sequel's own `.eager` machinery only
auto-adds the association's key column back into a custom `.select` for
association types that structurally require it (e.g. `many_to_many`, via its
join table). For a `one_to_many`/`many_to_one` — the common case — if you
`.select` inside the eager block and leave out the foreign key
(`entity_id` here), Sequel still runs one batched query, still filters it
correctly by `WHERE entity_id IN (...)`, but then **silently fails to match
the returned rows back to their parents**, because the values it groups by are
missing from what came back. You get empty association arrays on every
parent, with no error anywhere. Get the actual key column from the model's
own association reflection (`Model.association_reflection(:name).fetch(:key)`)
rather than hardcoding the FK column name, so this keeps working if the
association is ever redefined.

### The eager-loading gotcha (already fixed — know why before touching it)

Even with the foreign key included, `.eager(...)` on a dataset returned from a
**connection field** still silently degraded to one query per row when this
was first implemented. The cause: `GraphQL::Pagination::SequelDatasetConnection`
(via its parent `RelationConnection`) materializes the current page by calling
`Dataset#to_a`. Sequel never defines `to_a`, so Ruby falls back to
`Enumerable#to_a`, which drives the dataset through plain `#each` — and
Sequel's eager-loading post-processing (matching associated rows back to their
parents) is wired into `Dataset#all`, not `#each`. So `#to_a` produces the
right *entities*, but never triggers the eager batch — each entity's
`.accounts` association cache stays empty, and the field resolver falls back
to a normal per-instance query.

This is already fixed at the schema level in
[app/graphql/connections/sequel_dataset_connection.rb](../../../app/graphql/connections/sequel_dataset_connection.rb):
a `Connections::SequelDatasetConnection` subclass overrides the private
`load_nodes` to call `.all` instead of the inherited `.to_a`, registered once
in [app/graphql/schemas/money_flow_schema.rb](../../../app/graphql/schemas/money_flow_schema.rb)
via `connections.add(Sequel::Dataset, Connections::SequelDatasetConnection)`.

**Do not remove or "simplify" this override** — without it, every connection
field that eager-loads an association regresses to N+1 with no test failure
to catch it unless that specific query-count assertion exists (see Testing,
below). If you add a *new* connection field that eager-loads an association,
this fix already covers it automatically; you don't need to repeat anything
per-field. What you do need to repeat is the verification: write the same
kind of query-counting test for the new field, because "I called `.eager`" is
not sufficient evidence that batching is actually happening through a
connection — the only way to know is to count the queries.

## Sorbet: the Sequel boundary is typed, not `T.untyped`

`Sequel::Model::Associations` methods like `.eager` — and `.select`/`.where`
in their association-aware sense — are `extend`ed onto a model's dataset at
runtime. Sorbet's static types have no visibility into that by default, so
typing a parameter or return value as plain `Sequel::Dataset` and then
calling `.eager` on it fails `srb tc` with real errors like `Method 'eager'
does not exist on 'Sequel::Dataset'`.

This is solved, not worked around: the `tapioca-sequel` gem
(github.com/namelessnotion/tapioca-sequel) is a Tapioca DSL compiler that
generates a per-model fictional dataset class, `Models::<Model>::PrivateDataset`,
with every chainable query method (`where`, `select`, `eager`, `order`, ...)
typed to take/return that same class — so a `.select(...).eager(...)` chain
stays fully typed end to end. It also types association methods
(`entity.accounts`, `entity.accounts_dataset`, `add_account`, ...). Regenerate
the RBIs after adding a model, a column, or an association:

```bash
bin/tapioca dsl Models::Entity Models::Account
```

**`PrivateDataset` is a static-only fiction** — like Tapioca's ActiveRecord
`PrivateRelation` pattern, no such class exists at runtime; it lives only in
the generated `.rbi` file, which Ruby never loads. A normal `sig` block in
real `.rb` source referencing `Models::Entity::PrivateDataset` would raise
`NameError` the first time the method runs, because sorbet-runtime resolves
the constant lazily on first call. Use `T::Sig::WithoutRuntime.sig` instead
of `sig` at these boundaries (see `scope` / `eager_accounts` in `entity.rb`,
and `entities` in `query_type.rb`) — it's checked by `srb tc` exactly like a
normal `sig`, but never installs a runtime wrapper, so the fictional
constant is never forced. This is the same category of problem
`services/onboard_entity.rb` solves differently (with `T.untyped`) for the
generated protobuf/twirp boundary, which has no equivalent typed fiction to
reach for.

One real Sorbet limitation survives this: splatting a plain `T::Array[Symbol]`
(length unknown statically) into `dataset.select(*columns)` fails with
`Splats are only supported where the size of the array is known statically`
(https://srb.help/7019) — this is unrelated to Sequel and would happen for
any typed method call. Wrap just the array, not the dataset, in `T.unsafe`
at that one call site (see `scope` in `entity.rb`); everything else on the
chain stays checked.

`app/graphql/connections/sequel_dataset_connection.rb` is `typed: strict` too,
despite overriding a private method and memoizing into `@nodes` — an
instance variable it doesn't declare, owned by the gem superclass
(`RelationConnection#nodes` reads `@nodes` directly). The memoization must
use the `@nodes ||= T.let(expr, T.nilable(X))` form, not a `return @nodes if
@nodes` guard plus a separate assignment: that's the one pattern Sorbet
narrows back to a non-nilable return type, since the gem's own methods
(`limited_nodes`, `.all`) carry no sigs and resolve as `T.untyped`. Don't
rename `@nodes` to satisfy Rubocop's `Naming/MemoizedInstanceVariableName` —
it has to stay `@nodes` for the override to actually take effect; disable
that cop inline instead.

## Testing

Mirror the source path: `app/graphql/types/query_type.rb` →
`spec/app/graphql/types/query_type_spec.rb`. Execute queries the same way the
app does — `MoneyFlowSchema.execute(query, variables:).to_h` — and assert on
`result['data']` / `result['errors']`. Tests run inside a DB transaction that
always rolls back (`spec/spec_helper.rb`), so it's fine to hit the real
Postgres database rather than stubbing Sequel.

For anything claiming "selects only these columns" or "doesn't N+1", assert
on the actual SQL rather than trusting the code read right — capture it by
pushing a small recorder onto `DB.loggers` for the duration of the query:

```ruby
def capture_sql
  statements = []
  recorder = Object.new
  %i[info warn error].each { |level| recorder.define_singleton_method(level) { |message| statements << message } }

  DB.loggers << recorder
  yield
  statements
ensure
  DB.loggers.delete(recorder)
end
```

Then assert things like `sqls.count { |s| s.include?('FROM "accounts"') }`
equals 1 regardless of how many parent rows there are (proves batching, not
just correctness), or that a SQL string starts with the exact expected
`SELECT` column list. See
[spec/app/graphql/types/query_type_spec.rb](../../../spec/app/graphql/types/query_type_spec.rb)
for the full pattern, including a default-page-size test that creates 101
rows to prove the cap is actually enforced rather than merely configured.

## Checklist before calling a GraphQL change done

```bash
bundle exec rspec
bundle exec rubocop
bundle exec srb tc
bundle exec rake graphql:schema:dump
```

Run the schema dump last — rubocop or sorbet fixes can change field
descriptions or shapes, and the dump should reflect the final state. If the
dump task changes the `.graphql` file, that's a sign the schema spec would
otherwise have failed in CI.
