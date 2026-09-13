package main

// generateLadderChain 是 generateChainN 的多 fork 克隆（Plan D 差分执行 D1b）：
// 一条沿 ladderSpec 跨 fork 边界的 n 块链。fork 切换不需要任何手工介入——
// GenerateChainWithGenesis 的 makeHeader 每块 parent+10s 推进块时间，op-geth
// 的 cfg.Rules(...) 按块时间自动换挡；本函数只负责把"每块该是什么"如实记录：
//
//   - cfg 来自 buildChainConfigSpec({base: "regolith", activations})，Block==0
//     的档位跳过（regolith@0 就是 base 的形状）；ETH 孪生 fork（Shanghai/
//     Cancun/Prague）由 buildChainConfigSpec 自动重耦合。
//   - L1 attributes calldata 布局取父块 fork blockFork(cfg, blockTime-10)：
//     激活块携带旧布局——L1Block 升级在激活块内稍后才落地（与
//     assertL1BlockConsistency 的 S7 Ecotone 特例同一语义）。
//   - 每块 _info.hardfork = blockFork(cfg, blockTime)（块自身的 fork）。
//   - genesis alloc 的 L1Block code/storage 用梯顶 fork——但仅当整条 ladder
//     落在同一 L1Block 布局族内才合法（见下方 forkLayout 守卫）：一个 genesis
//     账户只能承载一种 L1Block runtime，Bedrock 族（regolith..canyon）与
//     Ecotone 族（ecotone..jovian）的 runtime/存储布局互斥（Ecotone 族不
//     dispatch Bedrock selector，槽位 1/3/7/8 vs 5/6 也不同名），跨族必须分链
//     生成。fp.daScalar=400 仅在梯顶为 jovian 时设置（与 chainN 的 jovian 臂
//     一致，DA 足迹可观察）；message passer 携带真实部署 runtime（Task 3 提款
//     要用，现在携带无害）；sender 预充值 max(1000, 2n) ETH（下限比 chainN 的
//     eth(100) 大，且线性覆盖每块一笔的 1 ETH transfer + gas：原硬编码
//     eth(1000) 在 --blocks=1000 时差最后一块 ~0.19 ETH 就 panic，实测 999
//     块 OK / 1000 块 insufficient funds；2n 对所有 n 都留有余量，小数点以下
//     的 gas 最坏 ~8.5e-4 ETH/块）。
//
// 每块配方 = L1-attributes deposit + recipeFor(fork, blockIdx, isForkActivation)
// 的用户存款 / 提款 / CREATE / precompile 探针 + 一笔真链上 nonce 的 sender
// transfer（Task 3 D1c）。deposit 必须排在非存款交易之前（assertDepositsFirst）；
// 提款写入的 MessagePasser 槽由 withdrawalSlots 声明（emitPostState 硬校验）。

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
)

// canyonCreate2DeployerAddr 是 op-geth 的 Canyon 激活注入地址
// （consensus/misc/create2deployer.go:22，pin e8800cffe）：state_processor.Process
// 在 blockTime == CanyonTime 的块（且仅该块）把 Create2Deployer 合约代码
// SetCode 进验证状态，而 chain_makers 的生成路径从不做这件事——生成/验证的
// state root 会在激活块分叉。corpus 里没有跨 CanyonTime 的既有用例（base
// fork 全是 fork-at-0，CanyonTime==0 != blockTime，注入从不触发），ladder 是
// 第一个踩中它的构造。修法（生成侧、机械性）：spec 显式激活 canyon 时把该
// 账户按 pin 的代码放进 genesis alloc —— 激活块的验证 SetCode 写入同值代码，
// 内容不变，两侧 root 一致。
var canyonCreate2DeployerAddr = common.HexToAddress("0x13b0D85CcB8bf860b6b79AF3029fCA081AE9beF2")

