# devnet fork-offset 机制定案实验（P1-1）

状态：**已完成（2026-09-14）**。结论：**post-edit 路径成立** —— op-deployer 部署产物（rollup.json / genesis.json）中的 fork 时间为阶梯偏移时，devnet 短链可真实穿越全部 fork 边界，op-node 在每个激活块按源码预期注入升级交易（Ecotone 6 / Fjord 3 / Isthmus 8 / Jovian 5，其余 0），L1-attributes calldata 布局随 fork 演进（260B→164B→176B→178B），batcher 生效（safe 头追上 unsafe）。

环境（`tools/devnet/versions.lock`）：
- geth：op-geth @ `e8800cffe`（upstream 1.17.2），二进制 `/tmp/d3-bin/geth`
- op-node / op-batcher：`/Users/octopus/octo/code/optimism-d3-precheck` @ `76e4fad5`
- op-deployer：`/tmp/op-deployer-d3`（同一 worktree 构建）
- anvil 1.7.1；L1 chain-id 31337，L2 chain-id 901

## 定案结论（含对任务假设的实测修正）

1. **fork 时间基准修正**：fork 激活时间 = **apply 时刻 op-deployer 记录的 L1 起始块时间戳**（rollup.json 的 `genesis.l2_time`，本实验第 1 次部署 = L1 block 43 ts，第 3 次 = L1 block 26 ts），**不是 L1 genesis 时间**。全部 8 个偏移相对该基准精确成立（l2_time+1250/1875/2500/3750/5000/6250/7500/8750）。任务书中「基准=L1 genesis（genesis.go:47）」的说法被实测否定。
2. **本 worktree 存在非标准 fork「Delta」（Canyon 与 Ecotone 之间）**：
   - op-deployer intent 必须显式给 `l2GenesisDeltaTimeOffset`（不给则默认 0，inspect 校验报 `fork delta set to 0, but prior fork canyon has higher offset`；与 Canyon 相等也被拒：`Forks in general cannot activate at the same post-Genesis block`）。本实验取 1875。
   - Delta 激活**不注入任何升级交易**（attributes.go 无 Delta 分支），且 geth 的 chain config 里没有 deltaTime（为 null）。
   - **post-edit 注意**：手改 rollup.json 时 delta_time 不能省略 —— `op-node/rollup/types.go checkFork(DeltaTime, EcotoneTime)` 在 delta=nil 且 ecotone 已设时报 `fork ecotone set (to X), but prior fork delta missing`。
3. **升级交易注入计数与源码完全一致**（`op-node/rollup/derive/attributes.go`）：
   - Ecotone 6 / Fjord 3 / Isthmus 8 / Jovian 5（Jovian = DAFootprint 3 + OperatorFeeFix 2）；
   - Canyon / Delta / Granite / Holocene = 0；
   - genesis 处激活不注入（本实验所有 fork 均在 genesis 后激活，未测 genesis 激活分支）。
4. **L1-attributes calldata 布局（实测 + 源码印证）**：布局切换发生在激活块的**下一个块**（`is*ButNotFirstBlock`：激活块本身仍用父 fork 布局，`l1_block_info.go:450-464`）：
   - Bedrock `setL1BlockValues` 0x015d8eb9 = 260B（至 Ecotone 激活块 1250 为止）
   - Ecotone `setL1BlockValuesEcotone` 0x440a5e20 = 164B（1251 起）
   - Isthmus `setL1BlockValuesIsthmus` 0x098999be = 176B（3751 起）
   - Jovian `setL1BlockValuesJovian` 0x3db6be2b = 178B（4376 起）
   - 任务书「Bedrock 260B → Ecotone 164B → Isthmus 176B → Jovian 178B」的四个数值正确，但「激活块即新布局」不成立 —— 激活块仍是父布局。
5. **升级交易形态实测**：deposit(0x7E)、非 L1-info（L1-info 由 from=0xdead…0001、to=L1Block 预部署 0x420…0015 识别）。逐笔与源码对应（deploy bytecode to=nil；代理升级 to=预部署 proxy、selector 0x3659cfe6 upgradeToAndCall；GPO 启用 0x22b90ab3 setEcotone / 0x8e98b106 setFjord / 0x291b0383 setIsthmus 等）。
6. **batcher 生效**：safe 头从 0 派生追至距 unsafe ≤ 15 块并同步推进；升级后的链上状态可验证（GasPriceOracle.isEcotone()=true、OperatorFeeVault.version()=1.0.0、L1Block.blobBaseFee() 可调）。

## 加速机制的真相（本次实测的关键方法学结论）

- op-node sequencer 的 L2 时间以**墙钟**为节拍（`sequencer.go timeNow=time.Now`，追到 `now+drift` 即匀速），**只把 L1 头时间当地板（origin 约束）**。因此：
  - 单纯 anvil RPC 跳时钟（`anvil_setBlockTimestampInterval`）**不能**加速 L2 穿越边界 —— L2 时间仍按墙钟走，8750s 阶梯需 ~2.4h 真实等待。
  - **有效组合**：① anvil `--timestamp <now-9000>` 让 L1 genesis 落在过去；② apply 后用 `anvil_setBlockTimestampInterval(1000)` 跳几个块把 **L1 头时间**推过最后一条边界再恢复 interval=2；③ 启动 L2 —— sequencer 从过去的 L2 genesis 以**机器速度**（实测 ~115 blocks/s）追赶墙钟，途中把全部边界依次穿越，总耗时 ~40s。
  - 该方式保持任务书规定的偏移数值不变（1250…8750），且逐边界真实穿越。

## 实测命令序列（第 3 次 run，全链路可复现）

