package main

// P2-C fork-sensitivity tests: SELFDESTRUCT in both shapes (with the
// pre/post-Ecotone differential), the EIP-150 63/64 gas-boundary probe, and
// the fork-gated opcode arms (PUSH0 / MCOPY / BLOBHASH).

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// compile-time: predeploys live in the synthetic 0xc0de namespace.
var _ = common.HexToAddress

// isAllZeroAccount reports the postState all-zero shape emitPostState derives
// for a deleted/never-existing candidate: {"balance":"0x0"} (zero balance and
// nonce, no code, no storage).
func isAllZeroAccount(acc types.Account) bool {
	return acc.Balance != nil && acc.Balance.Sign() == 0 && acc.Nonce == 0 &&
		len(acc.Code) == 0 && len(acc.Storage) == 0
}

// TestLadderSelfdestructForkSensitive is THE P2-C differential anchor: the
// same 22-byte SELFDESTRUCT runtime, executed against an existing (genesis
// predeployed, 1 ETH) contract, deletes the account BEFORE Ecotone and only
// drains its balance FROM Ecotone on (EIP-6780 = Cancun = Ecotone in the OP
// ladder). Both shapes are asserted explicitly, including their difference.
func TestLadderSelfdestructForkSensitive(t *testing.T) {
	spec, err := parseLadderFlag(ladderScaledExplicit8, 100)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generateLadderChain(spec, 100)
	if err != nil {
		t.Fatalf("generateLadderChain: %v", err)
	}
	wantCode := ladderSelfdestructCode(recA)

	// shapeOf returns the block's postState row for a selfdestruct probe.
	shapeOf := func(b, seq int) types.Account {
		t.Helper()
		acc, ok := out.Blocks[b].PostState[ladderSelfdestructProbeAddr(seq)]
		if !ok {
			t.Fatalf("block %d: selfdestruct probe seq %d missing from postState", b, seq)
		}
		return acc
	}
	aliveBalance := big.NewInt(1_000_000_000_000_000_000)

	// seq 0 (regolith segment): alive on block 4, DELETED at the destruct
	// block 5, and the explicit all-zero row persists via the candidate set.
	if got := shapeOf(4, 0); got.Balance.Cmp(aliveBalance) != 0 || got.Nonce != 1 ||
		string(got.Code) != string(wantCode) {
		t.Fatalf("seq0 alive shape: bal=%s nonce=%d codeLen=%d", got.Balance, got.Nonce, len(got.Code))
	}
	deleted := shapeOf(5, 0)
	if !isAllZeroAccount(deleted) {
		t.Fatalf("seq0 (regolith) post-destruct shape not all-zero: %+v", deleted)
	}
	if got := shapeOf(17, 0); !isAllZeroAccount(got) {
		t.Fatalf("seq0 deletion must persist to later blocks: %+v", got)
	}

	// seq 1 (canyon segment, still pre-Ecotone): same deletion.
	if got := shapeOf(16, 1); got.Balance.Cmp(aliveBalance) != 0 || got.Nonce != 1 {
		t.Fatalf("seq1 alive shape: bal=%s nonce=%d", got.Balance, got.Nonce)
	}
	if got := shapeOf(17, 1); !isAllZeroAccount(got) {
		t.Fatalf("seq1 (canyon) post-destruct shape not all-zero: %+v", got)
	}

	// seq 2 (ecotone segment): the FORK-SENSITIVE shape. The account is KEPT:
	// balance drained to the beneficiary but nonce 1 and the 22-byte runtime
	// remain -- in explicit contrast to the deleted pre-Ecotone shape.
	if got := shapeOf(28, 2); got.Balance.Cmp(aliveBalance) != 0 || got.Nonce != 1 {
		t.Fatalf("seq2 alive shape: bal=%s nonce=%d", got.Balance, got.Nonce)
	}
	kept := shapeOf(29, 2)
	if isAllZeroAccount(kept) {
		t.Fatal("seq2 (ecotone) account must be KEPT after cross-tx SELFDESTRUCT (EIP-6780)")
	}
	if kept.Balance.Sign() != 0 || kept.Nonce != 1 || string(kept.Code) != string(wantCode) {
		t.Fatalf("seq2 kept shape: bal=%s nonce=%d codeLen=%d, want bal=0 nonce=1 codeLen=%d",
			kept.Balance, kept.Nonce, len(kept.Code), len(wantCode))
	}
	if got := shapeOf(99, 2); got.Balance.Sign() != 0 || got.Nonce != 1 ||
		string(got.Code) != string(wantCode) {
		t.Fatalf("seq2 kept shape must persist to the chain top: %+v", got)
	}
	// The two shapes are the differential itself.
	if isAllZeroAccount(kept) == isAllZeroAccount(deleted) {
		t.Fatal("pre/post-Ecotone cross-tx SELFDESTRUCT shapes must differ")
	}
}

