package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

const ladderFull8 = "0:regolith,125:canyon,250:ecotone,375:fjord,500:granite," +
	"625:holocene,750:isthmus,875:jovian"

func TestParseLadderFlagFull8(t *testing.T) {
	spec, err := parseLadderFlag(ladderFull8, 1000)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(spec.Activations) != 8 {
		t.Fatalf("want 8 activations, got %d", len(spec.Activations))
	}
	first, last := spec.Activations[0], spec.Activations[7]
	if first.Fork != "regolith" || first.Block != 0 {
		t.Fatalf("first must be regolith@0, got %s@%d", first.Fork, first.Block)
	}
	if last.Fork != "jovian" || last.Block != 875 {
		t.Fatalf("last must be jovian@875, got %s@%d", last.Fork, last.Block)
	}
	// 激活时间 = ladderGenesisTime(1000) + ladderBlockInterval*block
	//（chain_makers.makeHeader 固定 +10s）
	if want := ladderGenesisTime + ladderBlockInterval*125; spec.Activations[1].Timestamp != want {
		t.Fatalf("canyon timestamp: want %d, got %d", want, spec.Activations[1].Timestamp)
	}
}

func TestParseLadderFlagRejections(t *testing.T) {
	cases := []struct {
		name    string
		ladder  string
		blocks  int
		wantErr string
	}{
		{"karst excluded", "0:regolith,100:karst", 200, "karst is excluded"},
		{"not increasing", "0:regolith,50:canyon,50:ecotone", 200, "not > previous"},
		{"block beyond chain", "0:regolith,500:jovian", 400, "would never activate"},
		{"unknown fork", "0:regolith,50:osaka", 200, `unknown fork "osaka"`},
		{"first not regolith@0", "10:regolith,50:canyon", 200, "first activation must be 0:regolith"},
		{"single activation", "0:regolith", 200, "needs >= 2 activations"},
		{"duplicate fork", "0:regolith,50:canyon,100:canyon", 200,
			`fork "canyon" out of canonical order`},
		{"out of order fork", "0:regolith,100:ecotone,200:canyon", 200,
			`fork "canyon" out of canonical order`},
		{"trailing comma", "0:regolith,50:canyon,", 200, "want <block>:<fork>"},
		{"empty input", "", 200, "needs >= 2 activations"},
		{"totalBlocks below floor", "0:regolith,50:canyon", 1, "totalBlocks >= 2"},
	}
	for _, c := range cases {
		_, err := parseLadderFlag(c.ladder, c.blocks)
		if err == nil {
			t.Fatalf("%s: want error, got nil", c.name)
		}
		if !strings.Contains(err.Error(), c.wantErr) {
			t.Fatalf("%s: error %q does not contain %q", c.name, err, c.wantErr)
		}
	}
}

// TestLadderSmoke3BlocksCrossingCanyon：generateLadderChain 的 3 块 smoke，
// 跨 canyon 边界（D1b）。fork 切换必须全自动：cfg 按 ladderSpec 激活，块时间
// 由 chain_makers 每块 parent+10s 推进，op-geth 的 cfg.Rules 按块时间换挡。
// 块时间线（genesis t=1000）：block1 t=1010，block2 t=1020，block3 t=1030；
// "2:canyon" => CanyonTime=1020，激活块本身即新 fork。
func TestLadderSmoke3BlocksCrossingCanyon(t *testing.T) {
	spec, err := parseLadderFlag("0:regolith,2:canyon", 3)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generateLadderChain(spec, 3)
	if err != nil {
		t.Fatalf("generateLadderChain: %v", err)
	}
	if len(out.Blocks) != 3 {
		t.Fatalf("want 3 blocks, got %d", len(out.Blocks))
	}
	// _info.hardfork 按块时间 fork：block1(t=1010) regolith，
	// block2(t=1020) canyon —— fork 切换必须自动发生。
	if got := out.Blocks[0].Info.Hardfork; got != "regolith" {
		t.Fatalf("block 1 hardfork: want regolith, got %q", got)
	}
	if got := out.Blocks[1].Info.Hardfork; got != "canyon" {
		t.Fatalf("block 2 hardfork: want canyon (fork switch must be automatic), got %q", got)
	}
	if got := out.Blocks[2].Info.Hardfork; got != "canyon" {
		t.Fatalf("block 3 hardfork: want canyon, got %q", got)
	}
	// chain-vector pre convention: ONLY block 0 carries pre; replayChainVector
	// reloads state only when `pre` is present, so a regression to nil (or to
	// non-nil on i>0) would fail silently downstream.
	if out.Blocks[0].Pre == nil {
		t.Fatal("block 0 must carry pre (chain replay origin)")
	}
	if out.Blocks[1].Pre != nil || out.Blocks[2].Pre != nil {
		t.Fatal("blocks 1/2 must NOT carry pre (replayer inherits chain state)")
	}
	// currentTimestamp must really advance parent+10s (genesis t=1000 ->
	// 0x3f2/0x3fc/0x406 = 1010/1020/1030) -- the enabler for automatic fork
	// switching; a frozen timestamp would keep every block on the genesis fork.
	for i, want := range []string{"0x3f2", "0x3fc", "0x406"} {
		if got := out.Blocks[i].Env.CurrentTimestamp; got != want {
			t.Fatalf("block %d currentTimestamp: want %s, got %s", i, want, got)
		}
	}
	// block 0's L1-attributes deposit must have succeeded (0x1) -- a failed
	// attributes deposit would silently leave the L1Block predeploy stale.
	if len(out.Blocks[0].OpExpected.Receipts) == 0 {
		t.Fatal("block 0 has no receipts")
	}
	if got := out.Blocks[0].OpExpected.Receipts[0].Status; got != "0x1" {
		t.Fatalf("block 0 attributes deposit receipt status: want 0x1, got %q", got)
	}
	if len(out.Blocks[2].OpExpected.Receipts) == 0 {
		t.Fatal("block 3 has no receipts")
	}
}

// TestLadderCrossesLayoutBoundary: A2. The old I1 guard
// (forkLayout(first) != forkLayout(top) -> error) is GONE; a chain crossing the
// Bedrock/Ecotone L1Block layout boundary now generates, because
//   - the genesis L1Block code is A1's combined four-way runtime
//     (l1BlockCodeForLadder), and
//   - genesis is seeded with the genesis fork's Bedrock layout (slots 1/5/6),
//     with each later family's slots introduced by its first real deposit.
//
// The genesis code is anchored externally via block 0's emitted `pre` (not by
// trusting the in-function readiness assert).
func TestLadderCrossesLayoutBoundary(t *testing.T) {
	spec, err := parseLadderFlag("0:regolith,2:ecotone", 6)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generateLadderChain(spec, 6)
	if err != nil {
		t.Fatalf("cross-family ladder must generate after A2, got: %v", err)
	}
	if out.Blocks[0].Pre == nil {
		t.Fatal("block 0 must carry pre")
	}
	acc, ok := (*out.Blocks[0].Pre)[l1BlockAddr]
	if !ok {
		t.Fatalf("genesis L1Block %s missing from block 0 pre", l1BlockAddr)
	}
	if !bytes.Equal(acc.Code, l1BlockCodeForLadder()) {
		t.Fatalf("genesis L1Block code is not the combined four-way runtime (len %d)", len(acc.Code))
	}
	// Genesis seeds are the Bedrock layout: Ecotone-family slots must be absent
	// (they are introduced by the deposit of the first post-Ecotone block).
	for _, slot := range []common.Hash{types.L1FeeScalarsSlot, types.L1BlobBaseFeeSlot, types.OperatorFeeParamsSlot} {
		if _, ok := acc.Storage[slot]; ok {
			t.Fatalf("genesis must not pre-seed Ecotone-family slot %s", slot.Hex())
		}
	}
	// Block 1 (index 0, t=1010) is regolith and still uses the Bedrock form;
	// the fork switch must be automatic.
	if got := out.Blocks[0].Info.Hardfork; got != "regolith" {
		t.Fatalf("block 0 hardfork: want regolith, got %q", got)
	}
}

