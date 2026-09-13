package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
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

// TestLadderRejectsLayoutBoundaryCrossing：I1 守卫。regolith(Bedrock 族) ->
// ecotone(Ecotone 族) 跨 L1Block 布局边界，一个 genesis 账户无法同时承载两种
// runtime，必须在生成前报错（而不是块内以误导性的 slot 不匹配失败）。
func TestLadderRejectsLayoutBoundaryCrossing(t *testing.T) {
	spec, err := parseLadderFlag("0:regolith,2:ecotone", 6)
	if err != nil {
		t.Fatal(err)
	}
	_, err = generateLadderChain(spec, 6)
	if err == nil {
		t.Fatal("want layout-boundary error, got nil")
	}
	if !strings.Contains(err.Error(), "layout family") {
		t.Fatalf("error %q does not contain %q", err, "layout family")
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
// is inert). (The create rule is fork-independent, so it IS reachable today
// inside the one layout family the guard allows.)
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
	// 注意 ladder 必须落在同一 L1Block 布局族内（当前仅 regolith..canyon 可生成）。
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
	if err := runLadderMode(dir, "0:regolith,2:canyon", 4, "testcommit"); err != nil {
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
	if err := runLadderMode(dir, "0:regolith,2:karst", 4, "c"); err == nil {
		t.Fatal("karst ladder must be rejected")
	}
	if err := runLadderMode("", "0:regolith,2:canyon", 4, "c"); err == nil {
		t.Fatal("missing out-dir must be rejected")
	}
}
