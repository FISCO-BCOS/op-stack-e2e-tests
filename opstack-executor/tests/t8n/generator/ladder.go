package main

import (
	"fmt"
	"strconv"
	"strings"
)

// ladderGenesisTime 与 generateChainN 的 genesisTime 对齐（1000）。
const ladderGenesisTime = uint64(1000)

// ladderBlockInterval 与 chain_makers.makeHeader 的固定块间隔一致（parent+10s）。
const ladderBlockInterval = uint64(10)

// ladderForks 是 OP-Stack fork 的规范激活顺序。ladder 激活表必须是它的
// （从 regolith 起始的）严格递增子序列：重复 fork 与乱序 fork（如 ecotone
// 早于 canyon）都会导致时间线矛盾或被下游 map 静默覆盖。数组本身即白名单。
var ladderForks = [...]string{
	"regolith", "canyon", "ecotone", "fjord",
	"granite", "holocene", "isthmus", "jovian",
}

// ladderActivation 是 ladder 模式的一档激活。块号语义（设计 v2 §3.1）：
// GenerateChain 的块时间固定 parent+10s，用绝对秒数手写激活点极易超出
// --blocks×10s 而整段静默失效。
type ladderActivation struct {
	Fork      string
	Block     int
	Timestamp uint64 // ladderGenesisTime + ladderBlockInterval*Block
}

type ladderSpec struct {
	Activations []ladderActivation
}

// parseLadderFlag 解析 "0:regolith,125:canyon,…"。校验：块号严格递增、fork 名
// 沿 ladderForks 规范顺序严格递增（重复与乱序一并拒绝）、首个 0:regolith、
// karst 拒绝（上游 op-geth 无 Karst EL——设计 §10）、max 块号 < totalBlocks
// （越界报错，不静默截断）。
//
// totalBlocks 下限是 2 而非全梯 8 档：部分 ladder（只激活前几档）是有意支持
// 的——Task 2 的 3 块 smoke 与 Task 5 的采样测试都依赖它；下限 2 因为至少要
// 有 1 个 post-genesis 块。
func parseLadderFlag(s string, totalBlocks int) (ladderSpec, error) {
	if totalBlocks < 2 {
		return ladderSpec{}, fmt.Errorf("ladder needs totalBlocks >= 2, got %d", totalBlocks)
	}
	parts := strings.Split(s, ",")
	if len(parts) < 2 {
		return ladderSpec{}, fmt.Errorf("ladder needs >= 2 activations, got %q", s)
	}
	var spec ladderSpec
	prevBlock, prevIdx := -1, -1
	for i, p := range parts {
		kv := strings.Split(strings.TrimSpace(p), ":")
		if len(kv) != 2 {
			return ladderSpec{}, fmt.Errorf("activation %d %q: want <block>:<fork>", i, p)
		}
		blk, err := strconv.Atoi(kv[0])
		if err != nil || blk < 0 {
			return ladderSpec{}, fmt.Errorf("activation %d %q: bad block %q (%v)", i, p, kv[0], err)
		}
		if blk <= prevBlock {
			return ladderSpec{}, fmt.Errorf(
				"activation %d block %d not > previous %d", i, blk, prevBlock)
		}
		prevBlock = blk
		fork := strings.ToLower(strings.TrimSpace(kv[1]))
		switch fork {
		case "karst":
			return ladderSpec{}, fmt.Errorf(
				"karst is excluded from ladder: op-geth pin has no Karst EL semantics (design §10)")
		}
		idx := -1
		for j, f := range ladderForks {
			if f == fork {
				idx = j
				break
			}
		}
		if idx < 0 {
			return ladderSpec{}, fmt.Errorf(
				"activation %d %q: unknown fork %q (want %s)",
				i, p, fork, strings.Join(ladderForks[:], "|"))
		}
		if idx <= prevIdx {
			return ladderSpec{}, fmt.Errorf(
				"activation %d fork %q out of canonical order (block %d)", i, fork, blk)
		}
		prevIdx = idx
		if blk >= totalBlocks {
			return ladderSpec{}, fmt.Errorf(
				"activation block %d >= --blocks %d; the fork would never activate", blk, totalBlocks)
		}
		spec.Activations = append(spec.Activations, ladderActivation{
			Fork: fork, Block: blk, Timestamp: ladderGenesisTime + ladderBlockInterval*uint64(blk),
		})
	}
	if spec.Activations[0].Fork != "regolith" || spec.Activations[0].Block != 0 {
		return ladderSpec{}, fmt.Errorf("first activation must be 0:regolith, got %d:%s",
			spec.Activations[0].Block, spec.Activations[0].Fork)
	}
	return spec, nil
}
