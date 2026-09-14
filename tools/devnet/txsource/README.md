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

- **RPC 套件型**：配置端点后让既有套件自发交易。如 bcos-testing：改 `hardhat.config.js` 的
  network（url=`--endpoint`、chainId=`--chain-id`、accounts=[`--funding-key`]）→
  `npx hardhat test --network bcosnet`，结束后由测试输出汇总出报告（Task 4 接入）。
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
