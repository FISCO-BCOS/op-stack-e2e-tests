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