```bash
# L1（genesis 时间放过去 9000s，给 sequencer 机器速度追赶留窗口）
TS0=$(date +%s)
anvil --chain-id 31337 --block-time 2 --timestamp $((TS0-9000)) >&LOGS/anvil.log &
# L1_GENESIS_TS=1789375966

# op-deployer（workdir=/tmp/d3-deployer）
/tmp/op-deployer-d3 init --l1-chain-id 31337 --l2-chain-ids 901 --intent-type custom --workdir /tmp/d3-deployer
# 编辑 intent.toml：填满全部非零角色/recipient（validateCustomConfig 走 CheckNoZeroAddresses），
# 删除 init 生成的 [chains.customGasToken]（空 Name 会 fail：CustomGasToken.Name must be set），
# contractsLocator 用 file:///…/packages/contracts-bedrock（需先 forge build 生成 forge-artifacts，见下），
# [globalDeployOverrides] 偏移用 0x 引号字符串：
#   canyon 0x4e2(1250) delta 0x753(1875) ecotone 0x9c4(2500) fjord 0xea6(3750)
#   granite 0x1388(5000) holocene 0x186a(6250) isthmus 0x1d4c(7500) jovian 0x222e(8750)
/tmp/op-deployer-d3 apply --workdir /tmp/d3-deployer --l1-rpc-url http://127.0.0.1:8545 \
  --private-key 0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80
/tmp/op-deployer-d3 inspect genesis --workdir /tmp/d3-deployer 901 > artifacts/genesis.json
/tmp/op-deployer-d3 inspect rollup  --workdir /tmp/d3-deployer 901 > artifacts/rollup.json
# 实测：inspect 的 --outfile 标志无效（输出走 stdout）；2>/dev/null 以外的行都是 JSON

# 把 L1 头时间推过 jovian 边界（l2_time+8750）后恢复 2s
cast rpc anvil_setBlockTimestampInterval 1000 --rpc-url http://127.0.0.1:8545
sleep 12 && cast rpc anvil_setBlockTimestampInterval 2 --rpc-url http://127.0.0.1:8545

# L2
geth --datadir l2datadir init artifacts/genesis.json
geth --datadir l2datadir --http --http.port 9545 --http.api eth,debug,net,web3 \
  --authrpc.port 8551 --authrpc.jwtsecret jwt.txt --gcmode archive --syncmode full \
  --nodiscover --port 0 --rollup.disabletxpoolgossip --rollup.sequencerhttp http://127.0.0.1:9546 &
op-node --l2 http://127.0.0.1:8551 --l2.jwt-secret jwt.txt --l1 http://127.0.0.1:8545 \
  --l1.beacon.ignore --rollup.l1-chain-config l1-chain-config.json \
  --sequencer.enabled --sequencer.l1-confs 0 --rollup.config artifacts/rollup.json --rpc.port 9546 &
op-batcher --l1-eth-rpc http://127.0.0.1:8545 --l2-eth-rpc http://127.0.0.1:9545 \
  --rollup-rpc http://127.0.0.1:9546 --private-key <batcher 即 anvil acct#1 的 key> \
  --max-channel-duration 2 --data-availability-type calldata \
  --throttle.unsafe-da-bytes-lower-threshold 0 --rpc.port 8548 &
```

### 踩坑记录（每条都真实消耗了时间，复现时必读）

| 坑 | 现象 | 解法 |
|---|---|---|
| forge 构建失败 | `forge build` exit=1 但只打印 Warning（`deny_warnings = true`，foundry 新键 `deny="warnings"`，`FOUNDRY_DENY_WARNINGS` 不生效） | `FOUNDRY_DENY=false forge build --force --skip "/**/test/**"`（实测 deny=false→deny="never"） |
| forge 构建失败的假象 | `forge build … \| tail -2; echo $?` 返回 0 —— `$?` 拿到的是 tail 的退出码 | 管道后用 `PIPESTATUS` 或直接重定向再取 `$?` |
| apply 报缺 artifact | `failed to open artifact "DeployImplementations.s.sol/…" / "SuperchainConfig.sol/…"` | forge-artifacts 不完整：上条解法全量重建（366 个 artifact 目录） |
| intent 校验 1 | `fork delta set to 0, but prior fork canyon has higher offset 1250` | intent 显式加 `l2GenesisDeltaTimeOffset` |
| intent 校验 2 | `both fork canyon and delta are set to 1250: Forks … cannot activate at the same post-Genesis block` | delta 取严格介于两者之间（1875） |
| intent 校验 3 | `CustomGasToken.Name must be set when using custom gas token` | 删除 init 生成的 `[chains.customGasToken]` 段 |
| op-node 启动 1 | `failed to read chain spec: open : no such file or directory` | L1 chain-id 31337 不在已知注册表，需 `--rollup.l1-chain-config <anvil 的 ChainConfig JSON>`（须含 blobSchedule，见 artifacts/l1-chain-config.json） |
| op-node 启动 2 | `flag provided but not defined: -l1.chain-config` | 正确旗标是 `--rollup.l1-chain-config` |
| op-node 运行 | `engine_forkchoiceUpdatedV3 does not exist/is not available` 刷屏 | `--l2` 必须指向 **authrpc 8551**（engine 命名空间），不是 9545 的普通 HTTP；另注意 kill 残留进程后日志混淆的假象 |
| batcher 启动 | `miner_setMaxDASize unavailable … either enable it or disable throttling` | `--throttle.unsafe-da-bytes-lower-threshold 0`；另 `--rpc.port` 默认 8545 会与 anvil 冲突，改 8548 |

## Step 6 定案验证数据（第 3 次 run，l2_time=1789376018，block n ts = l2_time+2n）

| fork | 边界 ts | 激活块 # | 块 ts | 升级交易数 | 预期 | L1-info calldata |
|---|---|---|---|---|---|---|
| canyon | 1789377268 | 625 | 1789377268 | **0** | 0 | 260B (Bedrock) |
| delta | 1789377893 | 938 | 1789377894 | **0** | 0（无注入分支） | 260B |
| ecotone | 1789378518 | 1250 | 1789378518 | **6** | 6 | 260B（父布局） |
| fjord | 1789379768 | 1875 | 1789379768 | **3** | 3 | 164B |
| granite | 1789381018 | 2500 | 1789381018 | **0** | 0 | 164B |
| holocene | 1789382268 | 3125 | 1789382268 | **0** | 0 | 164B |
| isthmus | 1789383518 | 3750 | 1789383518 | **8** | 8 | 164B（父布局） |
| jovian | 1789384768 | 4375 | 1789384768 | **5** | 5 | 176B（父布局） |