func TestParseLadderFlagNormalization(t *testing.T) {
	spec, err := parseLadderFlag(" 0:REGOLITH ,125:Canyon ", 1000)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(spec.Activations) != 2 {
		t.Fatalf("want 2 activations, got %d", len(spec.Activations))
	}
	if first := spec.Activations[0]; first.Fork != "regolith" || first.Block != 0 {
		t.Fatalf("first must be regolith@0, got %s@%d", first.Fork, first.Block)
	}
	if last := spec.Activations[1]; last.Fork != "canyon" || last.Block != 125 {
		t.Fatalf("last must be canyon@125, got %s@%d", last.Fork, last.Block)
	}
	if want := ladderGenesisTime + ladderBlockInterval*125; spec.Activations[1].Timestamp != want {
		t.Fatalf("canyon timestamp: want %d, got %d", want, spec.Activations[1].Timestamp)
	}
}

// TestRecipeForTable pins the per-fork recipe rules (design v2 §3.3/§6):
// deposit 1 everywhere; withdrawal from Canyon on; create iff blockIdx>0 &&
// blockIdx%100==0; granite/holocene carry the bn256-pairing probe,
// isthmus/jovian the P256VERIFY probe, all other forks none.
func TestRecipeForTable(t *testing.T) {
	// Independent floor for the pairing probe gas: the probe must clear
	// intrinsic(23304 for a 192B input) + 45000 + 34000/pair to actually run
	// the pairing. Anchored to literals so reverting the constant regresses.
	if ladderBn256PairingProbeGas <= 102_304 {
		t.Fatalf("pairing probe gas %d cannot execute 1 pair", ladderBn256PairingProbeGas)
	}
	pairingAddr := common.BytesToAddress(addrBytes(preBn256Pairing))
	p256Addr := common.BytesToAddress(addrBytes(preP256Verify))
	for _, fork := range ladderForks {
		r := recipeFor(fork, 0, false)
		if r.deposit != 1 {
			t.Fatalf("%s: deposit: want 1, got %d", fork, r.deposit)
		}
		wantWithdrawal := 1
		if fork == "regolith" {
			wantWithdrawal = 0
		}
		if r.withdrawal != wantWithdrawal {
			t.Fatalf("%s: withdrawal: want %d, got %d", fork, wantWithdrawal, r.withdrawal)
		}
		if r.create {
			t.Fatalf("%s: block 0 must not set create", fork)
		}
		if !recipeFor(fork, 100, false).create {
			t.Fatalf("%s: block 100 must set create", fork)
		}
		if recipeFor(fork, 101, false).create {
			t.Fatalf("%s: block 101 must not set create", fork)
		}

		switch fork {
		case "granite", "holocene":
			if len(r.precompiles) != 1 {
				t.Fatalf("%s: want 1 precompile probe, got %d", fork, len(r.precompiles))
			}
			p := r.precompiles[0]
			if p.addr != pairingAddr {
				t.Fatalf("%s: probe addr want %s, got %s", fork, pairingAddr, p.addr)
			}
			if !bytes.Equal(p.input, repeatedBn256Pair(1)) {
				t.Fatalf("%s: probe input is not repeatedBn256Pair(1)", fork)
			}
			if p.gas != ladderBn256PairingProbeGas {
				t.Fatalf("%s: probe gas want %d, got %d", fork, ladderBn256PairingProbeGas, p.gas)
			}
		case "isthmus", "jovian":
			if len(r.precompiles) != 1 {
				t.Fatalf("%s: want 1 precompile probe, got %d", fork, len(r.precompiles))
			}
			p := r.precompiles[0]
			if p.addr != p256Addr {
				t.Fatalf("%s: probe addr want %s, got %s", fork, p256Addr, p.addr)
			}
			if !bytes.Equal(p.input, validP256Sig()) {
				t.Fatalf("%s: probe input is not validP256Sig()", fork)
			}
			if p.gas != ladderP256VerifyProbeGas {
				t.Fatalf("%s: probe gas want %d, got %d", fork, ladderP256VerifyProbeGas, p.gas)
			}
		default:
			if len(r.precompiles) != 0 {
				t.Fatalf("%s: want no precompile probes, got %d", fork, len(r.precompiles))
			}
		}
	}
}

// TestRecipeForJovianActivationIsDepositsOnly pins the hard rule from
// op-geth core/types/rollup_cost.go:571-576: the Jovian activation block must
// carry deposits only. create/precompiles/withdrawal must ALL be suppressed,
// even at a block index that would otherwise trigger create.
func TestRecipeForJovianActivationIsDepositsOnly(t *testing.T) {
	want := txRecipe{deposit: 1}
	for _, idx := range []int{0, 1, 100, 750} {
		got := recipeFor("jovian", idx, true)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("jovian activation recipe at block %d: want %+v, got %+v", idx, want, got)
		}
	}
	// Non-activation Jovian blocks keep the normal recipe (deposit +
	// withdrawal + P256 probe).
	got := recipeFor("jovian", 0, false)
	if got.deposit != 1 || got.withdrawal != 1 || len(got.precompiles) != 1 {
		t.Fatalf("jovian non-activation recipe: unexpected %+v", got)
	}
}

// TestWithdrawalSlotsDeterministic pins withdrawalSlots' determinism and its
// versionedNonce/slot math against cases.go message_passer_withdraw's worked
// example (first call -> versionedNonce 1<<240, msgNonce slot 1 = 1).
func TestWithdrawalSlotsDeterministic(t *testing.T) {
	a1, a2 := withdrawalSlots(1)
	b1, b2 := withdrawalSlots(1)
	if a1 != b1 || a2 != b2 {
		t.Fatalf("withdrawalSlots(1) not deterministic: (%s,%s) vs (%s,%s)", a1, a2, b1, b2)
	}
	c1, c2 := withdrawalSlots(2)
	slot1 := common.BigToHash(big.NewInt(1))
	if a1 != c1 || a1 != slot1 {
		t.Fatalf("msgNonce declaration slot must be slot 1 for every k: k=1 %s, k=2 %s", a1, c1)
	}
	if a2 == c2 {
		t.Fatalf("k=2 sentMessages slot == k=1 (%s)", a2)
	}

	// Independent recomputation, verbatim from cases.go's construction.
	sentFor := func(k int) common.Hash {
		versionedNonce := new(big.Int).Or(
			new(big.Int).Lsh(big.NewInt(1), 240), big.NewInt(int64(k-1)))
		wh := crypto.Keccak256(abiEncodeWithdrawal(versionedNonce, addrOfKey(1),
			ladderWithdrawalTarget, 0, ladderWithdrawalGasLimit, ladderWithdrawalData))
		return common.BytesToHash(crypto.Keccak256(wh, make([]byte, 32)))
	}
	if a2 != sentFor(1) {
		t.Fatalf("k=1 sentMessages slot: want %s, got %s", sentFor(1), a2)
	}
	if c2 != sentFor(2) {
		t.Fatalf("k=2 sentMessages slot: want %s, got %s", sentFor(2), c2)
	}
}

