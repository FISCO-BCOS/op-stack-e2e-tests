package main

// Phase A″ stage 1/3: equivalence tests for l1BlockRuntimeCodeCombined.
//
// The combined runtime is the enabler for a single chain that crosses the
// Bedrock/Ecotone L1Block layout boundary (today generateLadderChain hard-
// rejects that crossing, ladder_chain.go:154-160, because one genesis account
// can only carry one runtime). The tests here execute the runtime on the REAL
// op-geth EVM (core/vm, the same machinery the block-level vectors go through)
// and prove:
//
//  1. composition: the bedrock / Isthmus-Jovian bodies are byte-identical
//     slices of the two existing family runtimes (no retyped bytecode), and the
//     Ecotone body is the Isthmus/Jovian body with only the slot8 segment
//     dropped;
//  2. per-selector storage equivalence against the existing family runtime for
//     Bedrock / Isthmus / Jovian (all 16 low slots compared, with seeded
//     sentinels so a zero-write into a seeded slot is observable);
//  3. the Ecotone path writes exactly l1BlockStorage("ecotone")'s slot set
//     {1,3,7} -- slot8 untouched -- and the PRE-EXISTING fact that the 146B
//     runtime REVERTS on the 164B Ecotone selector is recorded explicitly;
//  4. all four selectors are accepted, while unknown selectors and
//     calldatasize<4 revert (matching both existing runtimes).

import (
	"bytes"
	"math/big"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/holiman/uint256"
)

// combinedRuntimeProbeFork is the fork whose chain config drives the EVM call.
// Storage-only code, so the exact fork rules do not change the outcome; Isthmus
// is the newest config the corpus builds that has no Jovian-only EVM deltas
// relevant to SSTORE.
const combinedRuntimeProbeFork = "isthmus"

// l1Sentinel pre-seeds every low slot 0..15 with a distinct non-zero value, so
// the final storage reveals (a) which slots were written at all and (b) any
// zero-write that would be invisible on a fresh state.
func l1Sentinel() map[common.Hash]common.Hash {
	pre := map[common.Hash]common.Hash{}
	for i := 0; i < 16; i++ {
		pre[common.BigToHash(big.NewInt(int64(i)))] = common.BigToHash(big.NewInt(int64(0x5eed0000 + i)))
	}
	return pre
}

// l1BlockExec executes code as the L1Block predeploy on a real op-geth EVM and
// returns success, RETURN data, the final 0..15 storage (only non-zero entries)
// and gas used. pre seeds the contract's storage before the call.
func l1BlockExec(t *testing.T, code, data []byte, pre map[common.Hash]common.Hash) (bool, []byte, map[common.Hash]common.Hash, uint64) {
	t.Helper()
	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	addr := l1BlockAddr
	statedb.CreateAccount(addr)
	statedb.SetCode(addr, code, 0)
	for k, v := range pre {
		statedb.SetState(addr, k, v)
	}
	cfg, err := buildChainConfig(combinedRuntimeProbeFork)
	if err != nil {
		t.Fatalf("chain config: %v", err)
	}
	rnd := common.Hash{}
	blockCtx := vm.BlockContext{
		CanTransfer: core.CanTransfer,
		Transfer:    core.Transfer,
		GetHash:     func(uint64) common.Hash { return common.Hash{} },
		BlockNumber: big.NewInt(1),
		Time:        1000,
		Difficulty:  big.NewInt(0),
		BaseFee:     big.NewInt(1_000_000_000),
		BlobBaseFee: big.NewInt(1),
		Random:      &rnd,
		GasLimit:    30_000_000,
	}
	evm := vm.NewEVM(blockCtx, statedb, cfg, vm.Config{})
	caller := common.HexToAddress("0x1111111111111111111111111111111111111111")
	const gas = uint64(5_000_000)
	ret, gasLeft, err := evm.Call(caller, addr, data, gas, uint256.NewInt(0))
	st := map[common.Hash]common.Hash{}
	for i := 0; i < 16; i++ {
		k := common.BigToHash(big.NewInt(int64(i)))
		if v := statedb.GetState(addr, k); v != (common.Hash{}) {
			st[k] = v
		}
	}
	return err == nil, ret, st, gas - gasLeft
}

// slotVal reads slot i of a sparse storage map (missing == zero).
func slotVal(st map[common.Hash]common.Hash, i int) common.Hash {
	return st[common.BigToHash(big.NewInt(int64(i)))]
}