- 前块校验：每个激活块的父块 ts 均严格 < 边界（脚本断言通过）。
- 布局切换点：1251→164B(0x440a5e20)、3751→176B(0x098999be)、4376→178B(0x3db6be2b)。
- 状态证明：`isEcotone()=true`、`baseFeeScalar()=0x558`、`blobBaseFee()=1`、OperatorFeeVault `version()=1.0.0`。
- sync：safe 4644 / unsafe 4659（safe 追上并随动）；batcher 通道正常出帧。

## post-edit 路径操作要点（给后续 devnet/语料管线）

1. rollup.json 手改：以 `genesis.l2_time` 为基准写阶梯 `*_time`；**必须包含 delta_time**（见定案结论 2）；所有 `*_time` 严格递增。
2. genesis.json 手改：`config` 里的 `canyonTime/ecotoneTime/fjordTime/graniteTime/holoceneTime/isthmusTime/jovianTime` 必须与 rollup.json 完全一致；`shanghaiTime=canyonTime`、`cancunTime=ecotoneTime`、`pragueTime=isthmusTime` 的映射保持；**没有 deltaTime 字段**。
3. 两份文件在 `geth init` 之前定稿即可 —— geth/op-node 均在启动时读取，链上无其他东西校验产物来源；本次实验虽由 deployer 生成，但机制上与手改等价（时间数值一致即等价）。
4. 若想省事：把 jovian 之后还想再穿的 fork（如 interop）留空即可（nil=禁用）；或者把全部边界放进过去 9000s 窗口 + L1 跳时钟，40 秒内穿完（见加速机制）。

---

# 配置驱动的 devnet 启动器 `opdevnet`（P1-2）

