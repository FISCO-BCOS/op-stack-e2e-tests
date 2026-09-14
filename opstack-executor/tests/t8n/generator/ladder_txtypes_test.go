package main

// P2-B transaction-type diversity tests: the buildTx accesslist (type 0x01)
// arm and the per-fork-segment ladder injections (2930 / legacy / 7702).

import (
	"bytes"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

// ladderScaledExplicit8 mirrors the registered ladder's shape (every fork
// activated EXPLICITLY at its own block, no cumulative-coupling jumps) at
// 1/10 scale, 100 blocks. Coupled-jump specs (e.g. 0:regolith,60:isthmus)
// trip a PRE-EXISTING generator limitation at the jump block (L1-fee vault
// cross-check on the activation deposit: vault delta != 0 while the expected
// per-tx L1 fee sum is 0; reproduced on clean HEAD before P2-B touched
// anything). The registered ladder activates every fork explicitly and never
// hits it; this scaled mirror keeps that property.
const ladderScaledExplicit8 = "0:regolith,12:canyon,25:ecotone,37:fjord,50:granite," +
	"62:holocene,75:isthmus,87:jovian"

func ladderScaledExplicit8Blocks(t *testing.T) []int {
	t.Helper()
	spec, err := parseLadderFlag(ladderScaledExplicit8, 100)
	if err != nil {
		t.Fatal(err)
	}
	var idxs []int
	for _, a := range spec.Activations {
		if a.Block > 0 {
			idxs = append(idxs, a.Block-1)
		}
	}
	return ladderCreateCallProbeBlocks(100, idxs)
}

// TestBuildTxAccessListArm anchors the buildTx accesslist (EIP-2930) arm: the
// signed tx is a real 0x01 envelope, the output object mirrors the tuple list
// with nil StorageKeys normalized to [], and the sender recovery matches the
// signing key.
func TestBuildTxAccessListArm(t *testing.T) {
	cfg, err := buildChainConfig("isthmus")
	if err != nil {
		t.Fatal(err)
	}
	signer := types.MakeSigner(cfg, big.NewInt(1), 2000)

	// nil StorageKeys must normalize to [] on the output side (same contract
	// as the eip1559 arm's buildAccessList).
	in := accessListTransferTx(1, 3, recA, big.NewInt(0), 60_000, []outputAccessTuple{
		{Address: recA},
		{Address: ladderStorageProbeAddr, StorageKeys: []common.Hash{{}}},
	})
	tx, outRaw, err := buildTx(&in, signer, cfg)
	if err != nil {
		t.Fatalf("buildTx(accesslist): %v", err)
	}
	if tx.Type() != types.AccessListTxType {
		t.Fatalf("tx type = %d, want %d (AccessListTx)", tx.Type(), types.AccessListTxType)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if raw[0] != 0x01 {
		t.Fatalf("envelope type byte = 0x%02x, want 0x01", raw[0])
	}
	var out outputAccessListTx
	if err := json.Unmarshal(outRaw, &out); err != nil {
		t.Fatalf("decode outputAccessListTx: %v", err)
	}
	if out.OpType != "accesslist" {
		t.Fatalf("_op_type = %q, want accesslist", out.OpType)
	}
	env, err := hexutil.Decode(out.OpRaw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(env, raw) {
		t.Fatal("_op_raw != tx.MarshalBinary()")
	}
	if out.Sender != addrOfKey(1) {
		t.Fatalf("sender = %s, want key-1 address", out.Sender.Hex())
	}
	if len(out.AccessList) != 2 {
		t.Fatalf("accessList rows = %d, want 2", len(out.AccessList))
	}
	if out.AccessList[0].StorageKeys == nil || len(out.AccessList[0].StorageKeys) != 0 {
		t.Fatalf("nil StorageKeys not normalized to []: %v", out.AccessList[0].StorageKeys)
	}
	if len(out.AccessList[1].StorageKeys) != 1 {
		t.Fatalf("second tuple storageKeys = %d, want 1", len(out.AccessList[1].StorageKeys))
	}
	if got := tx.AccessList(); len(got) != 2 || got[0].Address != recA ||
		got[1].Address != ladderStorageProbeAddr {
		t.Fatalf("signed tx access list mismatch: %+v", got)
	}
	if got := tx.GasPrice(); got == nil || got.Int64() != 2_000_000_000 {
		t.Fatalf("gasPrice = %v, want 2 gwei", got)
	}
}

// TestLadderTxTypeProbesInjected anchors the P2-B ladder injections on the
// scaled explicit 8-fork ladder: every segment probe block carries exactly one
// 2930 (status 1, intrinsic+AL gasUsed) and one legacy (status 1, 21000) tx;
// the 7702 delegation + delegated call appear ONLY in the isthmus/jovian
// segments with chained authority nonces, and the authority account carries
// the 0xef0100||ladderLogsProbeAddr designator from the first delegation on.
func TestLadderTxTypeProbesInjected(t *testing.T) {
	spec, err := parseLadderFlag(ladderScaledExplicit8, 100)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generateLadderChain(spec, 100)
	if err != nil {
		t.Fatalf("generateLadderChain: %v", err)
	}
	blocks := ladderScaledExplicit8Blocks(t)
	if want := []int{5, 17, 29, 42, 54, 67, 79, 92}; !rsEqualInts(blocks, want) {
		t.Fatalf("segment probe blocks changed: want %v, got %v", want, blocks)
	}
	auth := addrOfKey(4)
	wantDesignator := append([]byte{0xef, 0x01, 0x00}, ladderLogsProbeAddr.Bytes()...)
	delegations := 0
	var st outputSignedTx
	for _, b := range blocks {
		blk := out.Blocks[b]
		rs := ladderProbeReceipts(t, blk)

		als := txTypedReceipts(t, blk, "accesslist")
		if len(als) != 1 {
			t.Fatalf("block %d: want exactly 1 accesslist probe, got %d", b, len(als))
		}
		if rs[als[0]].rc.Status != "0x1" {
			t.Fatalf("block %d: accesslist probe status %s, want 0x1", b, rs[als[0]].rc.Status)
		}
		// 21000 intrinsic + 2400 (recA tuple) + 2400 (churn tuple) + 1900 (key).
		if rs[als[0]].rc.GasUsed != "0x6c34" {
			t.Fatalf("block %d: accesslist gasUsed %s, want 0x6c34 (27700)", b, rs[als[0]].rc.GasUsed)
		}
		if env := opRawOf(t, blk, als[0]); env[0] != 0x01 {
			t.Fatalf("block %d: accesslist envelope type byte 0x%02x, want 0x01", b, env[0])
		}

		legs := txTypedReceipts(t, blk, "legacy")
		if len(legs) != 1 {
			t.Fatalf("block %d: want exactly 1 legacy probe, got %d", b, len(legs))
		}
		if rs[legs[0]].rc.Status != "0x1" || rs[legs[0]].rc.GasUsed != "0x5208" {
			t.Fatalf("block %d: legacy probe status/gasUsed %s/%s, want 0x1/0x5208",
				b, rs[legs[0]].rc.Status, rs[legs[0]].rc.GasUsed)
		}
		if env := opRawOf(t, blk, legs[0]); env[0] < 0xc0 {
			t.Fatalf("block %d: legacy envelope must be an RLP list, type byte 0x%02x", b, env[0])
		}

		scs := txTypedReceipts(t, blk, "setcode")
		prague := b >= 79 // isthmus activates at index 75+1-1... see below; anchored by the want-list
		if prague {
			if len(scs) != 1 {
				t.Fatalf("block %d (prague segment): want exactly 1 setcode tx, got %d", b, len(scs))
			}
			delegations++
			// delegated call: to the authority, success, one 3-topic log from it
			var calls []ladderProbeReceipt
			for _, r := range probeReceiptsTo(rs, auth) {
				if r.opType == "eip1559" { // skip the setcode tx itself (to == authority)
					calls = append(calls, r)
				}
			}
			if len(calls) != 1 || calls[0].rc.Status != "0x1" || len(calls[0].rc.Logs) != 1 ||
				calls[0].rc.Logs[0].Address != "0x"+common.Bytes2Hex(auth.Bytes()) {
				t.Fatalf("block %d: delegated call receipt mismatch: %+v", b, calls)
			}
		} else if len(scs) != 0 {
			t.Fatalf("block %d (pre-isthmus): setcode tx must NOT be injected, got %d", b, len(scs))
		}
	}
	if delegations != 2 {
		t.Fatalf("want exactly 2 delegations (isthmus+jovian segments), got %d", delegations)
	}

	// Authority postState shapes: absent before the first delegation block,
	// designator code + chained nonce from each delegation block on, and the
	// candidate bookkeeping keeps it in NON-probe postStates afterwards.
	if _, ok := out.Blocks[29].PostState[auth]; ok {
		t.Fatal("authority must be absent from pre-isthmus postStates")
	}
	acc, ok := out.Blocks[79].PostState[auth]
	if !ok {
		t.Fatal("authority missing from first delegation block postState")
	}
	if acc.Nonce != 1 || !bytes.Equal(acc.Code, wantDesignator) {
		t.Fatalf("authority after 1st delegation: nonce=%d code=%x, want nonce=1 code=%x",
			acc.Nonce, acc.Code, wantDesignator)
	}
	if acc, ok := out.Blocks[92].PostState[auth]; !ok || acc.Nonce != 2 ||
		!bytes.Equal(acc.Code, wantDesignator) {
		t.Fatalf("authority after 2nd delegation: nonce=%d code=%x, want nonce=2 + designator",
			acc.Nonce, acc.Code)
	}
	if _, ok := out.Blocks[80].PostState[auth]; !ok {
		t.Fatal("authority candidate bookkeeping broken: absent from block 80 postState")
	}

	// Auth nonce chaining: the 2nd delegation's authorization nonce must be 1.
	// (Locate the setcode tx by its _op_type: the P2-C probes append more txs
	// after it, so the index is not stable across stages.)
	var sc outputSetCodeTx
	foundSetcode := false
	for _, raw := range out.Blocks[92].Block.Transactions {
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatal(err)
		}
		if st.OpType != "setcode" {
			continue
		}
		if err := json.Unmarshal(raw, &sc); err != nil {
			t.Fatal(err)
		}
		foundSetcode = true
		break
	}
	if !foundSetcode {
		t.Fatal("block 92: setcode tx missing")
	}
	if len(sc.OpAuthorizationList) != 1 || uint64(sc.OpAuthorizationList[0].Nonce) != 1 {
		t.Fatalf("2nd delegation auth nonce = %+v, want [nonce 1]", sc.OpAuthorizationList)
	}
}

// txTypedReceipts returns the indices of the block's txs whose decoded
// _op_type equals opType.
func txTypedReceipts(t *testing.T, blk chainBlockOutput, opType string) []int {
	t.Helper()
	var out []int
	for i, raw := range blk.Block.Transactions {
		var st outputSignedTx
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatalf("decode tx %d: %v", i, err)
		}
		if st.OpType == opType {
			out = append(out, i)
		}
	}
	return out
}

func opRawOf(t *testing.T, blk chainBlockOutput, i int) []byte {
	t.Helper()
	var st outputSignedTx
	if err := json.Unmarshal(blk.Block.Transactions[i], &st); err != nil {
		t.Fatal(err)
	}
	env, err := hexutil.Decode(st.OpRaw)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func rsEqualInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
