# TLC

`tla2tools.jar` is vendored so every run of `make tla-check` and `make tla-trace`, locally and in CI, uses the same
model checker. The specs need what only the v1.8.0 line has: the `Json` standard module (`ndJsonDeserialize`, used
by `Traceeventstore.tla`), and `POSTCONDITION`. GitHub rebuilds the v1.8.0 pre-release in place, so its URL can't be
pinned.

- **Source:** https://github.com/tlaplus/tlaplus/releases/download/v1.8.0/tla2tools.jar, as published 2026-09-25
- **Reports itself as:** `TLC2 Version 2026.09.25.020137 (rev: 5d53605)`
- **sha256:** `7c6a30fcfca96c6d7476e705a545837afbf66446c3fcb34bf39b838cd50ee0c0`, which matches the digest GitHub
  published for that asset
- **Needs:** Java 11 or later

## Upgrading

1. Download the new jar over this one, and update the three lines above.
2. Run `make tla-check` and `make tla-trace`. A TLC upgrade can change results, and these runs show whether it did.

# Alloy

`make alloy-check` runs `spec/alloy/ledger.als` with Alloy 6's command-line `exec`. The jar isn't vendored. Unlike
TLC's pre-release, v6.2.0 is a fixed release, so the Makefile fetches it from a pinned URL on first use and checks
the download's digest.

- **Source:** https://github.com/AlloyTools/org.alloytools.alloy/releases/download/v6.2.0/org.alloytools.alloy.dist.jar
- **sha256:** `6b8c1cb5bc93bedfc7c61435c4e1ab6e688a242dc702a394628d9a9801edb78d` (`ALLOY_SHA256` in the Makefile)
- **Solver:** glucose, which ships native in the jar for linux/amd64 and darwin. It's much faster than the default,
  SAT4J.
- **Needs:** Java 17 or later

To upgrade, bump `ALLOY_VERSION` and `ALLOY_SHA256`, delete the cached jar, and run `make alloy-check`.
