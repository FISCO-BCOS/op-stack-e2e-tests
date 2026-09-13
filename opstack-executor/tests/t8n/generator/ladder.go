package main

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
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

// ---------------------------------------------------------------------
// Task 3 (D1c): per-fork transaction recipes.
// ---------------------------------------------------------------------

// ladderBn256PairingProbeGas / ladderP256VerifyProbeGas are the probe txs' gas
// limits (design v2 §3.3/§6). The P256VERIFY value (34500) clears intrinsic
// (160B input -> 23560) plus the 3450 RequiredGas, so the probe really runs.
// The bn256-pairing value (200000) must exceed intrinsic (192B input, 64 zero
// bytes -> 23304) + Bn256PairingBaseGasIstanbul(45000) + 34000/pair = 102304
// for repeatedBn256Pair(1); a lower value (the brief's 60000) OOGs INSIDE the
// precompile, so the pairing never executes and the probe is meaningless.
// The fine-grained Granite-era gas boundary (the 112687-byte input cap) is
// deferred to Task 6, once granite segments are reachable.
const (
	ladderBn256PairingProbeGas = 200_000
	ladderP256VerifyProbeGas   = 34_500
)

// txRecipe 描述一块的配方交易组成（attributes deposit 由 generateLadderChain
// 单独注入，不入配方）。设计 v2 §3.3/§6；withdrawal 以 §6 的 Canyon 起为准。
type txRecipe struct {
	deposit     int
	withdrawal  int
	create      bool
	precompiles []precompileProbe
}

type precompileProbe struct {
	addr  common.Address
	input []byte
	gas   uint64
}

// recipeFor 返回某 fork 某块号的配方。isForkActivation=true 表示该块是某个
// fork 的激活块：Jovian 的激活块必须 deposits-only（op-geth
// core/types/rollup_cost.go:571-576 会拒绝任何非存款交易），其余 fork 的激活
// 块无此限制。
//
// 规则（设计 v2 §3.3/§6）：
//   - deposit: 1 —— 每块一笔用户存款（attributes deposit 不在配方内）；
//   - withdrawal: 1 —— Canyon 起每块一笔（regolith 无 MessagePasser 提款语义）；
//   - create: blockIdx > 0 && blockIdx%100 == 0；
//   - precompile probes: granite/holocene 探 bn256 pairing 地址，isthmus/jovian
//     探 P256VERIFY 地址。
//
// 地址/输入全部复用既有 precompile 用例的构造（addrBytes(preBn256Pairing) /
// repeatedBn256Pair(1) 与 addrBytes(preP256Verify) / validP256Sig()），不另造值；
// 依据 op-geth 源码只有 Jovian 存在激活块 deposits-only 约束（见 report）。
func recipeFor(fork string, blockIdx int, isForkActivation bool) txRecipe {
	if isForkActivation && fork == "jovian" {
		return txRecipe{deposit: 1}
	}
	r := txRecipe{deposit: 1}
	if fork != "regolith" {
		r.withdrawal = 1
	}
	if blockIdx > 0 && blockIdx%100 == 0 {
		r.create = true
	}
	switch fork {
	case "granite", "holocene":
		r.precompiles = []precompileProbe{{
			addr:  common.BytesToAddress(addrBytes(preBn256Pairing)),
			input: repeatedBn256Pair(1),
			gas:   ladderBn256PairingProbeGas,
		}}
	case "isthmus", "jovian":
		r.precompiles = []precompileProbe{{
			addr:  common.BytesToAddress(addrBytes(preP256Verify)),
			input: validP256Sig(),
			gas:   ladderP256VerifyProbeGas,
		}}
	}
	return r
}

// ladderWithdrawalTarget / ladderWithdrawalGasLimit / ladderWithdrawalData 与
// cases.go message_passer_withdraw 的构造同构（selector 0xc2b3e5ac +
// ABI(address,uint256,bytes)，target 0xdead…0001，gasLimit 100000，data 0xbeef）。
var (
	ladderWithdrawalTarget   = common.HexToAddress("0xdead000000000000000000000000000000000001")
	ladderWithdrawalGasLimit = uint64(100_000)
	ladderWithdrawalData     = []byte{0xbe, 0xef}
)

