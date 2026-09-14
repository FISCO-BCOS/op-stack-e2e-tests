# txsource —— 可插拔交易源执行器（P1-3）

设计依据：`docs/plans/2026-09-14-real-derivation-pipeline-design.md` §3.2（接口契约）、§4.3（settle）、§4.4（资金桥）；
调度方案 A（per-segment 全量轮，2026-09-14 用户决策）。devnet 起栈见 `../README.md`（P1-1/P1-2 定案）。

```
opdevnet up ──▶ 运行中的 devnet（L1 anvil + L2 op-geth/op-node/op-batcher）
                      ▲
run.sh ── 每个 fork 段触发一轮插件 ──▶ plugins/<插件>（自发交易）
                      ▼
        每轮 settle（batcher 提交 + safe 追上 unsafe）→ summary.json
```

## 1. 插件契约

**插件 = 一个可执行入口**（bash/python/二进制均可），由 runner 以统一参数调用：

```
<plugin> --endpoint <L2 RPC> --chain-id <id> --funding-key <hex私钥> --segment <fork名> --round <n>
```

| 参数 | 含义 |
|---|---|
| `--endpoint` | L2 op-geth HTTP RPC（如 `http://127.0.0.1:9545`） |
| `--chain-id` | L2 chain id（十进制，devnet = 901） |
| `--funding-key` | runner 资金桥已注资的私钥（缺省 anvil acct#9）；签名/nonce 管理**插件自担** |
| `--segment` | 本轮所在 fork 段名（小写：`bedrock` / `canyon` / … / `jovian`，取自 rollup.json 时间表） |
| `--round` | runner 内全局轮次（1 起，跨段递增） |

行为约定：

1. **自行构造/发送交易**，stdout **仅**输出一行 JSON 交易报告，日志一律走 stderr：
   ```json
   {"tx_total": 3, "included": 3, "reverted": 0, "by_type": {"transfer": 3}}
   ```
2. 退出码 0 = 轮成功；非 0 = 轮失败（记录进 summary `rounds_failed`，runner 继续下一段，最终退出码 1）。
3. 环境变量：`TXSOURCE_ROUND_DIR`（本轮产物目录，可写中间产物）、`TXSOURCE_OUT_DIR`（整个 run 的目录）。
4. 插件**不做 settle、不做资金**——runner 负责每轮前的资金桥与每轮后的沉降等待。

**两类原型**（新插件对齐其一即可，管线零改动接入）：

- **RPC 套件型**：配置端点后让既有套件自发交易。实装：`plugins/bcos_testing/run.sh`
  （Task 4，bcos-testing/Hardhat 全套件，见其头注释与 §7）。
- **程序生成型**：插件自构造 raw tx 直接 `eth_sendRawTransaction`。原型见
  `plugins/demo_transfer.sh`（cast 串行发 3 笔转账，逐笔等回执计数）。

## 2. runner（`run.sh`）

```bash
cd tools/devnet/txsource
./run.sh --plugin plugins/demo_transfer.sh                 # 全部默认：读 ../devnet.toml
./run.sh --plugin <可执行> --funding-amount 1 --target-blocks 0 --settle-timeout 180
```

选项：`--plugin`（必填）；`--devnet-toml`（缺省 `../devnet.toml`，桩测试可传空串改用
`--rollup-json --l2-rpc --opnode-rpc` 直连）；`--funding-key/--funding-amount`（资金桥目标
账户/每轮金额 ether）；`--rich-key`（L1 富账户，缺省 toml `l1.private_key` = anvil acct#0）；
`--target-blocks`（到达即收尾，0=只在全部段覆盖后退出）；`--settle-timeout`；`--no-fund`；
`--out-dir`（缺省 `/tmp/txsource/<时间戳>`）。

产物：`$OUT_DIR/summary.json`（汇总）、`rounds.jsonl`（每轮一行）、`segments.tsv`（段表）、
`round-<n>-<segment>/`（每轮 report.json / plugin.log / 资金回执）。