// ladderCreate2DeployerCode == consensus/misc/create2deployer.bin @ pin
// e8800cffe（//go:embed 的 1584B 合约），Keccak256 ==
// 0xb0550b5b431e30d38000efb7107aaa0ade03d48a7198a140edda9d27134468b2
// （create2deployer.go:22 的 create2DeployerCodeHash，即完整性锚）。
var ladderCreate2DeployerCode = hexutil.MustDecode(
	"0x6080604052600436106100435760003560e01c8063076c37b21461004f578063" +
		"481286e61461007157806356299481146100ba57806366cfa057146100da5760" +
		"0080fd5b3661004a57005b600080fd5b34801561005b57600080fd5b5061006f" +
		"61006a366004610327565b6100fa565b005b34801561007d57600080fd5b5061" +
		"009161008c366004610327565b61014a565b60405173ffffffffffffffffffff" +
		"ffffffffffffffffffff909116815260200160405180910390f35b3480156100" +
		"c657600080fd5b506100916100d5366004610349565b61015d565b3480156100" +
		"e657600080fd5b5061006f6100f53660046103ca565b610172565b6101458282" +
		"6040518060200161010f9061031a565b7fffffffffffffffffffffffffffffff" +
		"ffffffffffffffffffffffffffffffffe082820381018352601f909101166040" +
		"52610183565b505050565b600061015683836102e7565b9392505050565b6000" +
		"61016a8484846102f0565b949350505050565b61017d838383610183565b5050" +
		"5050565b6000834710156101f4576040517f08c379a000000000000000000000" +
		"000000000000000000000000000000000000815260206004820152601d602482" +
		"01527f437265617465323a20696e73756666696369656e742062616c616e6365" +
		"00000060448201526064015b60405180910390fd5b815160000361025f576040" +
		"517f08c379a00000000000000000000000000000000000000000000000000000" +
		"0000815260206004820181905260248201527f437265617465323a2062797465" +
		"636f6465206c656e677468206973207a65726f60448201526064016101eb565b" +
		"8282516020840186f5905073ffffffffffffffffffffffffffffffffffffffff" +
		"8116610156576040517f08c379a0000000000000000000000000000000000000" +
		"00000000000000000000815260206004820152601960248201527f4372656174" +
		"65323a204661696c6564206f6e206465706c6f79000000000000006044820152" +
		"6064016101eb565b60006101568383305b600060405183604082015284602082" +
		"0152828152600b8101905060ff815360559020949350505050565b61014e8061" +
		"04ad83390190565b6000806040838503121561033a57600080fd5b5050803592" +
		"6020909101359150565b60008060006060848603121561035e57600080fd5b83" +
		"35925060208401359150604084013573ffffffffffffffffffffffffffffffff" +
		"ffffffff8116811461039057600080fd5b809150509250925092565b7f4e487b" +
		"7100000000000000000000000000000000000000000000000000000000600052" +
		"604160045260246000fd5b6000806000606084860312156103df57600080fd5b" +
		"8335925060208401359150604084013567ffffffffffffffff80821115610405" +
		"57600080fd5b818601915086601f83011261041957600080fd5b813581811115" +
		"61042b5761042b61039b565b604051601f82017fffffffffffffffffffffffff" +
		"ffffffffffffffffffffffffffffffffffffffe0908116603f01168101908382" +
		"1181831017156104715761047161039b565b8160405282815289602084870101" +
		"111561048a57600080fd5b826020860160208301376000602084830101528095" +
		"505050505050925092509256fe608060405234801561001057600080fd5b5061" +
		"012e806100206000396000f3fe6080604052348015600f57600080fd5b506004" +
		"361060285760003560e01c8063249cb3fa14602d575b600080fd5b603c603836" +
		"600460b1565b604e565b60405190815260200160405180910390f35b60008281" +
		"526020818152604080832073ffffffffffffffffffffffffffffffffffffffff" +
		"8516845290915281205460ff16608857600060aa565b7fa2ef4600d742022d53" +
		"2d4747cb3547474667d6f13804902513b2ec01c848f4b45b9392505050565b60" +
		"00806040838503121560c357600080fd5b82359150602083013573ffffffffff" +
		"ffffffffffffffffffffffffffffff8116811460ed57600080fd5b8091505092" +
		"5092905056fea26469706673582212205ffd4e6cede7d06a5daf93d48d0541fc" +
		"68189eeb16608c1999a82063b666eb1164736f6c63430008130033a264697066" +
		"7358221220fdc4a0fe96e3b21c108ca155438d37c9143fb01278a3c1d274948b" +
		"ad89c564ba64736f6c63430008130033")