// TestLadderCanyonWithdrawalSmoke: "0:regolith,2:canyon" n=4. CanyonTime=1020
// is block index 1 (block i has time 1000+10*(i+1)), so blocks 1..3 are the
// canyon window and each carries a withdrawal. Generation succeeding proves
// the slot declarations are complete (emitPostState hard-fails on any
// undeclared written slot).
func TestLadderCanyonWithdrawalSmoke(t *testing.T) {
	spec, err := parseLadderFlag("0:regolith,2:canyon", 4)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generateLadderChain(spec, 4)
	if err != nil {
		t.Fatalf("generateLadderChain: %v", err)
	}
	if len(out.Blocks) != 4 {
		t.Fatalf("want 4 blocks, got %d", len(out.Blocks))
	}
	isWithdrawal := func(raw json.RawMessage) bool {
		var st outputSignedTx
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatalf("decode block tx: %v", err)
		}
		return st.To != nil && *st.To == messagePasserAddr
	}
	for _, idx := range []int{1, 2, 3} {
		found := false
		for _, raw := range out.Blocks[idx].Block.Transactions {
			if isWithdrawal(raw) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("block %d (canyon) carries no withdrawal tx", idx)
		}
	}
	for _, raw := range out.Blocks[0].Block.Transactions {
		if isWithdrawal(raw) {
			t.Fatal("regolith block 0 must not carry a withdrawal")
		}
	}

	// Three withdrawals (blocks 1,2,3) -> final postState msgNonce (slot 1) = 3.
	acc, ok := out.Blocks[3].PostState[messagePasserAddr]
	if !ok {
		t.Fatal("MessagePasser missing from block 3 postState")
	}
	slot1 := common.BigToHash(big.NewInt(1))
	if got := acc.Storage[slot1]; got != common.BigToHash(big.NewInt(3)) {
		t.Fatalf("msgNonce slot 1: want 3, got %s", got.Hex())
	}
	// Every withdrawal's dynamic sentMessages[withdrawalHash] slot must also be
	// present (a task requirement): k=1..3 -> slot1 + 3 sent slots = 4.
	for k := 1; k <= 3; k++ {
		_, slotB := withdrawalSlots(k)
		if got := acc.Storage[slotB]; got != common.BigToHash(big.NewInt(1)) {
			t.Fatalf("sentMessages slot for withdrawal %d: want 0x1, got %s", k, got.Hex())
		}
	}
	if len(acc.Storage) != 4 {
		t.Fatalf("MessagePasser storage: want 4 slots (msgNonce + 3 sentMessages), got %d", len(acc.Storage))
	}
}

// TestLadderCreateProbeSmoke exercises the create:true recipe arm: with a
// 101-block canyon-family ladder, block index 100 (100>0 && %100==0) carries
// the EIP-1559 CREATE probe. The create tx's nonce re-derives the created
// address, and the postState must carry that account with its init-phase
// slot 0 = 1 -- without this the test would stay green even if the create
// silently reverted/OOG'd (the ExtraStorage declaration for the wrong address
// is inert). (The create rule is fork-independent, so a canyon-family ladder
// exercises it directly.)
func TestLadderCreateProbeSmoke(t *testing.T) {
	const n = 101
	spec, err := parseLadderFlag("0:regolith,2:canyon", n)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generateLadderChain(spec, n)
	if err != nil {
		t.Fatalf("generateLadderChain: %v", err)
	}
	found := false
	var createNonce uint64
	for _, raw := range out.Blocks[100].Block.Transactions {
		var st outputSignedTx
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatalf("decode block 100 tx: %v", err)
		}
		if st.OpType == "eip1559" && st.To == nil {
			found = true
			createNonce = uint64(st.Nonce)
			break
		}
	}
	if !found {
		t.Fatal("block 100 (blockIdx%100==0) carries no create tx (eip1559 to == null)")
	}
	// Re-derive the created address from the create tx's nonce and assert the
	// account + its init-phase slot 0 actually landed: a silently-reverted/OOG
	// create would still pass the tx-scan above (the ExtraStorage declaration
	// for a wrong address is inert), so this is the real anchor.
	created := crypto.CreateAddress(addrOfKey(1), createNonce)
	acc, ok := out.Blocks[100].PostState[created]
	if !ok {
		t.Fatalf("created account %s missing from block 100 postState", created.Hex())
	}
	if got := acc.Storage[common.Hash{}]; got != common.BigToHash(big.NewInt(1)) {
		t.Fatalf("created account slot0: want 0x1, got %s", got.Hex())
	}
	// No create probe before block 100.
	for _, raw := range out.Blocks[99].Block.Transactions {
		var st outputSignedTx
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatalf("decode block 99 tx: %v", err)
		}
		if st.OpType == "eip1559" && st.To == nil {
			t.Fatal("block 99 must not carry a create tx")
		}
	}
}

func TestLadderPostStateSampling(t *testing.T) {
	// 采样点（设计 v2 §3.4）：首块、末块、激活块±1、每 100 块。
	// A2 起 ladder 可跨 L1Block 布局族；此处用 canyon-only spec 保持采样集
	// 可精确枚举。
	//
	// ladderActivation.Block 是 1-based 块号（canyon@50 => Timestamp=1500），
	// block i 的时间是 genesis+10*(i+1)，故激活块 = 索引 49：48 是激活前一块、
	// 49 是激活块、50 是激活后一块。采样集应为 {0,48,49,50,100,119}。
	spec, err := parseLadderFlag("0:regolith,50:canyon", 120)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generateLadderChainSampled(spec, 120)
	if err != nil {
		t.Fatalf("generateLadderChainSampled: %v", err)
	}
	if len(out.Blocks) != 120 {
		t.Fatalf("want 120 blocks, got %d", len(out.Blocks))
	}
	sampledSet := map[int]bool{}
	for _, s := range out.SampledBlocks {
		sampledSet[s] = true
	}
	// 精确采样集（升序）：0/119 首末，48/49/50 激活窗口（48=激活前一块、
	// 49=激活块、50=激活后一块），100 每百。精确相等才能同时抓出窗口左移
	// （漏 48）与右移（多 51）。
	wantSampled := []int{0, 48, 49, 50, 100, 119}
	if !reflect.DeepEqual(out.SampledBlocks, wantSampled) {
		t.Fatalf("sampled blocks: want %v, got %v", wantSampled, out.SampledBlocks)
	}
	if sampledSet[1] {
		t.Fatalf("block 1 must NOT be sampled (not a boundary, not %%100)")
	}
	// 导出层：未采样块 postState 为空，采样块非空
	for i := range out.Blocks {
		empty := len(out.Blocks[i].PostState) == 0
		if empty == sampledSet[i] {
			t.Fatalf("block %d: sampled=%v but postState-empty=%v", i, sampledSet[i], empty)
		}
	}

	// 内部全量链未被采样破坏：全量版每块 postState 非空；逐块除 PostState 外
	// 与采样版 DeepEqual；每个采样块的 postState 与全量版对应块相等。这锚定
	// 了块间 Pre 链（采样在 generateLadderChain 返回后才置 nil）。
	full, err := generateLadderChain(spec, 120)
	if err != nil {
		t.Fatalf("generateLadderChain: %v", err)
	}
	if len(full.Blocks) != 120 {
		t.Fatalf("full: want 120 blocks, got %d", len(full.Blocks))
	}
	for i := range full.Blocks {
		if len(full.Blocks[i].PostState) == 0 {
			t.Fatalf("full chain block %d has empty postState", i)
		}
	}
	for i := range out.Blocks {
		if sampledSet[i] && !reflect.DeepEqual(out.Blocks[i].PostState, full.Blocks[i].PostState) {
			t.Fatalf("sampled block %d postState differs from full chain", i)
		}
		sb, fb := out.Blocks[i], full.Blocks[i]
		sb.PostState = fb.PostState // the ONLY intended difference
		if !reflect.DeepEqual(sb, fb) {
			t.Fatalf("block %d differs from full chain outside postState", i)
		}
	}
}