### 调度语义（方案 A：per-segment 全量轮）

- **段表权威 = rollup.json**（生效值），**不读 devnet.toml 的 forks 表**（那是输入）。
  段 = `bedrock`（genesis.l2_time 起）+ 每个 `<fork>_time` 段；激活块号 = `⌈(fork_time − l2_time)/block_time⌉`。
- 轮询当前 L2 头块时间戳 → 判定所在段 → **每个未触发的段触发恰好一轮**（一次轮询跨多个边界时按段序逐段补触发）。
- **激活块跳过语义**：触发条件要求头块号**严格大于**该段激活块——插件交易不与激活块的
  升级交易注入混块，保持 §7.4 的逐激活块升级交易计数（Ecotone 6/Fjord 3/Isthmus 8/Jovian 5）纯净。
- **中途加入**：runner 启动头之前的段（激活块已在链史中、且非当前段）标记 **missed**，不补轮
  ——链史已定型，补轮只会把交易打在后续段还谎报段名。当前段照常触发一轮。
  ⚠ 注意时序：本 devnet 的 fork 绝对时间锚定在 toml 固定时间戳；若 runner 在最后边界
  （jovian，墙钟 ~11:19 UTC）之后才启动，只能观察到 jovian 段。**全段覆盖需在 devnet up 后
  立即接入 runner、且墙钟尚未越过最后边界**；否则用 `--target-blocks` 或接受单段轮。
- 退出：全部段覆盖（最后一段已触发）或 `--target-blocks` 达到；轮失败/沉降失败不改退出时序，
  只反映在 summary 与退出码（1）。

### 资金桥（L1 depositTransaction，真实过桥）

devnet intent `fundDevAccounts = false` ⇒ **L2 无预富账户**（实测 anvil key0/key9 在 L2 余额 0）。
runner 每轮前：从 L1 富账户（anvil acct#0）调 `OptimismPortal.depositTransaction(插件账户, 金额, 200000, false, 0x)`
存入确定性金额（缺省 1 ether/轮）→ 轮询 L2 `eth_getBalance` 确认到账（存款经 L1 派生直接进块，
不依赖 batcher）→ 才触发插件。portal 地址取 op-deployer `state.json` 的
`opChainDeployments[id].OptimismPortalProxy`（按 chain-id 匹配）。

### settle（设计 §4.3）

每轮插件结束后：快照当前 unsafe → 轮询 op-node `optimism_syncStatus` 等 **safe ≥ 快照**
（batcher 已把 unsafe 块提交 L1、op-node 派生追平）→ 才进入下一段。超时（`--settle-timeout`，
缺省 180s）记 `settle_failed` 并在最终退出码体现。字段兼容 snake/camel（本构建 op-node 返回
`safe_l2`/`unsafe_l2`）。

## 3. 单测（不依赖真链）

`test/run_tests.sh` + `test/rpc_stub.py`（假 JSON-RPC 桩：按事件脚本应答，`eth_blockNumber` /
`optimism_syncStatus` 每次轮询推进事件指针，块时间戳按 `ts0 + n·block_time` 线性模型外推）。
覆盖：段边界检测（边界前/边界/边界后 + delta 奇数偏移 ceil→938）、每段恰好一轮（9 段 = bedrock+8
fork，含激活块当拍不触发/次拍触发）、中途加入 missed 不补轮、settle 失败、插件失败、target-blocks 退出。

```bash
test/run_tests.sh    # 期望 RESULT: PASS=18 FAIL=0
```

## 4. 端到端演示

