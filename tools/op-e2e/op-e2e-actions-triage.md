# op-e2e actions triage — 128 项全量

日期：2026-09-17 · 方法：sequencer（3）与 upgrades/holocene（3）逐行精读；upgrades 其余 / sync / derivation / batcher 读测试函数骨架 + 关键断言；interop / proofs / altda / helpers / 其他按名字与已知范围归类（上游基础设施或不适用）。

判定规则：
- **owner=EL**：断言落在 engine 的 payload 接收/状态语义（含错误码语义、原子性、激活边界）。
- **owner=op-node / batcher**：断言落在派生管线 / 批次格式 / 提交策略（FISCO 原样运行它们，C2 隐式覆盖）。
- **owner=L1**：断言落在 L1 侧或争议游戏（proofs 主干由 C2 contest 覆盖）。
- **carrier**：`C2-late-fork` / `in-repo-RPC` / `vector` / `不移植`（含理由）。
- **lane 约束**：FISCO OP lane 基线为 Isthmus（引擎 -38005 门只收 Isthmus+ payload）→ pre-Isthmus 激活场景不可表达，等价物取"Isthmus 基线 + 更高 fork 晚激活"。

## A. 精读部分

| test | category | owner | semantics | applicable | coverage | carrier | risk |
|---|---|---|---|---|---|---|---|
| sequencer/TestL2Sequencer_SequencerDrift | sequencer | op-node+EL | drift 窗口耗尽后 op-node 续链但属性强制 noTxPool；**EL 义务=接受 deposits-only/空属性并照常出块**（`engine.EngineApi.ForcedEmpty()`）；另断言 L1 origin 采纳时机（时间对齐即换 origin） | partial——drift 判定在 op-node；EL 侧=空属性接受 | 仓内（noTxPool 属性有用例）；drift 边界无 | in-repo-RPC（空属性接受）+ C2 隐式 | H |
| sequencer/TestL2Sequencer_SequencerOnlyReorg | sequencer | op-node | sequencer 的 unsafe 链带被 reorg 掉的 L1 origin；verifier 派生检测不一致后整链回退重建 | N——op-node 派生 reorg 逻辑；EL 仅被动接受重建 | C2 隐式（真实 op-node） | 不移植 | M |
| sequencer/TestL2SequencerAPI | sequencer | op-node | op-node 新 sequencer API（OpenBlock/SealBlock/CommitBlock/PublishBlock/CancelBlock） | N——FISCO 不实现该 API 栈 | 无 | 不移植 | L |
| upgrades/TestHoloceneActivationAtGenesis | upgrades | EL | Holocene-at-genesis：extraData/1559 参数从创世生效，派生阶段激活日志 | Y（等价物=Isthmus/Jovian-at-genesis） | **C2（run6/run8，canonical=0:jovian）** | 不移植（已覆盖） | M |
| upgrades/TestHoloceneLateActivationAndReset | upgrades | op-node+EL | Holocene 偏移 24 激活：pre-Holocene 阶段运转→L1 inclusion 越过激活点→派生阶段转换+FrameQueue 重置→verifier/seq 先后转换→**L2 块跨激活边界（激活块 deposits-only、extraData 形态切换）**；reorg 后重复激活幂等 | partial——阶段转换在 op-node；**EL 义务=接受跨激活边界的属性序列与激活块形态** | 无（两轮 C2 均genesis 即生效） | **C2-late-fork**（Isthmus 基线+Jovian 晚激活等价；Holocene 本体低于 lane 基线不可表达） | **H** |
| upgrades/TestHoloceneInvalidPayload | upgrades | op-node+EL | batcher 缓存块中签名清零→派生失败→**块被 deposit-only 版本替换**（EL 义务=接受替换块、unsafe 链 reorg、在替换链上继续建块） | partial——替换决策在 op-node；EL 义务三点 | 仓内部分（deposits-only） | **in-repo-RPC**（替换块接受+续建） | H |

## B. upgrades 其余（骨架+关键断言）

