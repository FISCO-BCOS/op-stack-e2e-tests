# Provenance

`tools/op-e2e` is migrated from the FISCO-BCOS monorepo (FISCO-BCOS/FISCO-BCOS,
PR #5593, branch `feat/engine-cutover-on-prereqs` @ `17fcff1ea`). The monorepo
review history for these files lives there; this copy is the authoritative home
going forward and must be kept in lockstep with the monorepo's `versions.json`
consumer `tools/.ci/c2-e2e.sh`, which checks out this repository at a pinned ref.

Splits of record:
- `2ef9d0c` — initial migration (pre-#5593 snapshot).
- `feat/sync-with-monorepo-5593` — sync to the reviewed #5593 head: review fixes
  (F6-F10, F20-F25), the `FISCO_REPO` parameterization that decouples this
  checkout from the monorepo layout, and the harness-local `versions.json`.