// writtenSlots returns the low-slot indices whose value differs from the
// sentinel pre-state (i.e. the runtime really wrote them).
func writtenSlots(pre, post map[common.Hash]common.Hash) map[int]bool {
	out := map[int]bool{}
	for i := 0; i < 16; i++ {
		if slotVal(pre, i) != slotVal(post, i) {
			out[i] = true
		}
	}
	return out
}

// l1BlockTestFeeParams is a feeParams with every Ecotone/Isthmus/Jovian field
// non-zero, so each family's writes carry distinguishable values.
func l1BlockTestFeeParams() feeParams {
	fp := defaultFeeParams()
	fp.opFeeScalar = 0x55c6fb7c
	fp.opFeeConstant = 1256417826609331460
	fp.daScalar = 0x1234
	return fp
}

// TestCombinedL1BlockComposition pins the byte-level composition: the combined
// runtime's Bedrock and Isthmus/Jovian bodies must be the EXACT bodies of the
// two existing family runtimes, and the Ecotone body must be their shared
// slot3+slot1+slot7 prefix plus RETURN (no slot8).
func TestCombinedL1BlockComposition(t *testing.T) {
	combined := l1BlockRuntimeCodeCombined
	if l1BlockCodeForLadder() == nil || !bytes.Equal(l1BlockCodeForLadder(), combined) {
		t.Fatal("l1BlockCodeForLadder must return l1BlockRuntimeCodeCombined")
	}
	// Layout: prefix 52B, revert 6B, bedrock body 24B, ecotone body 54B, ij body 103B.
	if len(combined) != 52+6+24+54+103 {
		t.Fatalf("combined runtime length: want %d, got %d", 52+6+24+54+103, len(combined))
	}
	bedrockBody := l1BlockRuntimeCodeBedrock[28:]
	ijBody := l1BlockRuntimeCode[43:]
	if !bytes.Equal(combined[58:58+len(bedrockBody)], bedrockBody) {
		t.Fatalf("combined[58:82] is not l1BlockRuntimeCodeBedrock's body")
	}
	if !bytes.Equal(combined[82+54:], ijBody) {
		t.Fatalf("combined[136:] is not l1BlockRuntimeCode's body")
	}
	// Ecotone body = JUMPDEST + ij slot3 + ij slot1 + ij slot7 + ij RETURN.
	ecotoneBody := combined[82:136]
	wantEcotone := make([]byte, 0, 54)
	wantEcotone = append(wantEcotone, 0x5b)
	wantEcotone = append(wantEcotone, ijBody[1:37]...)   // slot3 pack
	wantEcotone = append(wantEcotone, ijBody[37:43]...)  // slot1
	wantEcotone = append(wantEcotone, ijBody[43:49]...)  // slot7
	wantEcotone = append(wantEcotone, ijBody[98:103]...) // RETURN
	if !bytes.Equal(ecotoneBody, wantEcotone) {
		t.Fatalf("combined[82:136] is not the slot1/3/7-only Ecotone body:\n got %x\nwant %x", ecotoneBody, wantEcotone)
	}
}