// withdrawalCalldata 逐字节复刻 cases.go message_passer_withdraw 的 calldata
// 构造（selector 0xc2b3e5ac + ABI(address,uint256,bytes)，4+32*5 = 164 字节）。
func withdrawalCalldata() hexutil.Bytes {
	calldata := make([]byte, 4+32*5)
	copy(calldata[0:4], []byte{0xc2, 0xb3, 0xe5, 0xac})                   // selector
	copy(calldata[16:36], ladderWithdrawalTarget[:])                      // target (left-pad 12B)
	binary.BigEndian.PutUint64(calldata[60:68], ladderWithdrawalGasLimit) // gasLimit at [36:68]
	binary.BigEndian.PutUint64(calldata[92:100], 0x60)                    // dataOffset at [68:100]
	binary.BigEndian.PutUint64(calldata[124:132], 2)                      // dataLen at [100:132]
	copy(calldata[132:134], ladderWithdrawalData)                         // data at [132:134]
	return calldata
}

// withdrawalTx 是一笔从 key1 发往 L2ToL1MessagePasser 的 initiateWithdrawal
// EIP-1559 交易；nonce 由调用方以 bg.TxNonce(senderAddr) 链式给出。
func withdrawalTx(nonce uint64) inputTx {
	return transferTx(1, nonce, messagePasserAddr, big.NewInt(0), 200_000, withdrawalCalldata())
}

// withdrawalSlots 返回第 k 次（1-based）withdrawal 在 MessagePasser 上声明的两个
// 写入槽（emitPostState 对未声明槽位硬失败）。全部输入固定 → 确定性。
//
// 槽位语义（合约字节码 + cases.go:1030-1046 + 存储 dump 三重确认）：真实
// `_msgNonce` 恒位于 slot 1，其值随调用次数自增（第 k 次调用 hash 用的
// versionedNonce = (1<<240)|(k-1)，调用后 slot 1 = k）；声明只关心"哪个槽"，
// 值与声明无关，因此第一个返回值无条件为 slot 1。
// sentMessages[withdrawalHash] 是 mapping 动态槽 keccak256(withdrawalHash‖0)，
// 随 withdrawalHash 变化，故第二个返回值随 k 变化。
func withdrawalSlots(k int) (common.Hash, common.Hash) {
	versionedNonce := new(big.Int).Or(
		new(big.Int).Lsh(big.NewInt(1), 240), big.NewInt(int64(k-1)))
	withdrawalHash := crypto.Keccak256(abiEncodeWithdrawal(versionedNonce,
		addrOfKey(1), ladderWithdrawalTarget, 0, ladderWithdrawalGasLimit, ladderWithdrawalData))
	sentSlot := common.BytesToHash(crypto.Keccak256(withdrawalHash, make([]byte, 32)))
	return common.BigToHash(big.NewInt(1)), sentSlot
}

// ladderUserDeposit 是每块一笔的未签名用户存款（mint 1e17 到 recA，gas 50k）。
// label 由块号/序号生成，保证 sourceHash 唯一。
func ladderUserDeposit(label string) inputTx {
	to := recA
	return inputTx{
		OpType: "deposit",
		OpDeposit: &inputDeposit{
			From:       userDepositor,
			To:         &to,
			Mint:       hd256(big.NewInt(100_000_000_000_000_000)), // 1e17
			Gas:        math.HexOrDecimal64(50_000),
			SourceHash: sourceHash(label),
		},
	}
}

// ladderCreateInitCode: PUSH1 1 PUSH1 0 SSTORE; PUSH1 0 PUSH1 0 RETURN
// （init 阶段写 slot 0 = 1，runtime 为空；created 账户 slot 0 需声明）。
var ladderCreateInitCode = hexutil.MustDecode("0x600160005560006000f3")
