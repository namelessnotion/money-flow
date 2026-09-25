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
