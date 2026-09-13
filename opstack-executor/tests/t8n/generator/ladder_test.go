package main

import (
	"bytes"
	"encoding/json"
	"math/big"
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
	if a1 == c1 {
		t.Fatalf("k=2 msgNonce declaration slot == k=1 (%s)", a1)
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
	if a1 != common.BigToHash(big.NewInt(1)) {
		t.Fatalf("k=1 msgNonce declaration slot: want slot 1, got %s", a1)
	}
	if a2 != sentFor(1) {
		t.Fatalf("k=1 sentMessages slot: want %s, got %s", sentFor(1), a2)
	}
	if c1 != common.BigToHash(big.NewInt(2)) {
		t.Fatalf("k=2 msgNonce declaration slot: want slot 2, got %s", c1)
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
}

// TestLadderCreateProbeSmoke exercises the create:true recipe arm: with a
// 101-block canyon-family ladder, block index 100 (100>0 && %100==0) carries
// the EIP-1559 CREATE probe. Generation succeeding proves the declared slot /
// created-address pairing is exact -- a wrong crypto.CreateAddress would leave
// the real created account's slot 0 undeclared and emitPostState would
// hard-fail. (The create rule is fork-independent, so it IS reachable today
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
	for _, raw := range out.Blocks[100].Block.Transactions {
		var st outputSignedTx
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatalf("decode block 100 tx: %v", err)
		}
		if st.OpType == "eip1559" && st.To == nil {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("block 100 (blockIdx%100==0) carries no create tx (eip1559 to == null)")
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
