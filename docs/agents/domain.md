# Domain Docs

How the engineering skills should consume this repo's domain documentation when exploring the codebase.

## Before exploring, read these

- **`CONTEXT-MAP.md`** at the repo root: it points at one `CONTEXT.md` per context (`go/`, `ruby/`, `client/`). Read each one relevant to the topic.
- **`docs/adr/`**: system-wide decisions, including MoneyFlow's `proto/` contract. Also check `<context>/docs/adr/` (e.g. `ruby/docs/adr/`) for context-scoped decisions.

If any of these files don't exist, **proceed silently**. Don't flag their absence; don't suggest creating them upfront. The `/domain-modeling` skill (reached via `/grill-with-docs` and `/improve-codebase-architecture`) creates them lazily when terms or decisions actually get resolved.

## File structure

This is a multi-context repo. Each bounded context lives at the repo's top level rather than under `src/`:

```
/
├── CONTEXT-MAP.md
├── docs/adr/                          ← system-wide decisions (incl. the proto/ contract)
├── proto/                             ← MoneyFlow's published language
├── go/                                ← MoneyFlow
│   ├── CONTEXT.md
│   └── docs/adr/                      ← go-context-specific decisions
├── ruby/                              ← example application: business backend
│   ├── CONTEXT.md
│   └── docs/adr/                      ← ruby-context-specific decisions
└── client/                            ← example application: front end
    ├── CONTEXT.md
    └── docs/adr/                      ← client-context-specific decisions
```

MoneyFlow is `go/` plus `proto/`. `ruby/` and `client/` are **an example of building on MoneyFlow**, kept as the reference integration and not part of the product. The relationship is Open Host Service + Published Language:

- `go/` exposes Twirp services and publishes events and knows nothing about any consumer.
- `ruby/` is a downstream customer that uses only that surface.

When a change is proposed, decide which side it belongs to. A rule every integrator would need belongs in `go/`, even when the example is what surfaced it. A business rule specific to the example's marketplace (ACH, Securities) belongs in `ruby/`.

`proto/` is not itself a bounded context. It is MoneyFlow's Published Language: protobuf-defined commands, events and Twirp services. Decisions about that contract are system-wide and belong in the root `docs/adr/`, not a per-context one.

## Use the glossary's vocabulary

When your output names a domain concept (in an issue title, a refactor proposal, a hypothesis, a test name), use the term as defined in the relevant context's `CONTEXT.md`. Don't drift to synonyms the glossary explicitly avoids, and don't assume a term means the same thing across `go/`, `ruby/`, and `client/` — check the owning context's glossary.

If the concept you need isn't in the glossary yet, that's a signal: either you're inventing language the project doesn't use (reconsider) or there's a real gap (note it for `/domain-modeling`).

## Flag ADR conflicts

If your output contradicts an existing ADR (root-level or context-specific), surface it explicitly rather than silently overriding:

> _Contradicts ADR-0007 (event-sourced orders), but worth reopening because…_