// TestCombinedL1BlockSelectorEquivalence is the load-bearing equivalence test.
func TestCombinedL1BlockSelectorEquivalence(t *testing.T) {
	combined := l1BlockRuntimeCodeCombined
	fp := l1BlockTestFeeParams()
	pre := l1Sentinel()

	// --- Bedrock: combined == l1BlockRuntimeCodeBedrock, writes {1,5,6} ---
	bedrockData := fp.attributesData("regolith")
	okC, retC, stC, gasC := l1BlockExec(t, combined, bedrockData, pre)
	okB, retB, stB, gasB := l1BlockExec(t, l1BlockRuntimeCodeBedrock, bedrockData, pre)
	if !okC || !okB {
		t.Fatalf("bedrock selector: combined ok=%v, existing ok=%v (want both true)", okC, okB)
	}
	if !bytes.Equal(retC, retB) {
		t.Fatalf("bedrock selector: return data differs: %x vs %x", retC, retB)
	}
	if !reflect.DeepEqual(stC, stB) {
		t.Fatalf("bedrock selector: storage differs from l1BlockRuntimeCodeBedrock:\n got %v\nwant %v", stC, stB)
	}
	bedrockWritten := writtenSlots(pre, stC)
	assertWrittenSet(t, "bedrock", bedrockWritten, []int{1, 5, 6})
	assertWrittenValues(t, "bedrock", bedrockWritten, stC, fp.l1BlockStorage("regolith"))
	t.Logf("selector 0x015d8eb9 (Bedrock): ok=%v ret=%x gasUsed=%d (52B: gasUsed=%d) slots={1,5,6}",
		okC, retC, gasC, gasB)

	// --- Isthmus: combined == l1BlockRuntimeCode (146B), writes {1,3,7,8} ---
	isthmusData := fp.attributesData("isthmus")
	okC, retC, stC, gasC = l1BlockExec(t, combined, isthmusData, pre)
	okI, retI, stI, gasI := l1BlockExec(t, l1BlockRuntimeCode, isthmusData, pre)
	if !okC || !okI {
		t.Fatalf("isthmus selector: combined ok=%v, existing ok=%v (want both true)", okC, okI)
	}
	if !bytes.Equal(retC, retI) {
		t.Fatalf("isthmus selector: return data differs: %x vs %x", retC, retI)
	}
	if !reflect.DeepEqual(stC, stI) {
		t.Fatalf("isthmus selector: storage differs from l1BlockRuntimeCode:\n got %v\nwant %v", stC, stI)
	}
	assertWrittenSet(t, "isthmus", writtenSlots(pre, stC), []int{1, 3, 7, 8})
	assertWrittenValues(t, "isthmus", writtenSlots(pre, stC), stC, fp.l1BlockStorage("isthmus"))
	t.Logf("selector 0x098999be (Isthmus): ok=%v ret=%x gasUsed=%d (146B: gasUsed=%d) slots={1,3,7,8}",
		okC, retC, gasC, gasI)

	// --- Jovian: combined == l1BlockRuntimeCode (146B), writes {1,3,7,8} ---
	jovianData := fp.attributesData("jovian")
	okC, retC, stC, gasC = l1BlockExec(t, combined, jovianData, pre)
	okJ, retJ, stJ, gasJ := l1BlockExec(t, l1BlockRuntimeCode, jovianData, pre)
	if !okC || !okJ {
		t.Fatalf("jovian selector: combined ok=%v, existing ok=%v (want both true)", okC, okJ)
	}
	if !bytes.Equal(retC, retJ) {
		t.Fatalf("jovian selector: return data differs: %x vs %x", retC, retJ)
	}
	if !reflect.DeepEqual(stC, stJ) {
		t.Fatalf("jovian selector: storage differs from l1BlockRuntimeCode:\n got %v\nwant %v", stC, stJ)
	}
	assertWrittenSet(t, "jovian", writtenSlots(pre, stC), []int{1, 3, 7, 8})
	assertWrittenValues(t, "jovian", writtenSlots(pre, stC), stC, fp.l1BlockStorage("jovian"))
	// DA scalar must really be present in slot8 bytes [18:20].
	slot8 := slotVal(stC, 8)
	if !bytes.Equal(slot8[18:20], []byte{0x12, 0x34}) {
		t.Fatalf("jovian slot8 DA scalar: want 0x1234, got %x", slot8[18:20])
	}
	t.Logf("selector 0x3db6be2b (Jovian): ok=%v ret=%x gasUsed=%d (146B: gasUsed=%d) slots={1,3,7,8}",
		okC, retC, gasC, gasJ)

	// --- Ecotone: 146B runtime REVERTS (pre-existing fact); combined accepts,
	// writing exactly l1BlockStorage("ecotone")'s {1,3,7} and never slot8. ---
	ecotoneData := fp.attributesData("ecotone")
	ok146, ret146, st146, _ := l1BlockExec(t, l1BlockRuntimeCode, ecotoneData, pre)
	if ok146 {
		t.Fatalf("PRE-EXISTING FACT CHANGED: l1BlockRuntimeCode no longer reverts on the Ecotone selector")
	}
	if !reflect.DeepEqual(st146, pre) {
		t.Fatalf("reverting 146B runtime changed storage: %v", st146)
	}
	okOld, _, _, _ := l1BlockExec(t, l1BlockRuntimeCodeBedrock, ecotoneData, pre)
	if okOld {
		t.Fatalf("Bedrock runtime unexpectedly accepted the Ecotone selector")
	}
	okC, retC, stC, gasC = l1BlockExec(t, combined, ecotoneData, pre)
	if !okC {
		t.Fatal("combined runtime REJECTED the Ecotone selector (A″'s core purpose)")
	}
	if len(retC) != 0 {
		t.Fatalf("ecotone selector: return data want empty, got %x", retC)
	}
	// Slot-set equality with l1BlockStorage("ecotone"): {1,3,7}; slot8 NOT written.
	assertWrittenSet(t, "ecotone", writtenSlots(pre, stC), []int{1, 3, 7})
	if got := slotVal(stC, 8); got != slotVal(pre, 8) {
		t.Fatalf("ecotone must not write slot8 (operator-fee params are Isthmus+ only): got %x", got)
	}
	assertWrittenValues(t, "ecotone", writtenSlots(pre, stC), stC, fp.l1BlockStorage("ecotone"))
	t.Logf("selector 0x440a5e20 (Ecotone): ok=%v ret=%x gasUsed=%d slots={1,3,7} (slot8 untouched); "+
		"146B runtime on same calldata: ok=%v ret=%x (pre-existing revert)", okC, retC, gasC, ok146, ret146)

	// --- unknown selector / calldatasize<4 revert, matching both runtimes ---
	badInputs := map[string][]byte{
		"unknown 0xdeadbeef": {0xde, 0xad, 0xbe, 0xef},
		"empty":              nil,
		"3-byte prefix":      {0x01, 0x5d, 0x8e},
	}
	for name, data := range badInputs {
		okC, _, stC, _ := l1BlockExec(t, combined, data, pre)
		if okC {
			t.Fatalf("%s: combined runtime accepted an invalid input", name)
		}
		if !reflect.DeepEqual(stC, pre) {
			t.Fatalf("%s: reverting combined runtime changed storage", name)
		}
		for _, existing := range []struct {
			name string
			code []byte
		}{{"bedrock52", l1BlockRuntimeCodeBedrock}, {"146B", l1BlockRuntimeCode}} {
			if ok, _, _, _ := l1BlockExec(t, existing.code, data, pre); ok {
				t.Fatalf("%s: existing %s runtime accepted an invalid input", name, existing.name)
			}
		}
	}
}

