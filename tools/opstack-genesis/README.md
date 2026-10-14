# opstack-genesis: build-allocs.py

Merges FISCO's self-written predeploy overlay onto **op-deployer terminal
allocs** and emits FISCO-BCOS genesis `[alloc.N]` INI fragments for L2 mode.
L2 mode is `version = 3` in the `[executor]` section of `config.genesis` (the
OP lane; there is no mode key and no feature flag); allocs require
`executor.version >= 2` and the OP lane requires allocs (`validateL2Invariants`).

## Input authority split

The op-deployer-generated terminal alloc JSON (`--base-allocs`, **required**)
is the ONLY source for the OP-Stack side of genesis — every `0x42…` predeploy
proxy, every `0xc0d3…` implementation, ProxyAdmin ownership and prefunded
accounts, including full proxy storage. The tool never synthesizes an OP
account. On top of the base it overlays exactly the FISCO self-written
predeploys (`SystemConfig`, `L2ValidatorSet`) in their post-deployment terminal
state: proxy account (canonical Proxy runtime bytecode copied from a
designated base-alloc proxy, `proxy_code_source`) + EIP-1967 slots,
implementation account (bcos-l2-contracts forge bytecode, `_initialized=255`),
and the proxy contract storage (`_initialized=1`, `Ownable.owner`, packed
SystemConfig entries / L2ValidatorSet records). `system_config` must not carry
`feature_flags`: the node no longer reads a feature-bitmap slot, and the build
rejects the entry.

Provenance: pin the frozen base allocs' sha256 as `base_allocs_sha256` in the
chain-config YAML (mirroring `bcos-l2-contracts/op-fork-pin.toml`
`[karst_pin].base_allocs_sha256`); an unpinned or mismatching base warns/fails
at `verify_base_provenance`.

## Usage

The self-written contracts and their forge artifacts live in the FISCO-BCOS
checkout (`bcos-l2-contracts/`), so point CONTRACTS at it — the Makefile's
relative default only fits a checkout laid out as siblings.

```bash
FISCO_REPO=/path/to/FISCO-BCOS

# 1. build the self-written contracts (SystemConfig / L2ValidatorSet)
make contracts CONTRACTS="$FISCO_REPO/bcos-l2-contracts"

# 2. copy the template and adjust the checklist/overlay for your chain
cp chain-config.template.yaml chain-config.yaml

# 3. emit allocs.ini (and optionally the geth-style alloc JSON that feeds the
#    op-reth oracle so both chains share one account set)
python3 build-allocs.py --config chain-config.yaml \
    --contracts "$FISCO_REPO/bcos-l2-contracts" \
    --base-allocs /path/to/op-deployer-l2genesis.json \
    --out allocs.ini --out-json allocs.json
```

`allocs.ini` is appended to `config.genesis`; `NodeConfig::loadAllocs()` parses
the `[alloc.N]` / `[alloc.N.storage]` sections, and the Ledger materializes
them at genesis (bytecode + storage verbatim; zero-valued storage slots are
elided — trie-equivalent — and addresses are lowercased).

Dependencies: Python 3 stdlib + `pyyaml`. Tests also need `pytest`.

## Fork activation: `[op_fork_timestamps]`

As in OP Stack, OP forks activate by L2 block timestamp: `[op_fork_timestamps]`
in `config.genesis` carries `isthmus_time` / `jovian_time` / `karst_time`
(seconds, from rollup.json). Isthmus is the OP-lane baseline and needs no
entry; any other absent key is a fork that never activates. A mid-chain
activation is a timestamp after genesis (`setup_c2.sh` `L2_JOVIAN_OFFSET`
writes the rollup.json `jovian_time` there). `[features]` is not read on
`executor.version >= 2`.

## chain-config.yaml fields

| field | meaning |
|-------|---------|
| `base_allocs_sha256` | expected sha256 of the frozen op-deployer base allocs (provenance pin) |
| `proxy_code_source` | base-alloc address whose Proxy runtime bytecode the overlay copies |
| `proxy_admin` / `governance_owner` | the two-authority split: EIP-1967 admin vs `Ownable.owner` (must differ) |
| `predeploys` | FISCO overlay predeploys: `{name, address, sol_file}`; the SystemConfig entry carries the `system_config` values (chain_id, gas_limit, …; `feature_flags` is rejected), the L2ValidatorSet entry carries `validators` records |
| `expected_predeploys` | existence checklist: each address must be present in the base allocs WITH code, else the build fails (guards against wrong/incomplete op-deployer output) |

## Safety rules (enforced by the build)

- An overlay address that already exists in the base allocs is a hard error.
- An `expected_predeploys` entry missing (or codeless) in the base fails the build.
- A self-written implementation artifact still carrying `immutableReferences`
  aborts the build naming the contract (zero-filled immutables would be broken
  runtime code); there is no opt-out.
- The EIP-1967 admin slot (upgrade authority) is ProxyAdmin while
  `Ownable.owner` (config/validator write authority) is the governance entity —
  two independent slots, never the same entity.

## Testing

```bash
python3 -m pytest test_build_allocs.py -v
```

The suite uses synthetic base-allocs + forge artifacts written to `tmp_path`;
it does not run `forge` or op-deployer. It covers the overlay layers, the
`expected_predeploys` checklist failure mode, provenance pinning, and the
immutables abort.