| test | owner | semantics | applicable | coverage | carrier | risk |
|---|---|---|---|---|---|---|
| TestDencunL1ForkAfterGenesis / AtGenesis | L1 | L1 侧 Cancun 激活时序 | N（L1=anvil，外部） | — | 不移植 | L |
| TestDencunL2ForkAfterGenesis / AtGenesis | EL | L2 Cancun(Dencun) 按 L2 时间激活 | N——Cancun 低于 lane 基线 Isthmus | — | 不移植（lane 约束） | M |
| TestDencunBlobTxRPC / InTxPool / Inclusion（族） | EL | L2 上 blob 事务的 RPC/池/包含 | **待确认**——FISCO 4844 EL 支持面未定（开放项） | 无 | 不移植（本批），留探针 | M |
| TestEcotoneNetworkUpgradeTransactions | EL | Ecotone 升级交易（0x..dead 等 6 笔）执行 | Y | **向量（ladder 链已验 6/3/8/5 笔升级交易）** | 不移植（已覆盖） | M |
| TestEcotoneBeforeL1 | EL+op-node | L2 Ecotone 已激活而 L1 尚无 Dencun：blob fee 回退路径 | partial——可作 L1Block 定价回退探针 | 无 | vector 或 in-repo（低优先） | M |
| TestFjordNetworkUpgradeTransactions | EL | Fjord 升级交易（3 笔） | Y | 向量（ladder） | 不移植（已覆盖） | M |
| span_batch 族 7 项（DropSpanBatchBeforeHardfork / HardforkMiddleOfSpanBatch / AcceptSingularBatchAfterHardfork / MixOfBatchesAfterHardfork / SpanBatchEmptyChain / SpanBatchLowThroughputChain / BatchEquivalence） | batcher+op-node | span 批格式/硬分叉门控/等价性 | N——批次解析在 op-node；EL 仅执行结果块 | C2 隐式（真实 batcher 跑 span 批） | 不移植 | L-M |

## C. sync（12）

| test | owner | semantics | applicable | coverage | carrier | risk |
|---|---|---|---|---|---|---|
| TestSyncBatchType / TestUnsafeSync | op-node | 同步/gossip 不安全块 | N | C2 隐式 | 不移植 | L |
| TestBackupUnsafe / TestBackupUnsafeReorgForkChoiceNotInputError | op-node | backup-unsafe 状态机 | N | — | 不移植 | M |
| **TestBackupUnsafeReorgForkChoiceInputError** | op-node+**EL** | 注入 EL FCU 错误测 op-node 韧性；**EL 义务=FCU 错误码语义（invalid 参数 vs 内部错误可区分）** | partial——错误码语义是 EL 合规面 | 无 | **in-repo-RPC**（错误码断言） | M |
| TestELSync / TransitionstoCL / TransitionsToCLSyncAfterNodeRestart / ForcedELSyncCLAfterNodeRestart（族 4） | EL(v2 lane)+op-node | op-node 经 EL 同步源拉块、CL 切换 | partial——对应 FISCO ethereum-executor（v2）lane | 无 | **单列**（独立于 OP lane 主线，后续批） | M |
| **TestInvalidPayloadInSpanBatch** | op-node+**EL** | 批内坏交易→**EL 对含无效交易的 payload 报 INVALID/执行错误**→op-node 以 deposit-only 替换 | partial——**EL 拒绝语义为核心** | 无 | **in-repo-RPC** | **H** |
| TestSpanBatchAtomicity_Consolidation / _ForceAdvance | op-node | span 批原子性（整体应用或不应用） | N——派生管线内部 | C2 隐式 | 不移植 | M |

## D. derivation（5）与 batcher（4）