// TestLadderSelfdestructSameTxDeletedEveryFork: the same-tx create+SELFDESTRUCT
// probe (init = PUSH20 beneficiary; SELFDESTRUCT, 0.1 ETH endowment) deletes
// the created account on EVERY fork (EIP-6780 same-tx exemption / unconditional
// before it). Receipt: status 1, empty output (no runtime returned -- so no
// FINDING-create-output row), and the postState row is the explicit all-zero.
func TestLadderSelfdestructSameTxDeletedEveryFork(t *testing.T) {
	spec, err := parseLadderFlag(ladderScaledExplicit8, 100)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generateLadderChain(spec, 100)
	blocks := []int{5, 17, 29, 42, 54, 67, 79, 92}
	for _, b := range blocks {
		blk := out.Blocks[b]
		found := false
		for i, raw := range blk.Block.Transactions {
			var st outputSignedTx
			if err := json.Unmarshal(raw, &st); err != nil {
				t.Fatal(err)
			}
			if st.To != nil || st.OpType != "eip1559" || st.Value == nil ||
				(*big.Int)(st.Value).Cmp(big.NewInt(ladderCreateDestructEndowment)) != 0 {
				continue
			}
			found = true
			created := crypto.CreateAddress(addrOfKey(1), uint64(st.Nonce))
			rc := blk.OpExpected.Receipts[i]
			if rc.Status != "0x1" || rc.Output != "0x" {
				t.Fatalf("block %d: same-tx destruct receipt status/output = %s/%s, want 0x1/0x",
					b, rc.Status, rc.Output)
			}
			acc := blk.PostState[created]
			if !isAllZeroAccount(acc) {
				t.Fatalf("block %d: created %s must be all-zero in postState: %+v",
					b, created.Hex(), acc)
			}
		}
		if !found {
			t.Fatalf("block %d: no same-tx create+destruct probe (To==nil, endowment)",
				b)
		}
	}
}

// TestLadderGasBoundaryProbe: the caller forwards ALL gas via CALL into the
// INVALID predeploy; the 63/64 rule retains 1/64 of the caller frame, so the
// receipt is status 1 with an exactly-pinnable gasUsed -- identical on every
// fork segment (no fork-dependent gas in the caller's path).
func TestLadderGasBoundaryProbe(t *testing.T) {
	spec, err := parseLadderFlag(ladderScaledExplicit8, 100)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generateLadderChain(spec, 100)
	if err != nil {
		t.Fatalf("generateLadderChain: %v", err)
	}
	for _, b := range []int{5, 17, 29, 42, 54, 67, 79, 92} {
		rs := ladderProbeReceipts(t, out.Blocks[b])
		calls := probeReceiptsTo(rs, ladderGasBoundaryCallerAddr)
		if len(calls) != 1 {
			t.Fatalf("block %d: want exactly 1 gas-boundary call, got %d", b, len(calls))
		}
		if calls[0].rc.Status != "0x1" {
			t.Fatalf("block %d: caller must survive the exhausted callee (63/64 keeps 1/64), status %s",
				b, calls[0].rc.Status)
		}
		// Pinned: 21000 intrinsic + dispatch + cold CALL 2600 + 63/64 of the
		// frame burned by the 0xfe callee + remainder. Observed 0x2423c.
		if calls[0].rc.GasUsed != "0x2423c" {
			t.Fatalf("block %d: gas-boundary gasUsed %s, want 0x2423c (148284)",
				b, calls[0].rc.GasUsed)
		}
	}
}

