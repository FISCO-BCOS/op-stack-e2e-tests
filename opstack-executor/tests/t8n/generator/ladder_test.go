package main

import "testing"

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
	// 激活时间 = ladderGenesisTime(1000) + 10*block（chain_makers.makeHeader 固定 +10s）
	if want := ladderGenesisTime + 10*125; spec.Activations[1].Timestamp != want {
		t.Fatalf("canyon timestamp: want %d, got %d", want, spec.Activations[1].Timestamp)
	}
}

func TestParseLadderFlagRejections(t *testing.T) {
	cases := []struct {
		name   string
		ladder string
		blocks int
	}{
		{"karst excluded", "0:regolith,100:karst", 200},
		{"not increasing", "0:regolith,50:canyon,50:ecotone", 200},
		{"block beyond chain", "0:regolith,500:jovian", 400},
		{"unknown fork", "0:regolith,50:osaka", 200},
		{"first not regolith@0", "10:regolith,50:canyon", 200},
		{"single activation", "0:regolith", 200},
	}
	for _, c := range cases {
		if _, err := parseLadderFlag(c.ladder, c.blocks); err == nil {
			t.Fatalf("%s: want error, got nil", c.name)
		}
	}
}