// init anchors the hand-copied literal above: op-geth's init() only guards its
// own copy (consensus/misc/create2deployer.go), so a generator-side typo would
// silently seed wrong code and split the activation-block state root.
func init() {
	if got := crypto.Keccak256Hash(ladderCreate2DeployerCode); got !=
		common.HexToHash("0xb0550b5b431e30d38000efb7107aaa0ade03d48a7198a140edda9d27134468b2") {
		panic("ladderCreate2DeployerCode corrupt: keccak " + got.Hex())
	}
}

func generateLadderChain(spec ladderSpec, n int) (*chainOutput, error) {
	if n < 2 {
		return nil, fmt.Errorf("generateLadderChain: n must be >= 2 (got %d)", n)
	}
	if len(spec.Activations) == 0 {
		return nil, fmt.Errorf("generateLadderChain: empty ladder spec")
	}
	activations := make(map[string]uint64, len(spec.Activations))
	for _, a := range spec.Activations {
		if a.Block == 0 {
			continue // regolith@0 即 base 的形状（buildChainConfig("regolith")）
		}
		activations[a.Fork] = a.Timestamp
	}
	cfg, err := buildChainConfigSpec(chainConfigSpec{base: "regolith", activations: activations})
	if err != nil {
		return nil, err
	}
	topFork := spec.Activations[len(spec.Activations)-1].Fork
	// 单个 genesis 账户只能承载一种 L1Block runtime：Bedrock 族
	// （regolith..canyon）与 Ecotone 族（ecotone..jovian）的 runtime 与存储
	// 布局互斥（Ecotone 族不 dispatch Bedrock selector，槽位 1/3/7/8 vs 5/6
	// 也不同名）。因此一条 ladder 必须落在同一布局族内——跨族请分链生成。
	if forkLayout(spec.Activations[0].Fork) != forkLayout(topFork) {
		return nil, fmt.Errorf(
			"ladder spans the L1Block layout boundary (%s..%s): one chain can only "+
				"carry one L1Block runtime; generate one ladder per layout family "+
				"(bedrock: regolith..canyon, ecotone-family: ecotone..jovian)",
			spec.Activations[0].Fork, topFork)
	}
	fp := defaultFeeParams()
	if topFork == "jovian" {
		fp.daScalar = 400 // non-zero DA scalar: block DA footprint observable (chainN jovian arm)
	}

	// Canyon 激活 ⇒ Process 在激活块 SetCode Create2Deployer（见
	// canyonCreate2DeployerAddr 注释），生成侧必须在 genesis 预置同一代码。
	canyonActivated := false
	for _, a := range spec.Activations {
		if a.Fork == "canyon" {
			canyonActivated = true
		}
	}

	const (
		denom      = uint64(50)
		elasticity = uint64(6)
		gasLimit   = uint64(10_000_000)
	)
	beaconRoot := common.HexToHash("0x0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c")
	senderAddr := addrOfKey(1)
	// 每块至多一笔 1 ETH 的 sender transfer（recipeFor），n 块最多 n ETH；所有
	// tx 都从 key1 出，gas 上界 ≈ 4.2e5 gas × 2 gwei ≈ 8.5e-4 ETH/块。故 2n
	// 对任意 n 都有余量；下限 1000 保持小 ladder 的历史金额不变（n=3 仍是
	// eth(1000)，既有 smoke/采样向量逐字节不变）。
	senderFund := int64(n) * 2
	if senderFund < 1000 {
		senderFund = 1000
	}
	genesisPre := types.GenesisAlloc{
		l1BlockAddr:       {Balance: big.NewInt(0), Nonce: 1, Code: l1BlockCodeFor(topFork), Storage: fp.l1BlockStorage(topFork)},
		messagePasserAddr: {Balance: big.NewInt(0), Nonce: 1, Code: realMessagePasserCode},
		senderAddr:        {Balance: eth(senderFund)},
	}
	if canyonActivated {
		genesisPre[canyonCreate2DeployerAddr] = types.Account{
			Balance: big.NewInt(0), Nonce: 0, Code: ladderCreate2DeployerCode,
		}
	}
	// Jovian requires a non-nil minBaseFee in EncodeOptimismExtraData (see
	// processChainPair for the panic rationale). The smoke (regolith->canyon)
	// exercises the nil path; the full 8-fork ladder needs the pointer set NOW
	// so Task 6 cannot panic.
	var minBaseFee *uint64
	knobs := genesisKnobs{
		Timestamp:          math.HexOrDecimal64(ladderGenesisTime),
		GasLimit:           math.HexOrDecimal64(gasLimit),
		BaseFee:            hdu(1_000_000_000),
		EIP1559Denominator: math.HexOrDecimal64(denom),
		EIP1559Elasticity:  math.HexOrDecimal64(elasticity),
	}
	if topFork == "jovian" {
		v := uint64(0)
		minBaseFee = &v
		knobs.MinBaseFee = hd64(0)
	}
	genesis := &core.Genesis{
		Config:     cfg,
		Timestamp:  ladderGenesisTime,
		GasLimit:   gasLimit,
		BaseFee:    big.NewInt(1_000_000_000),
		Difficulty: big.NewInt(0),
		ExtraData:  eip1559.EncodeOptimismExtraData(cfg, ladderGenesisTime, denom, elasticity, minBaseFee),
		Alloc:      genesisPre,
	}

	engine := beacon.New(ethash.NewFaker())
	var (
		db          ethdb.Database
		blocks      []*types.Block
		receiptsAll []types.Receipts
	)
	ins := make([]*inputCase, n)
	txSets := make([][]*types.Transaction, n)
	outSets := make([][]json.RawMessage, n)
	forkPath := make([]string, len(spec.Activations))
	for i, a := range spec.Activations {
		forkPath[i] = a.Fork
	}
	withdrawalCount := 0 // per-generation 1-based withdrawal counter (withdrawalSlots k)
	if err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("GenerateChainWithGenesis panic: %v", r)
			}
		}()
		db, blocks, receiptsAll = core.GenerateChainWithGenesis(genesis, engine, n, func(i int, bg *core.BlockGen) {
			blockTime := bg.Timestamp() // chain_makers: parent.Time()+10, real per-block advance
			blockExtra := eip1559.EncodeOptimismExtraData(cfg, blockTime, denom, elasticity, minBaseFee)
			bg.SetCoinbase(sequencerVault)
			bg.SetExtra(blockExtra)
			// Pre-Cancun headers must carry NO beacon root (verifyHeader rejects
			// a non-nil one there) -- unlike chainN (isthmus/jovian only), the
			// ladder starts pre-Cancun, so gate on the block's own time, the
			// same way processBlockVector gates the single-block generation.
			if cfg.IsCancun(bg.Number(), blockTime) {
				bg.SetParentBeaconRoot(beaconRoot)
			}
			signer := bg.Signer() // == MakeSigner(cfg, bg.Number(), bg.Timestamp())

			// L1-attributes calldata layout = the PARENT block's fork
			// (blockTime-10 == parent time): activation blocks carry the OLD
			// layout because the L1Block upgrade lands later IN the block.
			layoutFork := blockFork(cfg, blockTime-ladderBlockInterval)
			attr := fp.attributesTx(fmt.Sprintf("ladder_%d_%d", n, i), layoutFork)

			in := &inputCase{
				Info: caseInfo{
					Hardfork: blockFork(cfg, blockTime), // the BLOCK's own fork
					Description: fmt.Sprintf("ladder chain of %d blocks (%s), block %d/%d",
						n, strings.Join(forkPath, "->"), i+1, n),
				},
				Genesis:               knobs,
				Coinbase:              sequencerVault,
				ParentBeaconBlockRoot: beaconRoot,
			}
			if i == 0 {
				in.Pre = genesisPre // block i>0's Pre is filled AFTER assembly, from i-1's postState
			}
			var txs []*types.Transaction
			var outTxs []json.RawMessage
			// add builds + injects one tx, keeping ins[i].Transactions in EXACT
			// sync with bg.AddTx order (the vector's tx list must match the block).
			add := func(it inputTx) {
				tx, out, terr := buildTx(&it, signer, cfg)
				if terr != nil {
					panic(fmt.Errorf("block %d %s tx: %w", i, it.OpType, terr))
				}
				bg.AddTx(tx)
				in.Transactions = append(in.Transactions, it)
				txs = append(txs, tx)
				outTxs = append(outTxs, out)
			}

			// Order (assertDepositsFirst): attributes deposit FIRST, then user
			// deposits, then non-deposit txs.
			add(attr)

			blockForkName := blockFork(cfg, blockTime)
			// Jovian activation block must be deposits-only (op-geth
			// core/types/rollup_cost.go:571-576); assertL1BlockConsistency
			// (main.go:3851-3855) enforces the same. Jovian is the only fork
			// with such a constraint (checked against the pin; see report).
			isForkActivation := cfg.JovianTime != nil && blockTime == *cfg.JovianTime
			recipe := recipeFor(blockForkName, i, isForkActivation)

			for d := 0; d < recipe.deposit; d++ {
				add(ladderUserDeposit(fmt.Sprintf("ladder_dep_%d_%d", i, d)))
			}

			// Existing per-block sender transfer; nonce read live (chained).
			// It is a NON-deposit tx, so it must be suppressed on a
			// deposits-only activation block (Jovian, the only fork for which
			// isForkActivation is ever true -- see recipeFor). Without this the
			// Jovian activation block would carry a transfer and op-geth's
			// CalcDAFootprint / assertL1BlockConsistency would reject it.
			if !isForkActivation {
				add(transferTx(1, bg.TxNonce(senderAddr), recA, eth(1), 21_000, nil))
			}

			// Recipe non-deposit txs: every nonce is read at injection time via
			// bg.TxNonce(senderAddr) so the key-1 txs chain 0,1,2,...
			for w := 0; w < recipe.withdrawal; w++ {
				withdrawalCount++
				add(withdrawalTx(bg.TxNonce(senderAddr)))
				slotA, slotB := withdrawalSlots(withdrawalCount)
				if in.ExtraStorage == nil {
					in.ExtraStorage = map[common.Address][]common.Hash{}
				}
				in.ExtraStorage[messagePasserAddr] = append(in.ExtraStorage[messagePasserAddr], slotA, slotB)
			}
			if recipe.create {
				createNonce := bg.TxNonce(senderAddr)
				created := crypto.CreateAddress(senderAddr, createNonce)
				add(createTx(1, createNonce, 200_000, ladderCreateInitCode))
				if in.ExtraStorage == nil {
					in.ExtraStorage = map[common.Address][]common.Hash{}
				}
				// The created account's init phase writes slot 0; emitPostState
				// hard-fails on undeclared slots.
				in.ExtraStorage[created] = append(in.ExtraStorage[created], common.Hash{})
			}
			for _, p := range recipe.precompiles {
				add(precompileCallTxNonce(1, bg.TxNonce(senderAddr), p.addr.Bytes(), p.input, p.gas, 0))
			}

			ins[i] = in
			txSets[i] = txs
			outSets[i] = outTxs
		})
		return nil
	}(); err != nil {
		return nil, err
	}
	if len(blocks) != n {
		return nil, fmt.Errorf("generateLadderChain: expected %d generated blocks, got %d", n, len(blocks))
	}

	// Self-check: independent fresh DB, Process+ValidateState over ALL n blocks
	// in sequence -- real parent-child header linkage across the fork boundary.
	if err := selfCheck(genesis, blocks); err != nil {
		return nil, fmt.Errorf("ladder InsertChain self-check FAILED: %w", err)
	}

	out := &chainOutput{Blocks: make([]chainBlockOutput, n)}
	for i := range blocks {
		// Block i's pre IS block i-1's post-state (Decision A2 generalized).
		if i > 0 {
			ins[i].Pre = out.Blocks[i-1].PostState
		}
		// NOTE: assertL1BlockConsistency derives the block time from
		// in.Genesis.Timestamp+10 (main.go:3754), so it judges every block by
		// the FIRST block's fork/layout -- i.e. it assumes a layout-uniform
		// chain. That is exactly what the forkLayout guard at the top of this
		// function guarantees. If a cross-family chain is ever supported, pass
		// an explicit block time parameter instead of overloading
		// knobs.Timestamp (that field is the chain's genesis timestamp,
		// main.go:218-221).
		if err := assertL1BlockConsistency(cfg, ins[i]); err != nil {
			return nil, fmt.Errorf("block %d: %w", i, err)
		}
		if err := assertDepositsFirst(ins[i].Transactions); err != nil {
			return nil, fmt.Errorf("block %d: %w", i, err)
		}
		signer := types.MakeSigner(cfg, blocks[i].Number(), blocks[i].Time())
		var startRoot common.Hash
		if i == 0 {
			startRoot = genesis.ToBlock().Root()
		} else {
			startRoot = blocks[i-1].Root() // replay origin = previous post-state
		}
		o, _, err := assembleOutput(ins[i], cfg, signer, genesis, db, blocks[i], receiptsAll[i], txSets[i], outSets[i], startRoot)
		if err != nil {
			return nil, fmt.Errorf("ladder block %d: %w", i, err)
		}
		var pre *map[common.Address]outputAccount
		if i == 0 {
			p := emitPre(ins[i].Pre)
			pre = &p
		}
		out.Blocks[i] = chainBlockOutput{
			Info: o.Info, Env: o.Env, Pre: pre, Block: o.Block,
			PostState: o.PostState, OpExpected: o.OpExpected,
		}
	}
	return out, nil
}