func TestLadderWithdrawalReceiptCarriesLogs(t *testing.T) {
	// canyon 段的 withdrawal 会产生 MessagePassed 事件 → 回执 logs 必须被导出，
	// 且 address/topics/data 必须是全小写 hex（Go 的 Address.Hex() 是 EIP-55
	// 混合大小写，FISCO 侧 bcos::toHex 全小写——不归一化会导致对拍必红）。
	spec, err := parseLadderFlag("0:regolith,2:canyon", 4)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generateLadderChain(spec, 4)
	if err != nil {
		t.Fatalf("generateLadderChain: %v", err)
	}
	wantAddr := "0x" + common.Bytes2Hex(messagePasserAddr.Bytes())
	// MessagePassed(address,address,uint256,uint256,bytes) 事件签名 hash，
	// 全链常量（实测 receipt 值，见 report）。
	const messagePassedTopic0 = "0x02a52367d10742d8032712c1bb8e0144ff1ec5ffda1ed7d70bb05a2744955054"

	// 格式校验在地址过滤之前做，否则 "address == wantAddr" 蕴含
	// "address == ToLower(address)"，lowercase 断言恒假（死代码）。
	for _, blk := range out.Blocks {
		for ri, r := range blk.OpExpected.Receipts {
			if r.LogsCount != len(r.Logs) {
				t.Fatalf("receipt %d: logsCount %d != len(logs) %d", ri, r.LogsCount, len(r.Logs))
			}
			for _, l := range r.Logs {
				if !strings.HasPrefix(l.Address, "0x") || len(l.Address) != 42 ||
					l.Address != strings.ToLower(l.Address) {
					t.Fatalf("bad log address hex: %s", l.Address)
				}
				for _, topic := range l.Topics {
					if !strings.HasPrefix(topic, "0x") || len(topic) != 66 ||
						topic != strings.ToLower(topic) {
						t.Fatalf("bad topic hex: %s", topic)
					}
				}
				if !strings.HasPrefix(l.Data, "0x") || l.Data != strings.ToLower(l.Data) {
					t.Fatalf("bad data hex: %s", l.Data)
				}
			}
		}
	}

	// Canyon 块 1..3 的 withdrawal 是块内最后一笔（idx 3），其回执恰好携带
	// 一条 MessagePassed log：地址 = L2ToL1MessagePasser，4 个 topic，
	// topics[0] = 事件签名。这是对 "logs 真的被导出且内容正确" 的硬锚。
	for blk := 1; blk <= 3; blk++ {
		rs := out.Blocks[blk].OpExpected.Receipts
		if len(rs) != 4 {
			t.Fatalf("block %d: want 4 receipts (attrs+deposit+transfer+withdrawal), got %d", blk, len(rs))
		}
		r := rs[3]
		if r.LogsCount != 1 || len(r.Logs) != 1 {
			t.Fatalf("block %d withdrawal receipt: want exactly 1 log, got logsCount=%d len=%d",
				blk, r.LogsCount, len(r.Logs))
		}
		l := r.Logs[0]
		if l.Address != wantAddr {
			t.Fatalf("block %d log address: want %s, got %s", blk, wantAddr, l.Address)
		}
		if len(l.Topics) != 4 {
			t.Fatalf("block %d MessagePassed topics: want 4, got %d", blk, len(l.Topics))
		}
		if l.Topics[0] != messagePassedTopic0 {
			t.Fatalf("block %d topic0: want %s, got %s", blk, messagePassedTopic0, l.Topics[0])
		}
	}
}

func TestRunLadderModeWritesVectorAndSums(t *testing.T) {
	dir := t.TempDir()
	if err := runLadderMode(dir, "0:regolith,2:canyon", 4, "testcommit", "boundary"); err != nil {
		t.Fatalf("runLadderMode: %v", err)
	}
	vec := filepath.Join(dir, "ladder_4.json")
	if _, err := os.Stat(vec); err != nil {
		t.Fatalf("vector missing: %v", err)
	}
	sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		t.Fatalf("SHA256SUMS missing: %v", err)
	}
	data, _ := os.ReadFile(vec)
	want := fmt.Sprintf("%x  ladder_4.json\n", sha256.Sum256(data))
	if string(sums) != want {
		t.Fatalf("SHA256SUMS mismatch:\n got %q\nwant %q", sums, want)
	}
	// rejection paths
	if err := runLadderMode(dir, "0:regolith,2:karst", 4, "c", "boundary"); err == nil {
		t.Fatal("karst ladder must be rejected")
	}
	if err := runLadderMode("", "0:regolith,2:canyon", 4, "c", "boundary"); err == nil {
		t.Fatal("missing out-dir must be rejected")
	}
}

// TestRunLadderModePostStateModes 验证 D1h 的 --poststate 导出粒度开关：
//   - full：每块都带 postState，且不写 sampledBlocks（缺省 = 全量采样的旧契约）；
//   - boundary：保持采样（sampledBlocks 键存在，未采样块无 postState）；
//   - 非法值报错并点名 --poststate。
func TestRunLadderModePostStateModes(t *testing.T) {
	type ladderDoc struct {
		Blocks        []map[string]json.RawMessage `json:"blocks"`
		SampledBlocks *[]int                       `json:"sampledBlocks"`
	}
	load := func(t *testing.T, dir string) ladderDoc {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, "ladder_12.json"))
		if err != nil {
			t.Fatalf("read ladder_12.json: %v", err)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(data, &top); err != nil {
			t.Fatalf("unmarshal outer: %v", err)
		}
		var doc ladderDoc
		if err := json.Unmarshal(top["ladder_12"], &doc); err != nil {
			t.Fatalf("unmarshal ladder doc: %v", err)
		}
		return doc
	}
	// canyon@5（1-based 块号）=> 0-based 索引 4；总 12 块 => 采样集必为真子集。
	const spec, blocks = "0:regolith,5:canyon", 12

	fullDir := t.TempDir()
	if err := runLadderMode(fullDir, spec, blocks, "testcommit", "full"); err != nil {
		t.Fatalf("full mode: %v", err)
	}
	full := load(t, fullDir)
	if full.SampledBlocks != nil {
		t.Fatalf("full mode must NOT emit sampledBlocks, got %v", *full.SampledBlocks)
	}
	if len(full.Blocks) != blocks {
		t.Fatalf("full mode: got %d blocks, want %d", len(full.Blocks), blocks)
	}
	for i, b := range full.Blocks {
		if _, ok := b["postState"]; !ok {
			t.Fatalf("full mode: block %d missing postState", i)
		}
	}

	boundaryDir := t.TempDir()
	if err := runLadderMode(boundaryDir, spec, blocks, "testcommit", "boundary"); err != nil {
		t.Fatalf("boundary mode: %v", err)
	}
	boundary := load(t, boundaryDir)
	if boundary.SampledBlocks == nil {
		t.Fatal("boundary mode must emit sampledBlocks")
	}
	sampled := make(map[int]bool, len(*boundary.SampledBlocks))
	for _, i := range *boundary.SampledBlocks {
		sampled[i] = true
	}
	if len(sampled) == blocks {
		t.Fatalf("boundary mode unexpectedly sampled every block (%v); test cannot prove a full/boundary difference", *boundary.SampledBlocks)
	}
	var unsampledWithPostState int
	for i, b := range boundary.Blocks {
		_, has := b["postState"]
		if sampled[i] != has {
			t.Fatalf("boundary block %d: sampled=%v but postState present=%v", i, sampled[i], has)
		}
		if !sampled[i] {
			unsampledWithPostState++
		}
	}
	if unsampledWithPostState == 0 {
		t.Fatal("test spec produced no unsampled block; full/boundary distinction is vacuous")
	}

	// 非法值必须报错并点名 flag。
	err := runLadderMode(t.TempDir(), spec, blocks, "c", "every")
	if err == nil {
		t.Fatal("invalid --poststate value must be rejected")
	}
	if !strings.Contains(err.Error(), "--poststate") || !strings.Contains(err.Error(), "every") {
		t.Fatalf("invalid --poststate error must name the flag and value, got %q", err)
	}
}