| test | owner | semantics | applicable | carrier | risk |
|---|---|---|---|---|---|
| TestL2Verifier_SequenceWindow | op-node | seq 窗口耗尽强制换 L1 origin（空 L2 块推进） | partial——EL 侧=接受空块（同 drift 族） | 并入 drift 场景 | M |
| TestDeriveChainFromNearL1Genesis | op-node | 近 genesis 派生 | Y | C2 隐式（启动即此场景） | L |
| TestBlockTimeBatchType / TestReorgBatchType / TestSystemConfigBatchType | op-node/batcher | 批型变体的派生/reorg/SystemConfig 更新 | N（SystemConfig 更新另有 FISCO overlay 语义） | 不移植 | L |
| TestL2BatcherBatchType | batcher | 提交策略 | N | 不移植 | L |
| TestEIP4844DataAvailability / MultiBlobs / DataAvailabilitySwitch（族 3） | batcher+L1 | 批数据走 L1 blob | N——batcher/L1 侧 | 不移植 | L |

## E. 不适用类（按范围归类）

| 类别 | 数量 | 理由 |
|---|---|---|
| interop | 37 | FISCO 未实现 interop |
| proofs | 31 | L1 争议游戏内部；主干由 C2 contest（DEFENDER_WINS/CHALLENGER_WINS/InvalidOutputRootProof）覆盖 |
| altda | 6 | 无 Alt-DA |
| helpers | 7 | 上游测试设施自检 |
| proposer / safedb / supernode | 3 | 外部组件不在 FISCO 范围 |

合计：3+20+12+5+4+6+37+31+7+3 = **128** ✓

## Selection（Phase 2 定稿）

以**用例粒度**（非上游函数粒度）计 12 项，源自 6 个上游测试函数；EL-owned 未移植义务经 triage 确认为约 6 组（少于设计预估的 20-25——多数 upgrades/sync 项归属 op-node/batcher 或已被覆盖，如实下调）：

| # | 场景（用例粒度） | 上游出处 | 载体 | 验收判据 |
|---|---|---|---|---|
| S1 | Jovian 晚激活：激活块 deposits-only、激活后 extraData 17B、跨边界续块 | TestHoloceneLateActivationAndReset | C2-late-fork | 激活块无用户交易；激活后首块 extraData=0x01…；链继续推进 |
| S2 | drift/seq 窗口耗尽：noTxPool 属性被引擎接受并出空块 | TestL2Sequencer_SequencerDrift + TestL2Verifier_SequenceWindow | in-repo-RPC | noTxPool FCU→VALID；产出块仅 deposit |
| S3 | 含无效签名交易的 payload → INVALID，latestValidHash=父 | TestInvalidPayloadInSpanBatch（EL 侧） | in-repo-RPC | status=Invalid 且父链保持 canonical |
| S4 | deposit-only 替换块被接受 + 在替换链上续建 | TestHoloceneInvalidPayload（EL 侧） | in-repo-RPC | 替换 payload VALID；其上再建一块 VALID |
| S5 | FCU invalid 参数错误码 vs 内部错误可区分 | TestBackupUnsafeReorgForkChoiceInputError | in-repo-RPC | 两类输入得到不同错误码/状态 |
| S6 | 属性时间戳不严格递增 → INVALID（父不受扰） | TestHoloceneInvalidPayload 邻近语义 | in-repo-RPC | INVALID + 父 canonical + 合法后续 VALID |
| S7 | pre-Holocene 父块携带非空 extraData → 拒/降级语义受控 | 同上族 | in-repo-RPC | 按布局规则拒绝并留名 |
| S8-S9 | 激活边界两侧 1559 参数切换（Holocene 形态进入后从 extraData 取参） | TestHoloceneLateActivationAndReset | in-repo-RPC | 边界前用链参数、后用 extraData 解码值 |
| S10 | L1Block 定价回退（L1 尚无对应数据时的 fee 路径） | TestEcotoneBeforeL1（等价） | vector（低优先，可选） | 与 op-geth 引用值一致 |
| S11 | （探针，预期可能按设计失败）pre-Isthmus 基线 canonical | lane 约束验证 | 手工探针 | -38005 拒绝则记录为 lane 约束证据 |
| S12 | （单列后续批）ELSync 族 → v2 lane 同步语义 | TestELSync 族 | 独立 | 另行立项 |

blob/4844（TestDencunBlobTx 族）保持开放项：待 FISCO 4844 EL 支持面确认后决定入批与否。