// TestLadderForkSensitiveOpcodes anchors the activation-boundary flips: an
// arm call is INVALID (status 0, gasUsed == gasLimit: the whole 100k burned)
// BEFORE its fork activates and succeeds after. PUSH0 flips at Canyon
// (Shanghai), MCOPY/BLOBHASH at Ecotone (Cancun).
func TestLadderForkSensitiveOpcodes(t *testing.T) {
	spec, err := parseLadderFlag(ladderScaledExplicit8, 100)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generateLadderChain(spec, 100)
	if err != nil {
		t.Fatalf("generateLadderChain: %v", err)
	}
	type armWant struct {
		sel    int
		status string
		gas    string
	}
	cases := map[int][]armWant{
		5:  {{0, "0x0", "0x186a0"}, {1, "0x0", "0x186a0"}, {2, "0x0", "0x186a0"}}, // regolith: all INVALID
		17: {{0, "0x1", "0x526f"}, {1, "0x0", "0x186a0"}, {2, "0x0", "0x186a0"}},  // canyon: PUSH0 only
		29: {{0, "0x1", "0x526f"}, {1, "0x1", "0x5296"}, {2, "0x1", "0x529f"}},    // ecotone: all live
		92: {{0, "0x1", "0x52a8"}, {1, "0x1", "0x52a8"}, {2, "0x1", "0x52a8"}},    // jovian: Prague gas
	}
	for b, wants := range cases {
		rs := ladderProbeReceipts(t, out.Blocks[b])
		calls := probeReceiptsTo(rs, ladderOpcodeProbeAddr)
		if len(calls) != 3 {
			t.Fatalf("block %d: want 3 opcode-arm calls, got %d", b, len(calls))
		}
		for _, w := range wants {
			got := calls[w.sel]
			if got.rc.Status != w.status || got.rc.GasUsed != w.gas {
				t.Fatalf("block %d arm %d: status/gasUsed = %s/%s, want %s/%s",
					b, w.sel, got.rc.Status, got.rc.GasUsed, w.status, w.gas)
			}
		}
	}
}

// TestLadderForkSensitivityPredeploys: every P2-C predeploy exists at genesis
// (nonce 1 + code; the cross-tx probes with 1 ETH each) and its runtime bytes
// are exactly the builders.
func TestLadderForkSensitivityPredeploys(t *testing.T) {
	spec, err := parseLadderFlag("0:regolith,2:canyon", 4)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generateLadderChain(spec, 4)
	if err != nil {
		t.Fatalf("generateLadderChain: %v", err)
	}
	pre := out.Blocks[0].Pre
	if pre == nil {
		t.Fatal("block 0 must carry pre")
	}
	check := func(a common.Address, wantCode []byte, wantBal int64) {
		t.Helper()
		acc, ok := (*pre)[a]
		if !ok {
			t.Fatalf("predeploy %s missing from genesis pre", a.Hex())
		}
		bal := (*big.Int)(acc.Balance)
		if acc.Nonce != 1 || string(acc.Code) != string(wantCode) || bal.Int64() != wantBal {
			t.Fatalf("predeploy %s: nonce=%d bal=%s codeLen=%d, want 1/%d/%d",
				a.Hex(), acc.Nonce, bal, len(acc.Code), wantBal, len(wantCode))
		}
	}
	for k := 0; k < ladderSelfdestructProbeCount; k++ {
		check(ladderSelfdestructProbeAddr(k), ladderSelfdestructCode(recA), 1_000_000_000_000_000_000)
	}
	check(ladderGasBoundaryCallerAddr, ladderGasBoundaryCallerCode(ladderInvalidProbeAddr), 0)
	check(ladderOpcodeProbeAddr, ladderOpcodeProbeCode(), 0)
}