```bash
../opdevnet.sh up                                     # 起栈（~90s，见 P1-2）
./run.sh --plugin plugins/demo_transfer.sh            # 单段全通路：资金桥→3 笔转账→报告→settle
# campaign 栈（fork 边界按墙钟逐段到来，见 ../README.md P2 节）：
../opdevnet.sh up --campaign
./run.sh --plugin plugins/bcos_testing/run.sh --funding-key 0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80 \
         --run-timeout 3600                           # 全段扫掠 ≈ 45min 墙钟，默认 1800s 不够
python3 check_distribution.py --rollup /tmp/opdevnet/artifacts/rollup.json --rpc http://127.0.0.1:9545
```

多段调度演示（插件触发次数 = 段数）由桩场景承担：`test/run_tests.sh` 的 T2（head 从 0 走到
4400，9 段各触发一轮、轮次 1..9、激活块跳过）+ T3（中途加入只触发当前段及未来段）。
桩与真链共用同一 run.sh 调度主循环——桩测的调度语义即真链语义。

## 5. 文件

| 文件 | 作用 |
|---|---|
| `run.sh` | runner：per-segment 调度 + 资金桥 + settle + 汇总（stdout 仅 summary JSON，日志 stderr） |
| `plugins/demo_transfer.sh` | 演示插件（程序生成型原型）：3 笔转账 → 交易报告 |
| `test/run_tests.sh` | 调度器单测（桩驱动，不依赖真链） |
| `test/rpc_stub.py` | 假 JSON-RPC 服务（事件脚本应答） |
| `test/fake_plugin.sh` | 桩插件：记录触发、返回合法报告（可注入失败） |
| `test/fixtures/rollup_stub.json` | 桩 rollup.json（真实 fork 偏移，锚 l2_time=1000） |

## 6. 已知边界

- 段间隔远小于轮询间隔时，跨边界一次性逐段补触发（每段仍恰好一轮）。
- genesis 处激活的 fork（l2_time 处）不在段表（本 devnet 全部 fork 在 genesis 后激活，Task 1 同）。
- funding 走 cast（依赖 foundry）；stub 单测用 `--no-fund`。
- 轮内插件失败/沉降失败：不中断后续段，summary + 退出码呈现（`rounds_failed`/`settle_failed`）。

## 7. bcos_testing 插件（P2 Task 4：bcos-testing 全套件作为交易源）

```
runner（per-segment 调度 + L1 资金桥 + settle）
   └── plugins/bcos_testing/run.sh
         ├── prepare：clone https://github.com/FISCO-BCOS/bcos-testing（缺省 /tmp/bcos-testing，
         │            环境变量 BCOS_TESTING_DIR 可改）+ npm ci + 补装上游缺声明依赖（见下）
         ├── run：逐文件 `npx hardhat test --config .hardhat.devnet.config.js --network bcosnet <file>`
         │        （mocha passing/failing/pending + 区块扫描该账户入块交易，逐文件入账）
         └── report：stdout 一行 JSON（tx_total/included/reverted/by_type + files[]/skipped[]，
                      断言失败 tests_failed 与未入块 tx_total 分列 —— 对应验收 C.4）
```

适配原则与实测定案（详见插件头注释）：

1. **上游文件零改动**。chainId 必须由外部 wrapper 提供：上游 hardhat.config.js 把 bcosnet
   硬编码为 chainId=20200，而 hardhat 的 `ChainIdValidatorProvider` 会在首个 RPC 请求校验
   config.chainId == 节点 eth_chainId（`INVALID_GLOBAL_CHAIN_ID`）→ 插件生成
   `.hardhat.devnet.config.js`（require 上游配置以保留插件注册/编译设置，仅覆写
   bcosnet 与 paths），`--config` 指入；文件由插件生成并在退出时清理，套件 git 树保持干净。
2. **项目根陷阱（HH1007）**：hardhat 项目根 = --config 文件所在目录且会对 config 路径做
   realpath —— macOS /tmp 是 /private/tmp 的符号链接，wrapper 内一切路径必须
   `realpathSync` 对齐，否则上游 contracts 被判 "outside the project"。
