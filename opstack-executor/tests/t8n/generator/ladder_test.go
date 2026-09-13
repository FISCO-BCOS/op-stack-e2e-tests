package main

import (
	"strings"
	"testing"
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