// TestPostStateDefaultIsFull 锚定 --poststate 的注册默认值是 full：全量比对
// （每块都带 postState、不写 sampledBlocks）是默认，boundary 采样是 opt-in。
// 用 flag.Lookup 读注册默认值（而非直接读变量），再用该默认值跑一遍 CLI 入口
// 并检查产物形状——若未来有人把默认悄悄改回 boundary，此测试必红。
func TestPostStateDefaultIsFull(t *testing.T) {
	f := flag.Lookup("poststate")
	if f == nil {
		t.Fatal(`flag "poststate" is not registered`)
	}
	if defaultPostStateMode != "full" {
		t.Fatalf("defaultPostStateMode: want %q, got %q", "full", defaultPostStateMode)
	}
	if f.DefValue != "full" {
		t.Fatalf("--poststate default: want %q, got %q (boundary sampling must be opt-in)", "full", f.DefValue)
	}

	// Mirror regen.sh's invocation: no explicit --poststate, so the CLI would
	// pass the registered default through to runLadderMode.
	dir := t.TempDir()
	const spec, blocks = "0:regolith,5:canyon", 12
	if err := runLadderMode(dir, spec, blocks, "testcommit", f.DefValue); err != nil {
		t.Fatalf("runLadderMode with default %q: %v", f.DefValue, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "ladder_12.json"))
	if err != nil {
		t.Fatalf("read ladder_12.json: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("unmarshal outer: %v", err)
	}
	var doc struct {
		Blocks        []map[string]json.RawMessage `json:"blocks"`
		SampledBlocks *[]int                       `json:"sampledBlocks"`
	}
	if err := json.Unmarshal(top["ladder_12"], &doc); err != nil {
		t.Fatalf("unmarshal ladder doc: %v", err)
	}
	if doc.SampledBlocks != nil {
		t.Fatalf("default run must NOT emit sampledBlocks, got %v", *doc.SampledBlocks)
	}
	if len(doc.Blocks) != blocks {
		t.Fatalf("default run: got %d blocks, want %d", len(doc.Blocks), blocks)
	}
	for i, b := range doc.Blocks {
		if _, ok := b["postState"]; !ok {
			t.Fatalf("default run: block %d missing postState (only boundary mode samples)", i)
		}
	}
}

// ---------------------------------------------------------------------
// A2 (stage 2/3): cross-family ladder prerequisites.
// ---------------------------------------------------------------------

// ladderBlockAttrsData returns block b's attributes deposit calldata (tx 0).
func ladderBlockAttrsData(t *testing.T, b chainBlockOutput) []byte {
	t.Helper()
	if len(b.Block.Transactions) == 0 {
		t.Fatalf("block has no transactions")
	}
	var st outputSignedTx
	if err := json.Unmarshal(b.Block.Transactions[0], &st); err != nil {
		t.Fatalf("decode attributes tx: %v", err)
	}
	return st.Data
}

// TestL1BlockGenesisSeeds pins the A2 seeding adjudication: a ladder's genesis
// seeds are the GENESIS fork's layout (regolith -> Bedrock 1/5/6), NOT the
// union of every activated layout.
//
// The union was the brief's first proposal, and it does fix the observed block-0
// failure, but it is unusable: pre-seeding the Ecotone slots makes op-geth's
// state-based NewL1CostFunc pick the Ecotone cost function while
// deriveOPStackFields extracts Bedrock gasParams from the still-Bedrock
// activation-block calldata; the two disagree and crossCheckVaults rejects the
// vector ("l1 fee cross-check: vault delta ... != sum of per-tx L1 fees ...").
// The lower half of this test documents that the rejected union really would
// have added those slots (so the premise is not vacuous).
func TestL1BlockGenesisSeeds(t *testing.T) {
	fp := defaultFeeParams()
	forks := []string{"regolith", "canyon", "ecotone", "isthmus", "jovian"}
	got := fp.l1BlockGenesisSeeds(forks)
	want := fp.l1BlockStorage("regolith")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("genesis seeds: want the genesis fork's (Bedrock) layout %v, got %v", want, got)
	}
	for _, slot := range []common.Hash{types.L1FeeScalarsSlot, types.L1BlobBaseFeeSlot, types.OperatorFeeParamsSlot} {
		if _, ok := got[slot]; ok {
			t.Fatalf("genesis seeds must NOT contain the later-family slot %s", slot.Hex())
		}
	}

	// Rejected alternative: the literal union (every activated layout, last
	// writer wins) WOULD add 3/7/8 -- so the exclusion above is a real decision.
	// (A non-zero operator fee makes Isthmus's slot8 non-zero, since the union's
	// slot8 entry is otherwise absent when every operator-fee field is zero.)
	unionFP := defaultFeeParams()
	unionFP.opFeeScalar = 5000
	union := map[common.Hash]common.Hash{}
	for _, fork := range forks {
		for k, v := range unionFP.l1BlockStorage(fork) {
			union[k] = v
		}
	}
	for _, slot := range []common.Hash{types.L1FeeScalarsSlot, types.L1BlobBaseFeeSlot, types.OperatorFeeParamsSlot} {
		if _, ok := union[slot]; !ok {
			t.Fatalf("test premise broken: the rejected union should contain slot %s", slot.Hex())
		}
	}
}

// TestL1BlockWrittenSlots pins the per-layout slot sets the ladder declares as
// extra_storage so emitPostState accepts the transition blocks' newly-written
// slots.
func TestL1BlockWrittenSlots(t *testing.T) {
	bedrock := []common.Hash{types.L1BaseFeeSlot, types.OverheadSlot, types.ScalarSlot}
	ecotone := []common.Hash{types.L1BaseFeeSlot, types.L1FeeScalarsSlot, types.L1BlobBaseFeeSlot}
	isthmus := []common.Hash{types.L1BaseFeeSlot, types.L1FeeScalarsSlot, types.L1BlobBaseFeeSlot, types.OperatorFeeParamsSlot}
	want := map[string][]common.Hash{
		"regolith": bedrock, "canyon": bedrock,
		"ecotone": ecotone, "fjord": ecotone, "granite": ecotone, "holocene": ecotone,
		"isthmus": isthmus, "jovian": isthmus,
	}
	for fork, exp := range want {
		got := l1BlockWrittenSlots(fork)
		if !reflect.DeepEqual(got, exp) {
			t.Fatalf("%s written slots: want %v, got %v", fork, exp, got)
		}
	}
}

// TestAssertL1BlockConsistencyAtExplicitBlockTime anchors Task 3: the wrapper
// keeps the genesis+10 default (so every pre-existing call site is unchanged),
// while assertL1BlockConsistencyAt judges the block by the time it is handed.
// knobs.Timestamp stays the CHAIN GENESIS field.
func TestAssertL1BlockConsistencyAtExplicitBlockTime(t *testing.T) {
	// regolith genesis, Ecotone activating at t=1040.
	cfg, err := buildChainConfigSpec(chainConfigSpec{
		base: "regolith", activations: map[string]uint64{"ecotone": 1040},
	})
	if err != nil {
		t.Fatal(err)
	}
	fp := defaultFeeParams()
	attr := fp.attributesTx("explicit-time", "ecotone")
	in := &inputCase{
		Genesis:      genesisKnobs{Timestamp: math.HexOrDecimal64(1000)},
		Pre:          types.GenesisAlloc{l1BlockAddr: {Storage: fp.l1BlockStorage("ecotone")}},
		Transactions: []inputTx{attr},
	}
	// Wrapper: blockTime = genesis+10 = 1010 -> still regolith/Bedrock, so the
	// 164B Ecotone deposit is rejected.
	if err := assertL1BlockConsistency(cfg, in); err == nil {
		t.Fatal("wrapper must judge the block at genesis+10 (pre-Ecotone) and reject a 164B Ecotone deposit")
	}
	// Explicit block time at/after EcotoneTime -> Ecotone layout, accepted.
	if err := assertL1BlockConsistencyAt(cfg, in, 1050); err != nil {
		t.Fatalf("explicit blockTime 1050 must accept the Ecotone deposit: %v", err)
	}
}

