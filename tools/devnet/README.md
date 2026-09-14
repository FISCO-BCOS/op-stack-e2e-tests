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