3. **上游 package.json 缺直接依赖**：`scripts/utils/transactionCreator.js` require
   `ethereum-cryptography/utils`、`@ethereumjs/rlp`，上游未声明；顶层被传递依赖钉在
   ethereum-cryptography@0.1.3（无 utils 子路径）→ 3 个 tx 测试 MODULE_NOT_FOUND。
   插件 prepare 检缺补装 `ethereum-cryptography@^2`（`--no-save`，不触碰上游清单）。
4. **断言失败 ≠ 轮失败**（验收 C.4）：bcos-testing 部分断言按 FISCO 语义写（如
   block.coinbase==0x0，sequencer 链上 coinbase=fee recipient），在 op-geth devnet 上
   必然失败 —— 属允许噪音，如实计入 report（`tests_failed`/`files_assert_fail`），不跳过、
   不算 rounds_failed；「交易未入块」由 tx_total/included/reverted 与
   `check_distribution.py` 的链级分布独立呈现。静态跳过表 SKIP 保留机制，当前为空
   （首轮实测未发现结构性不兼容文件）。
5. **轮预算/超时**：单文件 hardhat 进程上限 `BCOS_TESTING_FILE_TIMEOUT`（缺省 240s）、
   整轮预算 `BCOS_TESTING_ROUND_BUDGET`（缺省 270s，超时剩余文件记 skipped_budget），
   轮间按 ROUND_ROTATE 轮转起始文件。实测全套 19 文件一轮 ~260s（2s 块距），恰容纳于
   300s 段宽；超预算轮仍完成，交易会溢入后续段（段覆盖不受影响，见 §2 补触发语义）。
6. **funding**：bcos-testing 用 anvil key0（0xac09…ff80，其测试内默认私钥也是它）——
   实测该账户 L2 余额为 0（intent fundDevAccounts=false），必须走 runner 资金桥；
   runner 传 `--funding-key 0xac09…ff80`（L1 富账户自存自，每轮 1 ether 足够）。
7. **pre-ecotone 发送闸门（重要实测发现，fork 审计信号）**：本 op-geth 构建（d3 worktree
   `blockchain-impl/op-geth`）在 pre-ecotone 段校验用户交易时走 bedrock L1-cost 路径，
   硬编码槽位读取（rollup_cost.go `L1BaseFeeSlot=1/OverheadSlot=5/ScalarSlot=6`）与部署的
   （jovian 代 contracts-bedrock）L1Block 存储布局错位 —— 实测 canyon 段 slot6 已是 ecotone
   打包 scalars（读出 2.8e70 被当裸 scalar）→ `panic("overflow in total rollup cost: l1Cost")`
   （rollup_cost.go:373）→ RPC 层 recover（`method handler crashed`）但 payload builder
   死锁，链永久卡死（实测）。ecotone 起走 calldata 解码路径不受影响（jovian 段 106 笔全过）。
   插件因此对 `--segment ∈ {bedrock, canyon, delta}` 整轮跳过发交易（`tx_gate:
   "pre_ecotone_no_send"`，轮 rc=0），报告如实记录；段覆盖调度不受影响。

### 链级分布核查（验收 C.3）

`check_distribution.py --rollup <rollup.json> --rpc <L2 RPC> [--funding-addr 0x..]`：
逐块统计非 L1-attributes 交易（from != 0xdead…0001 且 type != 0x7E）按块 ts 落段分布，
退出码 0 = 用户交易覆盖 ≥2 段。

## 8. 文件（Task 4 增量）

| 文件 | 作用 |
|---|---|
| `plugins/bcos_testing/run.sh` | bcos-testing 套件插件（RPC 套件型实装） |
| `plugins/bcos_testing/txscan.py` | 区块扫描账本（入块/回执状态/类型，余额查询） |
| `check_distribution.py` | 非 attributes 交易按 fork 段分布核查（验收 C.3） |