// TestLadderEcotoneCrossFamilySmoke is acceptance smoke #1:
// "0:regolith,2:canyon,4:ecotone" (n=6). Generation succeeds, every block's
// _info.hardfork is correct, assertL1BlockConsistencyAt passes per block
// (generation would fail otherwise), and the Ecotone-family deposits REALLY
// execute: block 4/5 postState carries l1BlockStorage("ecotone")'s values.
//
// Contrast (recorded here and re-asserted below): before A2 the genesis L1Block
// used l1BlockCodeFor(topFork) = the 146B runtime for topFork=ecotone, and that
// runtime REVERTS on the 164B Ecotone selector (A1's
// TestCombinedL1BlockSelectorEquivalence pins the same fact). So the 164B
// deposits on blocks 4/5 are exactly what the combined runtime makes possible.
func TestLadderEcotoneCrossFamilySmoke(t *testing.T) {
	spec, err := parseLadderFlag("0:regolith,2:canyon,4:ecotone", 6)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generateLadderChain(spec, 6)
	if err != nil {
		t.Fatalf("generateLadderChain: %v", err)
	}
	wantHF := []string{"regolith", "canyon", "canyon", "ecotone", "ecotone", "ecotone"}
	for i, hf := range wantHF {
		if got := out.Blocks[i].Info.Hardfork; got != hf {
			t.Fatalf("block %d hardfork: want %s, got %s", i, hf, got)
		}
	}
	// Attributes deposit executed on EVERY block (status 0x1 == not reverted).
	for i := range out.Blocks {
		rs := out.Blocks[i].OpExpected.Receipts
		if len(rs) == 0 {
			t.Fatalf("block %d has no receipts", i)
		}
		if rs[0].Status != "0x1" {
			t.Fatalf("block %d attributes deposit status: want 0x1, got %s", i, rs[0].Status)
		}
	}
	// Calldata layout: Bedrock 260B through the Ecotone activation block
	// (block 3 carries the parent layout), 164B Ecotone from block 4 on.
	for i := 0; i < 4; i++ {
		d := ladderBlockAttrsData(t, out.Blocks[i])
		if len(d) != 4+32*8 || !bytes.Equal(d[:4], types.BedrockL1AttributesSelector) {
			t.Fatalf("block %d: want 260B Bedrock attributes, got len=%d sel=%x", i, len(d), d[:4])
		}
	}
	for i := 4; i < 6; i++ {
		d := ladderBlockAttrsData(t, out.Blocks[i])
		if len(d) != 164 || !bytes.Equal(d[:4], types.EcotoneL1AttributesSelector) {
			t.Fatalf("block %d: want 164B Ecotone attributes, got len=%d sel=%x", i, len(d), d[:4])
		}
	}
	// The pre-A2 runtime would revert on this very calldata.
	fp := defaultFeeParams()
	if ok, _, _, _ := l1BlockExec(t, l1BlockRuntimeCode, fp.attributesData("ecotone"), l1Sentinel()); ok {
		t.Fatal("PRE-EXISTING FACT CHANGED: the 146B runtime must revert on the Ecotone selector")
	}
	// A2 proof that the Ecotone deposits executed: postState slots ==
	// l1BlockStorage("ecotone") = {1,3,7}, slot8 untouched.
	want := fp.l1BlockStorage("ecotone")
	for _, idx := range []int{4, 5} {
		acc := out.Blocks[idx].PostState[l1BlockAddr]
		for slot, val := range want {
			if got := acc.Storage[slot]; got != val {
				t.Fatalf("block %d L1Block slot %s: want %s, got %s", idx, slot.Hex(), val.Hex(), got.Hex())
			}
		}
		if _, ok := acc.Storage[types.OperatorFeeParamsSlot]; ok {
			t.Fatalf("block %d: the Ecotone layout must not write slot8", idx)
		}
	}
}

// TestLadderFourFamilySmoke is acceptance smoke #2:
// "0:regolith,2:canyon,4:ecotone,6:isthmus,8:jovian" (n=12). All four L1Block
// layout families are reached in one chain. The spec SKIPS fjord/granite/
// holocene; generateLadderChain couples them to the next activated fork's
// timestamp (a real chain cannot skip a fork, and op-geth's post-Isthmus
// receipt cost function requires Fjord -- without coupling generation fails at
// the first 176B block).
func TestLadderFourFamilySmoke(t *testing.T) {
	spec, err := parseLadderFlag("0:regolith,2:canyon,4:ecotone,6:isthmus,8:jovian", 12)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generateLadderChain(spec, 12)
	if err != nil {
		t.Fatalf("generateLadderChain: %v", err)
	}
	wantHF := []string{
		"regolith", "canyon", "canyon", "ecotone", "ecotone",
		"isthmus", "isthmus", "jovian", "jovian", "jovian", "jovian", "jovian",
	}
	for i, hf := range wantHF {
		if got := out.Blocks[i].Info.Hardfork; got != hf {
			t.Fatalf("block %d hardfork: want %s, got %s", i, hf, got)
		}
	}
	for i := range out.Blocks {
		rs := out.Blocks[i].OpExpected.Receipts
		if len(rs) == 0 || rs[0].Status != "0x1" {
			t.Fatalf("block %d attributes deposit did not execute", i)
		}
	}
	// All four families reachable, and both activation-block forms are the
	// fauthful pre-fork form:
	//   idx3 ecotone activation -> parent Bedrock 260B;
	//   idx5 isthmus activation -> parent Ecotone 164B (Task 5 exemption);
	//   idx7 jovian activation  -> parent Isthmus 176B, deposits-only (Task 6).
	wantAttrs := []struct {
		n   int
		sel []byte
	}{
		{4 + 32*8, types.BedrockL1AttributesSelector}, // 0 regolith
		{4 + 32*8, types.BedrockL1AttributesSelector}, // 1 canyon
		{4 + 32*8, types.BedrockL1AttributesSelector}, // 2 canyon
		{4 + 32*8, types.BedrockL1AttributesSelector}, // 3 ecotone ACTIVATION (old Bedrock form)
		{164, types.EcotoneL1AttributesSelector},      // 4 first Ecotone-format
		{164, types.EcotoneL1AttributesSelector},      // 5 isthmus ACTIVATION (old Ecotone form)
		{176, types.IsthmusL1AttributesSelector},      // 6 first Isthmus-format
		{176, types.IsthmusL1AttributesSelector},      // 7 jovian ACTIVATION (old Isthmus form)
		{178, types.JovianL1AttributesSelector},       // 8 first Jovian-format
		{178, types.JovianL1AttributesSelector},       // 9
		{178, types.JovianL1AttributesSelector},       // 10
		{178, types.JovianL1AttributesSelector},       // 11
	}
	for i, w := range wantAttrs {
		d := ladderBlockAttrsData(t, out.Blocks[i])
		if len(d) != w.n || !bytes.Equal(d[:4], w.sel) {
			t.Fatalf("block %d attributes: want len=%d sel=%x, got len=%d sel=%x", i, w.n, w.sel, len(d), d[:4])
		}
	}
	// Jovian activation block: deposits-only (op-geth CalcDAFootprint).
	for _, raw := range out.Blocks[7].Block.Transactions {
		var st outputSignedTx
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatalf("decode gate block tx: %v", err)
		}
		if st.OpType != "deposit" {
			t.Fatalf("Jovian activation block must be deposits-only, got %q", st.OpType)
		}
	}
	// Task 6 DA transition: slot8 DA bytes stay unset through the Isthmus blocks
	// and the Jovian activation block; the FIRST 178B Jovian block's deposit
	// writes daScalar=400 at slot8[18:20] (operator-fee segment zero with the
	// default fp), and it persists.
	for _, idx := range []int{5, 6, 7} {
		acc := out.Blocks[idx].PostState[l1BlockAddr]
		if got, ok := acc.Storage[types.OperatorFeeParamsSlot]; ok && !isZero(got[18:20]) {
			t.Fatalf("block %d: DA bytes must be unset before the first 178B block, got %x", idx, got[18:20])
		}
	}
	daSlot := out.Blocks[8].PostState[l1BlockAddr].Storage[types.OperatorFeeParamsSlot]
	if !bytes.Equal(daSlot[18:20], []byte{0x01, 0x90}) {
		t.Fatalf("first 178B Jovian block must write daScalar 400 (0x0190), got %x", daSlot[18:20])
	}
	if !isZero(daSlot[20:32]) {
		t.Fatalf("operator-fee segment must stay zero with the default fp, got %x", daSlot[20:32])
	}
	for idx := 9; idx < 12; idx++ {
		if got := out.Blocks[idx].PostState[l1BlockAddr].Storage[types.OperatorFeeParamsSlot]; got != daSlot {
			t.Fatalf("block %d slot8 %s != first-Jovian slot8 %s (DA must persist)", idx, got.Hex(), daSlot.Hex())
		}
	}
}