// TestCombinedL1BlockAcceptsAllFourSelectors is A″'s core acceptance statement:
// every family selector executes without reverting and yields the family's
// canonical storage. It also logs the measured per-selector values.
func TestCombinedL1BlockAcceptsAllFourSelectors(t *testing.T) {
	combined := l1BlockRuntimeCodeCombined
	fp := l1BlockTestFeeParams()
	for _, tc := range []struct {
		fork   string
		layout string
	}{
		{"regolith", "bedrock"},
		{"ecotone", "ecotone"},
		{"isthmus", "isthmus"},
		{"jovian", "jovian"},
	} {
		data := fp.attributesData(tc.fork)
		ok, ret, st, gas := l1BlockExec(t, combined, data, l1Sentinel())
		if !ok {
			t.Fatalf("%s (%s layout): combined runtime reverted", tc.fork, tc.layout)
		}
		written := writtenSlots(l1Sentinel(), st)
		t.Logf("%-8s layout=%-7s dataLen=%3d ok=%v ret=%x gasUsed=%d written=%v",
			tc.fork, tc.layout, len(data), ok, ret, gas, sortedSlots(written))
	}
}

// assertWrittenSet asserts the exact set of slots a runtime wrote.
func assertWrittenSet(t *testing.T, label string, got map[int]bool, want []int) {
	t.Helper()
	wantSet := map[int]bool{}
	for _, i := range want {
		wantSet[i] = true
	}
	if !reflect.DeepEqual(got, wantSet) {
		t.Fatalf("%s: written slot set: want %v, got %v", label, want, sortedSlots(got))
	}
}

// assertWrittenValues asserts that every slot the runtime actually wrote holds
// exactly the value l1BlockStorage pre-seeds for that fork (the iron rule: the
// runtime's writes reproduce the seeded layout), and that no written slot falls
// outside the fork's storage layout.
func assertWrittenValues(t *testing.T, label string, written map[int]bool, got, want map[common.Hash]common.Hash) {
	t.Helper()
	for i := range written {
		slot := common.BigToHash(big.NewInt(int64(i)))
		wantVal, ok := want[slot]
		if !ok {
			t.Fatalf("%s: runtime wrote slot %d, which is not in l1BlockStorage(%s)", label, i, label)
		}
		if gotVal := got[slot]; gotVal != wantVal {
			t.Fatalf("%s: slot %d: want %s, got %s", label, i, wantVal.Hex(), gotVal.Hex())
		}
	}
}

func sortedSlots(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for i := range set {
		out = append(out, i)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
