package main

import (
	"fmt"
	"strconv"
	"strings"
)

// ladderGenesisTime 与 generateChainN 的 genesisTime 对齐（1000）。
const ladderGenesisTime = uint64(1000)

// ladderActivation 是 ladder 模式的一档激活。块号语义（设计 v2 §3.1）：
// GenerateChain 的块时间固定 parent+10s，用绝对秒数手写激活点极易超出
// --blocks×10s 而整段静默失效。
type ladderActivation struct {
	Fork      string
	Block     int
	Timestamp uint64 // ladderGenesisTime + 10*Block
}

type ladderSpec struct {
	Activations []ladderActivation
}

// parseLadderFlag 解析 "0:regolith,125:canyon,…"。校验：块号严格递增、首个
// 0:regolith、fork 名已知、karst 拒绝（上游 op-geth 无 Karst EL——设计 §10）、
// max 块号 < totalBlocks（越界报错，不静默截断）。
func parseLadderFlag(s string, totalBlocks int) (ladderSpec, error) {
	if totalBlocks < 8 {
		return ladderSpec{}, fmt.Errorf("ladder needs totalBlocks >= 8, got %d", totalBlocks)
	}
	parts := strings.Split(s, ",")
	if len(parts) < 2 {
		return ladderSpec{}, fmt.Errorf("ladder needs >= 2 activations, got %q", s)
	}
	var spec ladderSpec
	prev := -1
	for i, p := range parts {
		kv := strings.Split(strings.TrimSpace(p), ":")
		if len(kv) != 2 {
			return ladderSpec{}, fmt.Errorf("activation %d %q: want <block>:<fork>", i, p)
		}
		blk, err := strconv.Atoi(kv[0])
		if err != nil || blk < 0 {
			return ladderSpec{}, fmt.Errorf("activation %d %q: bad block", i, p)
		}
		if blk <= prev {
			return ladderSpec{}, fmt.Errorf("activation %d block %d not > previous %d", i, blk, prev)
		}
		prev = blk
		fork := strings.ToLower(strings.TrimSpace(kv[1]))
		switch fork {
		case "regolith", "canyon", "ecotone", "fjord", "granite", "holocene", "isthmus", "jovian":
		case "karst":
			return ladderSpec{}, fmt.Errorf(
				"karst is excluded from ladder: op-geth pin has no Karst EL semantics (design §10)")
		default:
			return ladderSpec{}, fmt.Errorf("unknown activation fork %q", fork)
		}
		if blk >= totalBlocks {
			return ladderSpec{}, fmt.Errorf(
				"activation block %d >= --blocks %d; the fork would never activate", blk, totalBlocks)
		}
		spec.Activations = append(spec.Activations, ladderActivation{
			Fork: fork, Block: blk, Timestamp: ladderGenesisTime + uint64(10*blk),
		})
	}
	if spec.Activations[0].Fork != "regolith" || spec.Activations[0].Block != 0 {
		return ladderSpec{}, fmt.Errorf("first activation must be 0:regolith, got %d:%s",
			spec.Activations[0].Block, spec.Activations[0].Fork)
	}
	return spec, nil
}