// ---------------------------------------------------------------------
// P2-A contract-layer probes.
// ---------------------------------------------------------------------

// ladderProbeReceipt pairs a block tx's `to` with its receipt, so tests can
// select probe txs by target address without postState-side heuristics.
type ladderProbeReceipt struct {
	opType string
	to     *common.Address
	nonce  uint64
	rc     expectedReceipt
}

func ladderProbeReceipts(t *testing.T, blk chainBlockOutput) []ladderProbeReceipt {
	t.Helper()
	if len(blk.Block.Transactions) != len(blk.OpExpected.Receipts) {
		t.Fatalf("tx/receipt count mismatch: %d vs %d",
			len(blk.Block.Transactions), len(blk.OpExpected.Receipts))
	}
	out := make([]ladderProbeReceipt, 0, len(blk.Block.Transactions))
	for i, raw := range blk.Block.Transactions {
		var st outputSignedTx
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatalf("decode tx %d: %v", i, err)
		}
		out = append(out, ladderProbeReceipt{
			opType: st.OpType, to: st.To, nonce: uint64(st.Nonce),
			rc: blk.OpExpected.Receipts[i],
		})
	}
	return out
}

func probeReceiptsTo(rs []ladderProbeReceipt, addr common.Address) []ladderProbeReceipt {
	var out []ladderProbeReceipt
	for _, r := range rs {
		if r.to != nil && *r.to == addr {
			out = append(out, r)
		}
	}
	return out
}

// TestLadderLogsCodeByteStable anchors the PRE-EXISTING logsCode() bytes. The
// P2-A extended contract is a separate object precisely so this sha256 does not
// move: the contract_logs vector's `pre.code` carries these bytes verbatim, so
// a change here would split that vector.
func TestLadderLogsCodeByteStable(t *testing.T) {
	const wantSHA = "ff7a6d4451bd10ea33f9133bce8669f95a862c529def55c09ecaf4640970bb59"
	got := sha256.Sum256(logsCode())
	if gotHex := fmt.Sprintf("%x", got); gotHex != wantSHA {
		t.Fatalf("logsCode() changed (contract_logs vector would split): want %s, got %s", wantSHA, gotHex)
	}
	if len(logsCode()) != 113 {
		t.Fatalf("logsCode() length: want 113, got %d", len(logsCode()))
	}
}

// TestLadderContractProbeEventShapes exercises the four selector entry points of
// the genesis logs predeploy on the first probe block (index 25):
//   - 3 topics + 32-byte non-empty data (ERC-20 Transfer shape),
//   - a >=2-log receipt,
//   - a zero-topic LOG0 with non-empty data,
//   - a 256-byte data log.
func TestLadderContractProbeEventShapes(t *testing.T) {
	spec, err := parseLadderFlag("0:regolith,2:canyon", 30)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generateLadderChain(spec, 30)
	if err != nil {
		t.Fatalf("generateLadderChain: %v", err)
	}
	receipts := probeReceiptsTo(ladderProbeReceipts(t, out.Blocks[25]), ladderLogsProbeAddr)
	if len(receipts) != 4 {
		t.Fatalf("block 25: want 4 calls to the logs predeploy, got %d", len(receipts))
	}
	for i, r := range receipts {
		if r.rc.Status != "0x1" {
			t.Fatalf("block 25 logs call %d: status want 0x1, got %s", i, r.rc.Status)
		}
	}

	transfer := receipts[0].rc
	if len(transfer.Logs) != 1 || len(transfer.Logs[0].Topics) != 3 {
		t.Fatalf("shape0: want 1 log with 3 topics, got logs=%d", len(transfer.Logs))
	}
	wantTopic0 := "0x" + common.Bytes2Hex(crypto.Keccak256([]byte("Transfer(address,address,uint256)")))
	if transfer.Logs[0].Topics[0] != wantTopic0 {
		t.Fatalf("shape0 topic0: want %s, got %s", wantTopic0, transfer.Logs[0].Topics[0])
	}
	wantFrom := "0x" + common.Bytes2Hex(common.LeftPadBytes(ladderLogsFromAddr.Bytes(), 32))
	wantTo := "0x" + common.Bytes2Hex(common.LeftPadBytes(ladderLogsToAddr.Bytes(), 32))
	if transfer.Logs[0].Topics[1] != wantFrom || transfer.Logs[0].Topics[2] != wantTo {
		t.Fatalf("shape0 indexed topics: want from=%s to=%s, got %v", wantFrom, wantTo, transfer.Logs[0].Topics[1:])
	}
	if transfer.Logs[0].Data == "0x" || len(transfer.Logs[0].Data) != 2+64 {
		t.Fatalf("shape0 data: want non-empty 32 bytes, got %q", transfer.Logs[0].Data)
	}

	if multi := receipts[1].rc; len(multi.Logs) < 2 {
		t.Fatalf("shape1: want >=2 logs, got %d", len(multi.Logs))
	}

	zero := receipts[2].rc
	if len(zero.Logs) != 1 || len(zero.Logs[0].Topics) != 0 {
		t.Fatalf("shape2: want exactly 1 zero-topic log, got logs=%d topics=%d",
			len(zero.Logs), topicsLen(zero.Logs))
	}
	if zero.Logs[0].Data == "0x" {
		t.Fatalf("shape2: zero-topic log must still carry non-empty data")
	}

	long := receipts[3].rc
	if len(long.Logs) != 1 || len(long.Logs[0].Topics) != 1 {
		t.Fatalf("shape3: want 1 log with 1 topic, got logs=%d", len(long.Logs))
	}
	if got := len(long.Logs[0].Data); got != 2+512 {
		t.Fatalf("shape3 data: want 256 bytes (514 hex chars), got %d chars", got)
	}
}

func topicsLen(logs []outputLog) int {
	if len(logs) == 0 {
		return 0
	}
	return len(logs[0].Topics)
}

// TestLadderContractProbeCreateCallLifecycle checks the in-chain CREATE of the
// same logs contract and the call that follows it: the created address
// re-derives from the create nonce and exists in postState with the exact probe
// runtime, and the call's receipt is a 3-topic Transfer log from that address.
func TestLadderContractProbeCreateCallLifecycle(t *testing.T) {
	spec, err := parseLadderFlag("0:regolith,2:canyon", 30)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generateLadderChain(spec, 30)
	if err != nil {
		t.Fatalf("generateLadderChain: %v", err)
	}
	rs := ladderProbeReceipts(t, out.Blocks[25])
	var create *ladderProbeReceipt
	for i := range rs {
		if rs[i].opType == "eip1559" && rs[i].to == nil {
			create = &rs[i]
			break
		}
	}
	if create == nil {
		t.Fatal("block 25 carries no in-chain CREATE probe (eip1559 to == null)")
	}
	created := crypto.CreateAddress(addrOfKey(1), create.nonce)
	acc, ok := out.Blocks[25].PostState[created]
	if !ok {
		t.Fatalf("created probe contract %s missing from block 25 postState", created.Hex())
	}
	if !bytes.Equal(acc.Code, ladderLogsProbeCode()) {
		t.Fatalf("created probe contract code differs from ladderLogsProbeCode (len %d vs %d)",
			len(acc.Code), len(ladderLogsProbeCode()))
	}
	call := probeReceiptsTo(rs, created)
	if len(call) != 1 {
		t.Fatalf("want exactly 1 call to the created contract, got %d", len(call))
	}
	if call[0].rc.Status != "0x1" || len(call[0].rc.Logs) != 1 || len(call[0].rc.Logs[0].Topics) != 3 {
		t.Fatalf("created-contract call: want status 0x1 + 1 three-topic log, got status=%s logs=%d",
			call[0].rc.Status, len(call[0].rc.Logs))
	}
	if call[0].rc.Logs[0].Address != "0x"+common.Bytes2Hex(created.Bytes()) {
		t.Fatalf("created-contract log address: want %s, got %s",
			created.Hex(), call[0].rc.Logs[0].Address)
	}
}