状态：**已完成（2026-09-14）**。Task 1 的实测流程固化为 `opdevnet.sh up|status|down|clean`，
全部参数来自 `devnet.toml` + `versions.lock`。验收：up 一条命令起全栈并自动穿越 8 个 fork 边界；
激活块计数 0/0/**6/3**/0/0/**8/5** 全命中；`down` 后重复 `up` 的 L2 genesis hash **完全一致**（幂等）；
`down` 后无残留进程/端口。

## 文件

- `devnet.toml` —— 唯一配置输入：fork 阶梯偏移（含 Delta 显式 1875）、链参数（chainId 901 /
  gasLimit / denominator/elasticity / minBaseFee / DA scalar）、加速参数、端口（anvil 8545 /
  geth 9545+8551+8552 / op-node 9546 / batcher 8548）、角色、固定 L1 genesis 时间戳与
  create2Salt、contracts-bedrock 路径。
- `versions.lock` —— 二进制路径（`path=` 字段）与 sha256 记录；脚本从 `path=` 解析二进制。
- `opdevnet.sh` —— up/status/down/clean。产物 `artifacts/`（genesis.json / rollup.json /
  l1-chain-config.json）与 `logs/` 在 down 后保留供导出；`clean` 才删运行时目录。
- `check_activations.py` —— 独立激活块检查：`--rollup artifacts/rollup.json --rpc
  http://127.0.0.1:9545`，逐 fork 找激活块、数 type-0x7E 升级交易并对照 attributes.go 预期
  （Ecotone 6 / Fjord 3 / Isthmus 8 / Jovian 5，其余 0），父块 ts 断言，退出码 0/1/2。

## 与 Task 1 流程的显式差异（全部为幂等服务，稳态行为等价）

1. **L1 genesis 时间戳固定为绝对值**（toml `l1.genesis_timestamp = 1789375966`，Task 1 第 3 次
   run 的值）。fork 绝对时间 = 该锚 + 阶梯偏移，不固定则两次 up 产物漂移。
2. **anvil 以 `--no-mining` 启动 + 手动矿**（Task 1 用 `--block-time 2`）。实测：
   - `evm_mine` **不读** `anvil_setBlockTimestampInterval`（那只作用于间隔矿 tick）；
   - `evm_setNextBlockTimestamp(T)` + `evm_mine` → 块 ts **精确 = T**（T 必须 > 父块 ts）；
   - 未再设置时下一块沿用上次值（不跳回墙钟）；
   - `evm_setIntervalMining 2` 可从无矿状态启用，tick 块 ts 恒 = parent+2（与 --block-time 等价）。
   流程：逐块精确矿到 `pin_block=26` 停住 → apply 期间代矿（parent+2）→ 跳时钟逐块 +1000 →
   `evm_setIntervalMining 2` 恢复稳态。
3. **apply 后产物归一化**（README 上节「post-edit 路径」的工具化）：deployer 的
   `set-start-block strategy=live` 在部署完成后才读 L1 头（apply 日志 stage 实证），起始块随代矿
   节奏漂移。脚本把 rollup/genesis 的 fork 时间整体平移到锚点
   `l2_time = 1789375966 + 2*26 = 1789376018`，同时：
   - **geth genesis 头部 `timestamp` 必须一并平移**（漏掉则整条 L2 阶梯相对 rollup 偏移，
     激活块全部错位——run7 实测）；
   - `rollup.genesis.l1` 重指到 pin_block（其 ts 恰 = 新 l2_time，保持推导起点构造一致）；
   - 归一化改变 genesis 内容 → genesis 块 hash 变化，rollup 的 `genesis.l2.hash` 由 **geth 链上
     block 0 回填**（不重新实现 RLP；不回填则 op-node 报 expected L2 genesis hash to match——run8 实测）。
4. **CREATE2 盐固定**（幂等头号杀手，run9/10/11 三跑三异后定位）：op-deployer `init` 把
   state.json 的 `create2Salt` 写成 0x0，`apply` 见零盐**随机生成**并持久化 →
   L1CrossDomainMessengerProxy 等 3 处 storage 引用的代理地址每次不同 → genesis hash 漂移。
   脚本在 init 后、apply 前把盐改写为 toml 固定值（sha256("d3-devnet-901")）。

## 实测记录（opdevnet 版本）

- up 全程 ~90s（apply ~25s + L2 机器速度追赶 ~30s + 检查）；激活块计数与 Task 1 表格逐格一致：
  canyon 625 块 0 笔 / delta 938 块 0 笔 / ecotone 1250 块 **6** 笔 / fjord 1875 块 **3** 笔 /
  granite 2500 块 0 笔 / holocene 3125 块 0 笔 / isthmus 3750 块 **8** 笔 / jovian 4375 块 **5** 笔。
- 幂等：`down` → `up` 两次，L2 genesis hash 均为 `0x43e8fa885515167608aba63bf5ee378ffe212b87acb5e862635585e679c4add7`，
  脚本输出 `IDEMPOTENT`。
- status：组件 pid/端口 + L2 unsafe 块高与 fork 段 + safe/unsafe 差；safe 以派生速度追赶 unsafe
  （batcher 生效，与 Task 1 行为一致）。
- 其他踩坑追加（全部已修并留断言）：
  - RPC 数值参数必须传 JSON 字符串 `"0x1a"`；裸 `0x1a` 不是合法 JSON，anvil 回 Invalid Request。
  - op-node RPC 没有 `eth_chainId`，探活用 `optimism_syncStatus`；该构建返回 snake_case 字段
    （`safe_l2`/`unsafe_l2`）。
  - bash `rpc()` 类 helper 传参遗漏会静默变成空 params（run5 教训：`evm_setIntervalMining []`）；
  - bash 后台启动勿写 `( cd X && nohup cmd ... & echo $! )`：`&&` 列表会被整体后台化，`$!` 记到
    外壳 subshell 的 pid，down 杀壳不杀真进程（孤儿实测）；须换行分隔语句；
  - 日志串里 `$var` 后紧跟全角字符（如 `$stuck（`）会被 bash 并进变量名，`set -u` 下报
    unbound variable 直接崩（down 的 WARN 分支首次执行时踩中）；`${var}` 花括号可解。

## 产物与日志

- 产物：`/tmp/d3-run/artifacts/{genesis.json,rollup.json,l1-chain-config.json}`；intent：`/tmp/d3-deployer/intent.toml`；state：`/tmp/d3-deployer/state.json`
- 日志：`/tmp/d3-run/logs/{anvil,geth,opnode,batcher,deployer-apply,forge-*}.log`
- 分析脚本：`/tmp/d3-run/analyze.py`（逐激活块计数）、`/tmp/d3-run/analyze2.py`（布局切换与升级交易明细）

---

# 可插拔交易源执行器 `txsource`（P1-3）

状态：**已完成（2026-09-14）**。交付目录 `tools/devnet/txsource/`（契约/用法/测试详见其
`txsource/README.md`）。runner = per-segment 调度（方案 A 全量轮）+ L1 资金桥 + settle；
单测用假 JSON-RPC 桩覆盖段边界/每段恰好一轮/激活块跳过/missed/settle 失败/插件失败/target
退出（18/18 绿）；真实 devnet 端到端跑通单段全通路（资金桥 L1→L2、插件 3 笔转账、报告、
safe 追平 unsafe）。

## 定案与实测发现

1. **段表权威 = rollup.json**：段 = bedrock（genesis.l2_time 起）+ 每个 `<fork>_time`；
   激活块号 = ⌈(fork_time − l2_time)/block_time⌉（delta 奇数偏移 1875 → 938）。不读
   devnet.toml 的 forks 表（那是输入，rollup.json 是生效值）。
2. **中途加入语义**：runner 启动头之前的段标记 missed 不补轮（链史已定型）；当前段照常
   触发一轮。本 devnet fork 绝对时间锚定 toml 固定时间戳，jovian 边界 = 墙钟约 11:19 UTC
   2026-09-14；`up` 完成时若已过该时刻则只能观察到 jovian 段（全段覆盖需 up 后立即接入
   runner，或桩演示多段语义）。
3. **资金桥必须走 L1 Portal 存款**：intent `fundDevAccounts = false` ⇒ anvil 账户 L2 余额
   全为 0（实测 key0/key9）。runner 每轮经 `OptimismPortal.depositTransaction` 存入确定性
   金额，轮询 L2 余额到账后才触发插件。
4. **Portal2 ABI 踩坑**：`depositTransaction` 的 gasLimit 参数是 **uint64**
   （selector `0xe9e05c42`）；按 uint256 编码（`0xfa92670c`）dispatcher 不识别，报空
   revert（`execution reverted, data "0x"`），gas 估算阶段就失败。
5. **op-geth `--rollup.sequencerhttp` 踩坑（重大，P1-3 发现并修正）**：op-geth 的
   `SendTx` 在该旗标存在时把用户 raw tx **转发**到指定端点（`eth/api_backend.go`）；
   P1-1/P1-2 的启动线把它指向 op-node(9546)，而 op-node 没有 `eth_sendRawTransaction`
   （-32601）→ 用户经 RPC 向 L2 发交易整条不可用（估算都过不去的是 depositTransaction L1
   侧；L2 侧 cast send 直接 -32601）。修正：geth 启动线删去该旗标（单 sequencer 语义 =
   交易进本地 txpool，由本节点 sequencer 出块），down/up 后 genesis hash 仍幂等。
   **不修则 Task 4 的 bcos-testing（hardhat 发 raw tx）无法工作。**
6. **settle 实测**：轮末快照 unsafe，op-node `optimism_syncStatus`（snake_case
   safe_l2/unsafe_l2）轮询 safe 追平，batcher 生效下数秒内完成。

---

# campaign 模式 + bcos-testing 交易源适配（P2 Task 4）

状态：**已完成（2026-09-14）**。`opdevnet.sh up --campaign` 起真实派生 devnet 且 fork 边界
按**墙钟**逐段到来；bcos-testing（Hardhat，19 测试文件）经 `txsource/plugins/bcos_testing/`
插件作为交易源接入 runner，逐段触发、交易入块、链级分布可核查。适配细节与踩坑全录见
`txsource/README.md` §7；本节只记 A（campaign）侧定案。

## A. `up --campaign [--forks "..."] [--segment-seconds N]`

问题：默认 up 的跳时钟策略把 L1 头时间一次性推过全部边界，L2 以机器速度追墙钟时瞬间穿完
8750s 阶梯 —— runner 启动时只剩最后一段可观察。campaign = 反其道：**链时间贴着墙钟走，
边界留给墙钟逐个穿越**。

实现方式：**生成临时 toml**（`/tmp/opdevnet-campaign.toml`，`OPDEVNET_CAMPAIGN_TOML` 可改），
基准 devnet.toml 一字不改（默认 up 的幂等语义完全不受影响；campaign 每次 up 重新锚定墙钟，
genesis hash 随墙钟漂移属预期，WARN 行如实报告）。派生时只改四处并做与基准的一致性自检
（runtime_dir/端口/chain-id 相同 → status/down 与 txsource run.sh 用缺省 devnet.toml 即可
操作 campaign 栈）：

1. `l1.genesis_timestamp = 墙钟 - 500s`（`CAMPAIGN_ANCHOR_OFFSET_S`）：l2_time = 锚 + 2·pin_block
   ≈ 墙钟 - 448s。up 本身耗时 ~110s，L2 追平墙钟后头块链时偏移 ≈ +480~580s →
   bedrock 段在 up 的追平阶段被结构性消耗（runner 标 missed），第一可观察段（canyon，
   窗口 ~120s）+ 后续 7 段全部可触发 —— 8 轮 8 段。
2. `[forks]` 压缩阶梯：第 i 个 fork 偏移 = i × `--segment-seconds`（缺省 300s）→
   8 边界跨 40min 链时间，全 9 段观察 ≤ 1h；`--forks "canyon delta ecotone"` 可取子集
   试点（intent/normalize/verify/status 走 `RUN_FORKS` 子集，delta=600 恒严格介于
   canyon=300 与 ecotone=900 之间，部署校验天然满足）。
3. `[accel].target_l2_blocks` 重算为末 fork 激活块 + 75（campaign catch-up 不用它，仅展示）。
4. `meta.name` 打标。

流程分支（`CAMPAIGN=1`）：**不做 L1 跳时钟**（anvil 稳态 2s interval 照旧，L1 头时间贴墙钟）
→ L2 catch-up 完成条件改为「头块时间进入墙钟 ±30s」（实测追平偏差 0s）→ **跳过激活块计数
检查**（边界未到，属预期；逐段覆盖由 runner 验证）。其余 pin/deployer/normalize/verify/
genesis-hash 回填全路径复用。

实测（2026-09-14 23:46，campaign #4，全 8 fork）：up 全程 ~34s；锚 = 生成时墙钟-500，
fork 绝对时间 = l2_time + {300,600,…,2400} 全部 OK（ARTIFACT VERIFY 全绿）；L2 catch-up
完成时头块时间追平墙钟（behind 0~1s）。

**campaign 链时间语义（实测修正）**：不跳时钟后，L2 时间以 **L1 头时间为地板**，而 L1
头时间以 2s/2s 从「墙钟-470s」的 genesis 逐块追赶 → L2 时间恒落后墙钟 ~440s，但速度仍是
2s/2s —— 每段窗口仍 = 300s 墙钟，边界按段序逐一穿越，只是整体后移。8 边界全穿完 ≈
40min 链时间 → **全段观察 ≈ 45min 墙钟，runner 需 `--run-timeout 3600`**（默认 1800s
会在第 7 段后被掐断；实测 jovian 段在同栈上补触发一轮即可，runner 中途加入语义原生支持）。

## B. bcos-testing 适配（要点；全文见 txsource/README.md §7）

- 套件 clone 至 `/tmp/bcos-testing`（`BCOS_TESTING_DIR` 可改，HTTPS 失败自动回落 SSH），
  上游文件零改动；bcos-testing HEAD = `f9b8338`（GitHub 公共仓）。
- **chainId 必须外部配置**：上游 bcosnet 硬编码 chainId=20200，hardhat
  `ChainIdValidatorProvider` 首个 RPC 即校验，devnet 901 由插件生成的 wrapper
  `--config` 提供（运行后清理，git 树零改动）。
- funding = anvil key0（与上游测试内硬编码默认私钥一致），实测 L2 余额 0 → 必须走
  runner L1 资金桥（key0 自存自，每轮 1 ether 足够，实测 gas ≈ 0）。
- **实测（campaign #4，2026-09-14 23:46–00:33，同一栈生命周期）**：8 段触发 8 轮
  （bedrock 段被 up 追平结构性消耗，runner 标 missed）；canyon/delta 两轮被
  pre-ecotone 闸门跳过（见下）；ecotone/fjord/granite/holocene/isthmus/jovian 六段
  全套件交易 **378 笔入块**（逐段 69/92/101/103/13/12，included 349、reverted 29，
  全部 eip1559）+ jovian 补轮 12/12；`check_distribution.py` 链级核查 = 用户交易分布
  6/9 段（exit 0）。断言失败为 FISCO 语义噪音（如 coinbase==0x0），与入块解耦呈现。
- **重大发现（fork 审计信号）**：本 op-geth 构建 pre-ecotone 段校验用户交易时，bedrock
  L1-cost 路径的 L1Block 槽位读取与部署的（jovian 代）L1Block 存储布局错位 →
  `panic("overflow in total rollup cost: l1Cost")`（rollup_cost.go:373）→ SendTx 崩溃且
  payload builder 死锁（链永久卡死）。插件对 bedrock/canyon/delta 三段设发送闸门
  （`tx_gate: "pre_ecotone_no_send"`），ecotone 起不受影响。详见 txsource/README.md §7.7。
- 运维踩坑：geth 磁余 <1.6GiB 会自杀（"Low disk space. Gracefully shutting down"），
  本机 APFS 可用空间波动大（快照/可清除空间），campaign 期间建议清理
  `~/Library/Caches/{vscode-cpptools,ccache,go-build,pip}` 并周期性
  `tmutil thinlocalsnapshots /`。

---

# chain-to-vector 导出器 `chainexport`（P3-1 Task 5）

状态：**已完成（2026-09-15）**。`tools/devnet/chainexport/` 把运行中的真实 devnet 链
导出为与 ladder 逐字段同构的 chain-mode 差分向量（schema v3-block），直接供
FISCO `OpT8nReplay` 试重放。导出面 100% 命中 ladder 的键集家族；试重放给出
3 族真实分歧（见下），既有语料全绿。

## 模块归属定案

- **拷入 op-geth 树构建**（与 `generator/regen.sh` 的 `cmd/opt8n-ref` 同一模式）：
  源码唯一真相 = 本目录 `main.go`（stdlib-only，无 op-geth import）；
  `build.sh` 把它拷进 `$OPGETH/cmd/chainexport` 用 op-geth 的 go.mod 编译，
  产物落回 `chainexport.bin`（.gitignore），临时目录即删——op-geth tracked 树保持干净。
  即便当前无 op-geth import 仍走该模式：① 与既有构建仪式一致；② 构建环境被
  pin（HEAD==e8800cffe、tracked 树干净）钉死，而 chainexport 消费的 RPC 字段
  形状正是该 pin 的 `internal/ethapi MarshalReceipt` / `txJSON` 暴露面。
- 用法：`bash build.sh && ./chainexport.bin --rpc http://127.0.0.1:9545
  --rollup /tmp/opdevnet/artifacts/rollup.json --out-dir <dir> [--poststate boundary|full]`。
  stdout 打 `DEVNET-STEM <stem>`（regen.sh 的 LADDER-STEM 惯例）与文件 SHA256。

## 重大前置修复（opdevnet.sh，随本提交）

**`--gcmode archive` 单独不再构成归档节点**（P3-1 实测）：本 geth（path state
scheme）的历史状态随机读只覆盖 recent diff-layer 窗口（实测恰 128 块：
head-129 的 `debug_accountRange`/`eth_getBalance` 即报 missing trie node /
historical state not available）；`--history.state 0` 只扩 history journal（日志
`state-history="entire chain"` 佐证），实测不解锁 RPC 随机读。修复 =
`--state.scheme hash`（`geth init` 与 `geth` 启动两处都要——init 缺省仍写
path 标记，漏改则启动报 `incompatible state scheme`）。经典 hash+archive 语义下
全史状态可查，chainexport 的任意历史块 postState 导出才成立。

## 导出实现要点（与 generator 逐字段对齐）

- header：`eth_getBlockByNumber(n,true)`；withdrawalsRoot/requestsHash/blobGasUsed
  按 RPC 存在性发射（nil→absent），与 ladder 各 fork 键集逐键一致。
- 回执：`eth_getBlockReceipts` 的 OP 专属字段全量映射；`_op_l1_fee_scalar` 仅当
  RPC 十进制串（scaled 值）为精确整数时发射 hex（generator 的 `big.Exact` 口径；
  devnet raw scalar 0x558 非 1e6 倍数 → pre-Ecotone 恒 absent）；`_op_operator_fee`
  仅当 scalar/constant 非零（devnet 全零 → 双侧 absent；公式已实现备用）；
  `_op_da_footprint`=jovian 回执 blobGasUsed、`_op_da_footprint_gas_scalar`=同名 RPC 字段。
- `_op_raw`：每笔非 deposit tx 经 `eth_getRawTransactionByBlockNumberAndIndex`；
  deposit 走结构化字段（`is_system_tx` RPC 缺失补 false；`mint` 按 RPC 存在性——
  本 devnet op-node 给 attributes 存款带 `mint:0x0`，与执行语义等价故原样保留；
  `value` 非 0 才发射——runner 资金桥的 portal 存款带 `value=mint=1e18`，漏发会
  造成假分歧）。
- `output`（回执返回数据）：RPC 回执不携带，用 `debug_traceBlockByNumber + callTracer`
  逐块取 top-level output（空补 "0x"）。
- postState：`debug_accountRange` 分页（≤256/页；**next 光标是 base64**——Go []byte
  JSON 序列化，`start` 参数要 0x-hex，须转码）。pre = block 0 全量状态（replayer
  从它播种整条链）。`--poststate boundary`（缺省）采样 = 首/末 + 每 100 块 +
  激活块 ±1，写出 `sampledBlocks`（0-based）；full 每块全量。
- `_info.hardfork`：rollup.json fork 时间表 + 块时间戳段判定（复用 Task 3 runner
  的语义）；**delta 段标注为 canyon**——Delta 是 devnet 本地 rollup 表 fork，本
  pin 无任何 EL 行为（chainconfig 无 deltaTime、attributes.go 无注入分支），而
  向量契约只认 8 个 EL fork 名（OpT8nReplay 硬校验）；stdout 打 NOTE 行存证。
- stem：`devnet_<块数>_<digest8>`，digest8 = sha256("0:regolith,<激活块>:<fork>,…")[:8]
  （激活块号取自**实际链上头**，1-based；与 ladderStem 同构，spec 不同块数不同的
  导出互不覆盖）。

## 导出实测（2026-09-15 04:39，复用运行中栈 + jovian 段 bcos-testing 一轮）

- 链：6721 块（l2_time=1789376018，激活块 625/938/1250/1875/2500/3125/3750/4375，
  与 rollup 公式逐一吻合）；交易 6744 deposit + 105 eip1559（bcos-testing jovian 轮，
  块 6474–6612，included 92 / reverted 13）；`txsource` settle OK。
- 文件 `devnet_6721_57bd1bcb.json` 67,471,802 B（boundary 采样 92 块 postState）；
  结构自检与 ladder_1000_69292b10.json 逐键对照：顶层形状/每 block 键集
  （pre 仅 block0）/env 8 字段/header 键集按 fork（regolith 无 withdrawals、canyon+、
  ecotone+ 加 blobGasUsed、isthmus+ 加 requestsHash）/receipts `_op_*` 家族按 fork
  与类型/postState 的 GenesisAlloc 形状（balance 恒有、nonce/code/storage zero 省略）
  ——**全部为 ladder 键集家族的子集或同集，无越界键**；devnet 独有仅
  「deposit 回执带 logs」与 delta 头部（=canyon 形状），均为契约内合法存在性。

## FISCO 试重放结果（临时注册 → OpT8nReplay/Vectors → 已还原）

既有语料（165 向量含 ladder）**全绿**（0 条非 devnet 失败行）；devnet 向量
6721 块中 1254 块分歧、14988 行，全部归三族：

1. **全部 pre-Ecotone 段（idx 0..1250，1251 块）**：`gasUsed`/`receiptsRoot`/
   `stateRoot` + `receipts[0].gasUsed/.cumulativeGasUsed` + 采样块上
   `postState.0x…0015.storage.0x0/0x1/0x2/0x3/0x4`。逐条原样：块 0
   `gasUsed want=0x230f0 got=0x6cf5`；块 1 `want=0xcd14 got=0x6cf5`；块 1000
   `want=0xb740 got=0x6d01`；ecotone 前一块 1249 `want=0x16c4d6 got=0x167c99`。
   got 侧恒为本征+calldata 的近平常数（~0x6cf5/0x6d01），want 随 L1 价波动
   63k–1.49M → FISCO 的 pre-Ecotone 系统存款 L1 data fee 计价为 0/错位，且
   L1Block 槽 0/1/2/3/4 未写入（want=真实 L1 值，got=0x0）。与 P2 Task 4 §7.7
   的 l1Cost 槽错位发现同根（SendTx 路径 panic、重放路径计 0）。
2. **Ecotone 激活块（idx 1250）**：`postState.0x…0015.storage.0x6 want=
   0x1000…00c3c9d00000558 got=0x0`——真实链在激活块写入 Ecotone scalar blob，
   FISCO 重放未写（后续 ecotone 块正常，说明仅激活块分支分歧）；同块
   `gasUsed want=0xf8cb got=0x27287`（方向反转：FISCO 计价反超真实）。
3. **fjord/isthmus/jovian 激活块（idx 1874/3749/4374）**：升级存款小额 gas 差
   （isthmus `receipts[2].gasUsed want=0x7580 got=0x7579`；fjord 块
   `gasUsed want=0x168068 got=0x168061`）；升级部署 tx 的
   `receipts[i].output` 不符（want=callTracer 取回的真实部署返回码）；isthmus/
   jovian 激活块 `withdrawalsRoot want=0x8ed4baae… got=0x56e81f17…(空树根)`
   ——prague 激活块的真实 withdrawalsRoot 非空树根，与 ecotone/fjord 激活块
   （空根命中）行为不一致，属待裁决点；`stateRoot` 随之分歧。

分歧即本管线价值：1/2 族指向 FISCO pre-Ecotone L1-cost/L1Block 写入路径，
3 族指向激活块升级 tx 的执行细节，均为正式 DIVERGENCES 判定的输入（本任务
不豁免、不裁决）。

---

# 健壮性审计与加固（P4，2026-09-15）

对全套工具做只读失败注入审计（全部在 /tmp 隔离复现，不污染语料仓），确认 10 项
must-fix 并修复。逐项证据见提交 `harden(devnet)`；本节只记定案。

## 审计确认的关键失败模式（修复前）

| 维度 | 复现证据（摘要） |
|---|---|
| 恢复路径 | 二进制缺失（重启后 /tmp 必失）时 `down`/`status`/`clean` 全部 die exit 1 —— 重启后无法清理陈旧 pidfile |
| PID 复用 | 把无辜 `sleep` 的 pid 写进 `geth.pid`，`down` 直接 SIGTERM+SIGKILL（无进程身份校验） |
| 半状态 | 坏 fork 阶梯（inspect 失败）与坏 geth 二进制两个场景，up 失败后 anvil 孤儿进程继续监听 |
| 静默死亡 | inspect 步骤无守卫：坏阶梯实测脚本 exit 1 且零输出，报错只在 `logs/inspect-genesis.err` |
| 垃圾锚点 | toml 删 `genesis_timestamp` 后锚点静默算成 `l2_time=52`（空值当 0） |
| 阶梯乱序穿透 | ecotone=4000/fjord=2500 被 op-deployer apply 接受，genesis 生成期才炸 |
| sha256 无断言 | versions.lock 自述「不参与启动断言」；截断的 geth 一路放行到 init 阶段才间接暴露 |
| 挂死 | chainexport http.Client 无 Timeout：accept-no-reply 服务器实测挂死 >40s 永不返回 |
| RPC 静默空转 | run.sh L2 RPC 不可达时 head=0 ts=0 空转，烧完 run-timeout 才报超时 |
| 裸 traceback | check_activations RPC 不可用 → URLError traceback + exit 1（与「计数不符」语义冲突） |

## 加固定案

- `opdevnet.sh up` 预检 fail-fast：sha256 使用时断言（lock 条目含 `sha256=` 即校验）、
  必需工具（curl/jq/python3/lsof）、devnet.toml 结构校验（必需键/数值/端口范围与重复/
  fork 阶梯严格递增/jwt 形状/create2_salt 形状/da_type）、磁盘余量（默认 5GiB，
  `DEVNET_MIN_FREE_DISK_GB` 覆盖；geth 自杀阈值 ~1.6GiB）、up 互斥锁（mkdir 原子 +
  陈旧 pid 抢占，封 preflight TOCTOU 窗口）。
- 失败传播：catch-up 完成前任何失败 → 自动清理本次启动的组件（反序、经身份校验）+
  非零退出 + 指向日志；catch-up 完成后的失败（激活计数不符）**保留运行现场**供 fork
  审计取证，提示 `down` 清理。
- `down`/`status`/`clean` 不再要求二进制存在；pidfile 格式升为 `<pid> <comm>`，kill 前
  `ps -o comm` 校验身份，PID 复用时告警并跳过。apply 记 `apply.pid`（旧版 Ctrl-C 后成
  孤儿且 preflight 不可见）；apply 代矿循环监督 anvil 存活（旧版 anvil 死亡后 apply 挂死）。
- `txsource/run.sh`：L2 RPC 连续 20 次轮询失败 → 明确 die（容忍 ~20s 节点重启）；
  `--run-timeout` 默认 1800→7200（bcos-testing 8 段全量实测需 ~3600s）。
- `check_activations.py`：RPC 不可用 → 3 次重试后明确报错 **exit 3**（新增，与语义失败
  区分）；rollup.json 结构坏 → **exit 2** 带说明。
- `chainexport`：每请求超时（`-rpc-timeout`，默认 300s）+ 传输层错误 3 次退避重试
  （RPC error 结果不重试）+ 输出文件存在默认拒绝覆盖（`--force` 才覆盖；stem 含头块高，
  链在长则 stem 变化，不会误伤）。
- 修复引入 `proc_comm_of_pid` 时实测踩坑：ps 对已死 pid 返回 rc=1，经 pipefail 传进
  substitution 会让 `set -e` 下 down 静默 exit 1 —— 恢复路径上所有 substitution 一律
  `|| true` 兜底（第 3 次回归抓出并修复）。

## 回归（加固后全过）

- 默认 up ×3（两次经 down）→ 激活计数 6/3/8/5 全中、`status` HEALTHY、genesis hash
  跨 down/up 幂等（`IDEMPOTENT: identical`）。
- txsource 单测 21/21（含新增 T7：RPC 不可达 → die）；demo 插件 e2e（资金桥→3 交易
  入块→settle）通过。
- chainexport 真链导出 5897 块成功；同 stem 重导被拒、`--force` 覆盖且 sha256 一致
  （导出确定性顺带验证）。

# WI-E2：op-geth 真实 deposit 回执 golden 捕获（RPC 表示层）

目的：给 FISCO `bcos-rpc` 的 `depositNonce`/`contractAddress`/deposit 回执形状提供
**op-geth 真实 RPC 输出** golden（消除「FISCO 自写自过」验证缺口）。golden 落
FISCO 树 `bcos-rpc/test/unittests/rpc/golden/op-geth-deposit-receipt/`，由
`Web3ResponseTest.cpp` 的 `opgethGolden*Receipt` 用例逐字段对拍。

## 捕获方法（devnet 重启后可随时重做）

```bash
# 0) /tmp/d3-bin 二进制重启即失：按 versions.lock src= 重建（geth @ op-geth e8800cffe，
#    op-node/op-batcher/op-deployer @ optimism-d3-precheck 76e4fad5）。
#    实测 `go build -o /tmp/d3-bin/geth ./cmd/geth`（op-geth 树）、
#    `go build -o /tmp/d3-bin/op-node ./op-node/cmd` 等（optimism 树）产物
#    sha256 与 versions.lock 逐位一致。
~/.cache/fisco-t8n-corpus/tools/devnet/opdevnet.sh up
export PATH="$PATH:/Users/octopus/.foundry/bin"; R=http://127.0.0.1:9545

# 1) attributes deposit：任意块首笔（type 0x7e）。多采样几个高度确认 nonce 语义。
TX=$(cast rpc eth_getBlockByNumber 0xbb8 false --rpc-url $R | jq -r '.transactions[0]')
cast rpc eth_getTransactionReceipt $TX --rpc-url $R | jq .

# 2) 用户存款：anvil key 对 L1 OptimismPortal（地址取
#    /tmp/opdevnet/run/deployer/state.json 的 OptimismPortalProxy）发 depositTransaction：
cast send 0xd01da3544ee5600483d8a149a3bb21a4aaa6c4de \
  "depositTransaction(address,uint256,uint64,bool,bytes)" \
  0x70997970C51812dc3A010C7d01b50e0d17dc79C8 1000000000000000 100000 false 0x \
  --value 1000000000000000 \
  --private-key 0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80 \
  --rpc-url http://127.0.0.1:8545

# 3) 找 L2 落点：看 opnode.log 的 "Sequencer sealed block ... txs=2"（用户存款块）。
#    ⚠️ 派生滞后 ~2×（sequencer 消费 L1 origin 比实时慢一倍），别扫当前 head 窗口——
#    实测存款块在 head 后方数百块。
grep -n "txs=2" /tmp/opdevnet/logs/opnode.log | tail
cast rpc eth_getTransactionReceipt <hash> --rpc-url $R > golden.json

# 4) 创建型存款（拿 contractAddress golden）：isCreation=true 时 to 必须是 address(0)，
#    否则 portal revert custom error 0xc5defbad（OptimismPortal_BadTarget）。
cast send 0xd01da3544ee5600483d8a149a3bb21a4aaa6c4de \
  "depositTransaction(address,uint256,uint64,bool,bytes)" \
  0x0000000000000000000000000000000000000000 0 100000 true 0x \
  --private-key 0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80 \
  --rpc-url http://127.0.0.1:8545
```

## 实测语义（2026-09-15，Jovian 全档 devnet）

- **depositNonce = 存款执行时发送者账户的 EVM nonce**（`core/state_processor.go:218-225`
  `nonce = statedb.GetNonce(msg.From)`），**不是全局存款计数器**：
  - attributes deposit 全部出自 `0xdead...0001`，逐块 +1（block 100→0x63、3000→0xbb9、
    4375→0x1119）；op-node 偶发的 predeploy 配置存款同账户，会插队 +1（fjord 块 1875
    的 GasPriceOracle 配置存款 nonce 0x754 紧跟 l1info 0x753）。
  - 用户存款各自按（未 alias 的）L1 发送者账户 nonce 计（实测首笔 0x0、次笔 0x1）。
  - 升级存款出自 `0x4210...00..07` 专用账户（to=null 创建型，nonce 从 0 计）与
    硬编码 `0x0000...0000`（nonce 逐笔累积：ecotone 0x0 → jovian 0x6）。
- **depositReceiptVersion**：Cyanon 起恒 `0x1`（`CanyonDepositReceiptVersion`）；Canyon 前
  字段**缺席**（非 null）。deposit 回执上从不出现 l1GasPrice/l1Fee 等（门控
  `!tx.IsDepositTx()`），`effectiveGasPrice` 恒 "0x0"。
- **本 devnet 不做 L1→L2 address aliasing**：d3-precheck 的 op-node 在
  `op-node/rollup/derive/deposit_log.go:84` `dep.From = from`（上游 stock 是
  `ApplyL1ToL2Alias(from)`）——用户存款回执 `from` = L1 原始发送者。这不是 RPC
  表示层问题，但读 golden 时别误判为形状分歧。