// generateLadderChainSampled = generateLadderChain + 导出层采样。内部 postState
// 全量保留（块间 Pre 链与 assertL1BlockConsistency 依赖前块 postState），
// 只在导出前把未采样块置 nil。采样集只以 out.SampledBlocks 为唯一真相。
func generateLadderChainSampled(spec ladderSpec, n int) (*chainOutput, error) {
	out, err := generateLadderChain(spec, n)
	if err != nil {
		return nil, err
	}
	// ladderActivation.Block 是 1-based 块号（Timestamp = ladderGenesisTime +
	// ladderBlockInterval*Block），而 out.Blocks 是 0-based 索引：block i 的
	// 时间是 genesis+10*(i+1)，故激活块 Block 对应索引 Block-1。
	var activationIdxs []int
	for _, a := range spec.Activations {
		if a.Block > 0 {
			activationIdxs = append(activationIdxs, a.Block-1)
		}
	}
	var sampled []int
	for i := range out.Blocks {
		if shouldEmitPostState(i, n, activationIdxs) {
			sampled = append(sampled, i)
		} else {
			out.Blocks[i].PostState = nil
		}
	}
	out.SampledBlocks = sampled
	return out, nil
}

// runLadderMode 生成一条 ladder 向量并落盘（向量 + SHA256SUMS）。幂等：同参数
// 两次执行逐字节相同（json.Marshal 对 map 按键排序；签名/源哈希/槽位全部确定性）。
func runLadderMode(outDir, ladderFlag string, blocks int, opGethCommit string) error {
	if outDir == "" {
		return fmt.Errorf("--out-dir is required for --mode=ladder")
	}
	spec, err := parseLadderFlag(ladderFlag, blocks)
	if err != nil {
		return err
	}
	out, err := generateLadderChainSampled(spec, blocks)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	stem := fmt.Sprintf("ladder_%d", blocks)
	if err := writeChainVector(outDir, stem, out, opGethCommit); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(outDir, stem+".json"))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	line := fmt.Sprintf("%x  %s.json\n", sum, stem)
	return os.WriteFile(filepath.Join(outDir, "SHA256SUMS"), []byte(line), 0o644)
}