// TestLadderContractProbeChurnStorage checks the storage-churn contract: each
// probe block leaves exactly its K=8 slot group non-zero, clears the previous
// group (absent from postState), and stores the warmed rewrite values.
func TestLadderContractProbeChurnStorage(t *testing.T) {
	spec, err := parseLadderFlag("0:regolith,2:canyon", 60)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generateLadderChain(spec, 60)
	if err != nil {
		t.Fatalf("generateLadderChain: %v", err)
	}
	checkGroup := func(idx int, base uint64) {
		t.Helper()
		acc, ok := out.Blocks[idx].PostState[ladderStorageProbeAddr]
		if !ok {
			t.Fatalf("block %d: churn account missing from postState", idx)
		}
		if len(acc.Storage) != ladderChurnSlotCount {
			t.Fatalf("block %d: want %d non-zero churn slots, got %d",
				idx, ladderChurnSlotCount, len(acc.Storage))
		}
		for j := 0; j < ladderChurnSlotCount; j++ {
			key := common.BigToHash(new(big.Int).SetUint64(base + uint64(j)))
			want := common.BigToHash(new(big.Int).SetUint64(0x31 + uint64(j)))
			if got := acc.Storage[key]; got != want {
				t.Fatalf("block %d slot %d: want %s, got %s", idx, base+uint64(j), want.Hex(), got.Hex())
			}
		}
		// The previous group must be gone (cleared to zero -> not in the trie).
		if base >= ladderChurnSlotStride {
			prev := common.BigToHash(new(big.Int).SetUint64(base - ladderChurnSlotStride))
			if got, ok := acc.Storage[prev]; ok {
				t.Fatalf("block %d: previous group slot %s should be cleared, got %s",
					idx, prev.Hex(), got.Hex())
			}
		}
	}
	checkGroup(25, 0*ladderChurnSlotStride)
	checkGroup(50, 1*ladderChurnSlotStride)
	// Between probes the churn state persists through the candidate chain, so
	// the pre of the next probe still sees the previous group (the clear path
	// above is only meaningful if it does).
	if got := out.Blocks[26].PostState[ladderStorageProbeAddr].Storage; len(got) != ladderChurnSlotCount {
		t.Fatalf("block 26 (non-probe): churn storage not carried forward, got %d slots", len(got))
	}
}

// TestLadderContractProbeRevertAndInvalid checks the two failure paths:
// REVERT with abi-encoded Error(string) reason (status 0, non-empty output,
// partial gas) and the INVALID opcode (status 0, empty output, all gas).
func TestLadderContractProbeRevertAndInvalid(t *testing.T) {
	spec, err := parseLadderFlag("0:regolith,2:canyon", 30)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generateLadderChain(spec, 30)
	if err != nil {
		t.Fatalf("generateLadderChain: %v", err)
	}
	rs := ladderProbeReceipts(t, out.Blocks[25])

	rev := probeReceiptsTo(rs, ladderRevertProbeAddr)
	if len(rev) != 1 {
		t.Fatalf("want 1 revert probe, got %d", len(rev))
	}
	wantOut := "0x" + common.Bytes2Hex(ladderRevertReasonPayload())
	if rev[0].rc.Status != "0x0" {
		t.Fatalf("revert probe status: want 0x0, got %s", rev[0].rc.Status)
	}
	if rev[0].rc.Output != wantOut {
		t.Fatalf("revert probe output: want %s, got %s", wantOut, rev[0].rc.Output)
	}
	if rev[0].rc.GasUsed == fmt.Sprintf("0x%x", ladderRevertProbeGas) {
		t.Fatalf("revert probe must not consume the whole gas limit (REVERT is partial)")
	}

	inv := probeReceiptsTo(rs, ladderInvalidProbeAddr)
	if len(inv) != 1 {
		t.Fatalf("want 1 invalid-opcode probe, got %d", len(inv))
	}
	if inv[0].rc.Status != "0x0" {
		t.Fatalf("invalid probe status: want 0x0, got %s", inv[0].rc.Status)
	}
	if inv[0].rc.Output != "0x" {
		t.Fatalf("invalid probe output: want 0x, got %s", inv[0].rc.Output)
	}
	if want := fmt.Sprintf("0x%x", ladderInvalidProbeGas); inv[0].rc.GasUsed != want {
		t.Fatalf("invalid probe gasUsed: want %s (whole limit), got %s", want, inv[0].rc.GasUsed)
	}
}

// TestLadderContractProbeSkipRule anchors the skip rule: probes land on every
// 25th block EXCEPT the existing %100 CREATE block and any fork activation
// block. canyon@26 puts its activation on index 25, a probe slot that must be
// skipped; index 50 (no conflict) must still carry the probes.
func TestLadderContractProbeSkipRule(t *testing.T) {
	hasProbe := func(blk chainBlockOutput) bool {
		return len(probeReceiptsTo(ladderProbeReceipts(t, blk), ladderLogsProbeAddr)) > 0
	}
	// canyon@26 -> activation index 25.
	spec, err := parseLadderFlag("0:regolith,26:canyon", 60)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generateLadderChain(spec, 60)
	if err != nil {
		t.Fatalf("generateLadderChain: %v", err)
	}
	if hasProbe(out.Blocks[25]) {
		t.Fatal("block 25 is the canyon activation block: probes must be skipped")
	}
	// The REGULAR recipe txs on an activation block are untouched (attr + user
	// deposit + transfer + withdrawal).
	if got := len(out.Blocks[25].Block.Transactions); got != 4 {
		t.Fatalf("activation block tx count: want 4 (regular recipe intact), got %d", got)
	}
	if !hasProbe(out.Blocks[50]) {
		t.Fatal("block 50 (no activation conflict) must carry the probes")
	}

	// %100==0 keeps only the existing CREATE probe.
	spec2, err := parseLadderFlag("0:regolith,2:canyon", 101)
	if err != nil {
		t.Fatal(err)
	}
	out2, err := generateLadderChain(spec2, 101)
	if err != nil {
		t.Fatalf("generateLadderChain: %v", err)
	}
	if hasProbe(out2.Blocks[100]) {
		t.Fatal("block 100 is the %100 CREATE block: contract probes must be skipped")
	}
	created := false
	for _, r := range ladderProbeReceipts(t, out2.Blocks[100]) {
		if r.opType == "eip1559" && r.to == nil {
			created = true
		}
	}
	if !created {
		t.Fatal("block 100 must still carry the pre-existing CREATE probe")
	}
}

// TestLadderContractProbeDeterminism: same spec twice -> byte-identical JSON
// (new addresses, calldata, nonces and storage declarations are all inputs).
func TestLadderContractProbeDeterminism(t *testing.T) {
	spec, err := parseLadderFlag("0:regolith,2:canyon,4:ecotone", 55)
	if err != nil {
		t.Fatal(err)
	}
	sum := func() string {
		out, err := generateLadderChain(spec, 55)
		if err != nil {
			t.Fatalf("generateLadderChain: %v", err)
		}
		data, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		h := sha256.Sum256(data)
		return fmt.Sprintf("%x", h)
	}
	if a, b := sum(), sum(); a != b {
		t.Fatalf("ladder generation is not deterministic: %s != %s", a, b)
	}
}
