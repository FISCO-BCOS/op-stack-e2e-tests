# FISCO-BCOS OP-Stack e2e tests

Independent repository for the FISCO-BCOS OP-Stack execution-client (EL) e2e
test assets. This repo holds everything that drives a devnet or generates
reference data **without compiling the FISCO-BCOS main repository**; the
white-box C++ e2e/integration tests that link against the EL internals stay in
[FISCO-BCOS/FISCO-BCOS](https://github.com/FISCO-BCOS/FISCO-BCOS)
(`opstack-executor/tests/*`, `bcos-rpc/test/unittests/rpc/*`).

## Test layering

| Layer | Location | Nature |
|---|---|---|
| Black-box devnet e2e | `tools/op-e2e/` (this repo) | Scripts driving anvil L1 + op-node/op-deployer (from the optimism monorepo) + a FISCO-BCOS L2 binary over RPC/Engine API |
| Genesis toolchain | `tools/opstack-genesis/` (this repo) | Genesis allocs/rollup-config/header generation feeding the B3/C2 chains |
| t8n reference vectors | `opstack-executor/tests/t8n/` (this repo) | Go generator + pinned op-geth regen pipeline + contract anchors/manifests consumed by the C++ replay gate |
| White-box C++ gate | `opstack-executor/tests/OpT8nReplayTest.cpp` (main repo) | Compiles into `opstack-executor-block-tests`; reads `t8n/vectors` + `t8n/golden` via the `OP_T8N_VECTORS_DIR` / `OP_T8N_GOLDEN_ENGINE_DIR` compile-time macros |

## Contents

- `tools/op-e2e/` — devnet drivers: `setup_c2.sh` (anvil L1 + op-deployer +
  op-node + FISCO L2 full loop), `setup_op_node.sh` (B3/B3a gate),
  `run_all.sh` (suite aggregator), deposit/withdraw/xdm/resilience scenario
  scripts, `nonce_fee_boundaries.sh`, Python drivers (`c2lib.py`,
  `chain_driver.py`, ...), `versions.json` (pinned optimism monorepo commit and
  on-chain contract versions, ENFORCED by `setup_c2.sh`).
- `tools/opstack-genesis/` — `build-allocs.py`, `gen_rollup_config.py`,
  `gen_eth_header_fixture.py`, `mpt_state_root.py`, `op-fork-base-allocs.json`,
  chain configs. Fed by the main repo's `tools/opstack-genesis` history; see
  its README.
- `opstack-executor/tests/t8n/` — `generator/` (Go case definitions +
  `regen.sh` + `ensure-vectors.sh`), `vectors/` (hand-maintained divergence
  docs + tracked anchors; the generated `vectors/*.json` are git-ignored and
  materialized by the regen pipeline), `golden/` (engine golden manifests and
  `SHA256SUMS`). Regen requires the pinned op-geth commit (see
  `generator/regen.sh` header) — local and CI use `ensure-vectors.sh`.
- `.github/actions/opstack-t8n-regen/` — composite action delegating to
  `ensure-vectors.sh`; consumed by the main repo's workflows (Phase 2 wiring).

## Relationship with the main repository

Migrated from `FISCO-BCOS/FISCO-BCOS` (`sync-release-3.18.0`, commit
`6f7b45c0a`). The main repo will drop the duplicated directories once the CI
wiring (Phase 2) is green; until then this repo is the canonical source of the
black-box assets and the main repo retains its current copies.

The C++ replay gate needs the generated vectors at build time. Main-repo CI
clones this repo (pinned commit), runs `ensure-vectors.sh`, and passes the
resulting paths to cmake via `-DOP_T8N_VECTORS_DIR=... -DOP_T8N_GOLDEN_ENGINE_DIR=...`
(the macros default to the in-tree paths when unset, so a main-repo checkout
that still carries `opstack-executor/tests/t8n/` keeps working).

## Running

Black-box suites need: a FISCO-BCOS `fisco-bcos-air` binary (built from the
main repo), foundry (`anvil`/`forge`), and for C2 the optimism monorepo tools
(`op-node`, `op-deployer`, `op-batcher`). See `tools/op-e2e/setup_op_node.sh`
and `setup_c2.sh` headers; version pins live in `tools/op-e2e/versions.json`.

t8n vector regen:

```bash
bash opstack-executor/tests/t8n/generator/ensure-vectors.sh
```

## Status

- Phase 1 (migration of black-box + t8n data-plane assets): done.
- Phase 2 (CI wiring — workflows for C2/B3/static checks, main-repo job
  handoff): pending.
