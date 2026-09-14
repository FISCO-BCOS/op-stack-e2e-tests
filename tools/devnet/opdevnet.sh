#!/usr/bin/env bash
# opdevnet.sh —— 配置驱动的 devnet 启动器（P1-2；P2 加 campaign 模式）
#
#   ./opdevnet.sh up      从干净状态起全栈：anvil → op-deployer(init/apply/inspect) → geth → op-node → op-batcher
#                         → 健康检查（端口 + 激活块升级交易计数 6/3/8/5）
#   ./opdevnet.sh up --campaign [--forks "..."] [--segment-seconds N]
#                         campaign 变体：fork 阶梯压缩为每段 N 秒（缺省 300）+ L1 genesis
#                         锚定 = 墙钟-500s，链时间贴着墙钟走 —— txsource runner 可以在真实
#                         墙钟里逐段观察全部 fork 段（默认 up 的跳时钟策略会让链时间瞬间
#                         越过全部边界，只能观察到最后一段）。实现 = 生成临时 toml
#                         （/tmp/opdevnet-campaign.toml），默认 up 完全不受影响（幂等语义不变）。
#                         campaign 模式下：不做 L1 跳时钟、不做激活块计数检查（边界本来就没到）、
#                         L2 catch-up 以「头块时间追平墙钟」为完成信号。
#                         runtime_dir/端口/chain-id 与默认 up 相同 → status/down、txsource run.sh
#                         用缺省 devnet.toml 即可操作 campaign 栈。
#   ./opdevnet.sh status  组件进程/端口 + L2 块高 + 所在 fork 段 + safe/unsafe 差
#   ./opdevnet.sh down    反序 kill + 清理（产物 artifacts/ 与日志 logs/ 默认保留）
#   ./opdevnet.sh clean   down 后连运行时目录一起删除
#
# 全部参数来自 devnet.toml + versions.lock，工具不复现实验、不发明参数。
#
# 与 Task 1 实测序列的两处显式差异（目的都是幂等，稳态行为等价）：
#   1) anvil 以 --no-mining 启动（Task 1 用 --block-time 2）：块生产完全由脚本驱动——
#      用 evm_setNextBlockTimestamp 逐块指定精确 ts（实测手动 evm_mine 不读
#      anvil_setBlockTimestampInterval）：先精确矿到固定 pin_block 停住（确定 apply 时刻的
#      L1 起始块 = 确定产物 fork 绝对时间），apply 期间脚本代矿（parent+2），跳时钟阶段
#      逐块 +jump_interval_s，最后 evm_setIntervalMining 2 恢复与 --block-time 2 等价的稳态
#      （tick 块 ts 恒 = parent+2，Task 1 实测过全程 2s 整齐阶梯）。
#   2) L1 genesis 时间戳取配置文件的固定绝对值（Task 1 取 now-9000 的相对值）。
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
TOML="${DEVNET_TOML:-$DIR/devnet.toml}"
LOCK_FROM_TOML=""
STEP="init"
FORKS="canyon delta ecotone fjord granite holocene isthmus jovian"
RUN_FORKS="$FORKS"                # 本次 up 生效的 fork 集合（campaign --forks 子集时 ≠ FORKS）
FORKS_NOINJECT="canyon delta granite holocene"   # attributes.go 无注入分支 / 计数为 0
UPGRADE_COUNTS="canyon=0 delta=0 ecotone=6 fjord=3 granite=0 holocene=0 isthmus=8 jovian=5"

# ---------- campaign 模式（P2 Task 4-A） ----------
# 目标：让 fork 边界在真实墙钟里依次到来，runner 可逐段观察。
#   - 阶梯压缩：第 i 个 fork 偏移 = i * segment_seconds（缺省 300s → 8 边界跨 40min 链时间）
#   - L1 genesis 锚 = 墙钟 - 500s。实测：up 的 L2 机器速度追平会把链时间推进到
#     l2_time + ~480s（bedrock 段被结构性消耗，runner 标 missed），第一可观察段 = canyon
#     （窗口 ~120s），其余 7 段全部可触发 —— 8 轮 8 段
#   - 生成临时 toml（CAMPAIGN_TOML），默认 up 的 devnet.toml 一字不改（幂等语义不受影响）
CAMPAIGN=0
CAMPAIGN_FORKS=""                 # 空 = 全部 8 fork；否则为空格分隔子集（保持标准顺序）
CAMPAIGN_SEG_S="300"
CAMPAIGN_ANCHOR_OFFSET_S="500"    # genesis_ts = now - 500
CAMPAIGN_TOML="${OPDEVNET_CAMPAIGN_TOML:-/tmp/opdevnet-campaign.toml}"
CAMPAIGN_CATCHUP_DRIFT_S="30"     # L2 头块时间追平到墙钟该误差内 = catch-up 完成

log()  { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
die()  { printf '[%s] FAIL(step=%s): %s\n' "$(date +%H:%M:%S)" "$STEP" "$*" >&2; exit 1; }
usage() { sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

# ---------- 配置解析（仅支持本仓库 devnet.toml 的平面格式） ----------
toml_get() { # $1=section $2=key -> value（去掉引号）
  awk -v sec="$1" -v key="$2" '
    { sub(/#.*/, ""); sub(/[ \t]+$/, "") }   # 先剥行内注释与行尾空白（本文件值中不含 #）
    /^[ \t]*\[/ { cur=$0; gsub(/[][]/,"",cur); gsub(/[ \t]/,"",cur); next }
    cur == sec && NF >= 3 {
      k=$1; sub(/^[^=]*=[ \t]*/,""); v=$0
      if (k == key) { gsub(/^"|"$/,"",v); print v; exit }
    }' "$TOML"
}
lock_path() { # $1=binary key in versions.lock
  awk -v k="$1" '$1==k { for (i=2;i<=NF;i++) if ($i ~ /^path=/) { sub(/^path=/,"",$i); print $i; exit } }' "$2"
}

load_config() {
  [ -f "$TOML" ] || die "config not found: $TOML"
  local lock
  RUNTIME="$(toml_get meta runtime_dir)"
  ART="$RUNTIME/artifacts"; LOGS="$RUNTIME/logs"; RUN="$RUNTIME/run"; L2DIR="$RUNTIME/l2"
  L1_CHAIN_ID="$(toml_get l1 chain_id)"
  L1_GENESIS_TS="$(toml_get l1 genesis_timestamp)"
  L1_BLOCK_TIME="$(toml_get l1 block_time)"
  PIN_BLOCK="$(toml_get l1 pin_block)"
  DEPLOYER_KEY="$(toml_get l1 private_key)"
  BATCHER_KEY="$(toml_get l1 batcher_private_key)"
  P_ANVIL="$(toml_get l1.ports rpc)"
  L2_CHAIN_ID="$(toml_get l2 chain_id)"
  JWT="$(toml_get l2 jwt)"
  P_GETH_HTTP="$(toml_get l2.ports http)"; P_GETH_AUTH="$(toml_get l2.ports authrpc)"
  P_GETH_WS="$(toml_get l2.ports ws)"; P_OPNODE="$(toml_get l2.ports opnode_rpc)"
  P_BATCHER="$(toml_get l2.ports batcher_rpc)"
  JUMP_S="$(toml_get accel jump_interval_s)"
  MARGIN_S="$(toml_get accel activation_margin_s)"
  TARGET_BLOCKS="$(toml_get accel target_l2_blocks)"
  CATCHUP_TIMEOUT="$(toml_get accel catchup_timeout_s)"
  MAX_CHAN_DUR="$(toml_get batcher max_channel_duration)"
  DA_TYPE="$(toml_get batcher da_type)"
  CB="$(toml_get paths contracts_bedrock)"
  LOCK_FROM_TOML="$(toml_get paths versions_lock)"
  lock="${LOCK_FROM_TOML:-$DIR/versions.lock}"
  ANVIL="$(toml_get paths anvil)";     [ -n "$ANVIL" ]     || ANVIL="$(lock_path anvil "$lock")"
  GETH="$(toml_get paths geth)";       [ -n "$GETH" ]      || GETH="$(lock_path geth "$lock")"
  OPNODE="$(toml_get paths op_node)";  [ -n "$OPNODE" ]    || OPNODE="$(lock_path op-node "$lock")"
  OPBATCHER="$(toml_get paths op_batcher)"; [ -n "$OPBATCHER" ] || OPBATCHER="$(lock_path op-batcher "$lock")"
  OPDEPLOYER="$(toml_get paths op_deployer)"; [ -n "$OPDEPLOYER" ] || OPDEPLOYER="$(lock_path op-deployer "$lock")"
  for b in "$ANVIL" "$GETH" "$OPNODE" "$OPBATCHER" "$OPDEPLOYER"; do
    [ -x "$b" ] || die "binary not executable: $b (check versions.lock / devnet.toml [paths])"
  done
  [ -d "$CB" ] || die "contracts-bedrock not found: $CB"
  L2_HTTP="http://127.0.0.1:$P_GETH_HTTP"; L1_RPC="http://127.0.0.1:$P_ANVIL"
  OPNODE_RPC="http://127.0.0.1:$P_OPNODE"; BATCHER_RPC="http://127.0.0.1:$P_BATCHER"
  EXPECTED_L2_TIME=$((L1_GENESIS_TS + L1_BLOCK_TIME * PIN_BLOCK))
}

# ---------- 通用小工具 ----------
dec() { printf '%d' "${1:-0x0}" 2>/dev/null; }   # "0x1f" -> 31（RPC 数值统一转十进制）
rpc() { # $1=url $2=method [$3=params-json]
  curl -s -m 10 -X POST -H 'Content-Type: application/json' \
    --data "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$2\",\"params\":${3:-[]}}" "$1"
}
rpc_res() { rpc "$@" | jq -r '.result // empty' 2>/dev/null; }
port_busy() { lsof -nP -iTCP:"$1" -sTCP:LISTEN 2>/dev/null | grep -q LISTEN; }
wait_listen() { # $1=port $2=timeout_s
  local i=0; while [ "$i" -lt $(( $2 * 2 )) ]; do port_busy "$1" && return 0; sleep 0.5; i=$((i+1)); done; return 1
}
wait_rpc() { # $1=url $2=timeout_s [$3=method] —— result 非 null 即就绪
  local m="${3:-eth_chainId}" i=0; while [ "$i" -lt $(( $2 * 2 )) ]; do
    [ -n "$(rpc_res "$1" "$m")" ] && return 0; sleep 0.5; i=$((i+1)); done; return 1
}
start_bg() { # $1=name $2=logfile $3..=cmd —— 子进程统一以 $RUN 为工作目录（op-node 的
             # discovery/peerstore 目录会落在 cwd，防止泄漏进仓库）。
             # 注意必须用换行分隔而非 `cd && nohup ... &`：后者会把整个 AND 列表后台化，
             # $! 记到外壳 subshell 的 pid，down 时杀壳不杀真进程（实测踩坑）。
  local name="$1" logf="$2"; shift 2
  (
    cd "$RUN" || exit 1
    nohup "$@" >>"$logf" 2>&1 &
    echo $! >"$RUN/$name.pid"
  )
  log "started $name (pid $(cat "$RUN/$name.pid"), log $logf)"
}
pid_alive() { [ -f "$1" ] && kill -0 "$(cat "$1")" 2>/dev/null; }
kill_pidfile() { # $1=pidfile $2=name
  [ -f "$1" ] || return 0
  local pid; pid="$(cat "$1")"
  if kill -0 "$pid" 2>/dev/null; then
    kill "$pid" 2>/dev/null || true
    local i=0
    while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 10 ]; do sleep 0.5; i=$((i+1)); done
    kill -9 "$pid" 2>/dev/null || true
  fi
  rm -f "$1"
}
tail_log() { printf -- '---- tail %s ----\n' "$1"; tail -n "${2:-25}" "$1" 2>/dev/null || true; }
mine() { rpc "$L1_RPC" evm_mine '[]' >/dev/null; }
# 确定性手动矿：实测（anvil 1.7.1）
#   - evm_mine 不读 anvil_setBlockTimestampInterval（那只作用于间隔采矿 tick）
#   - evm_setNextBlockTimestamp(T)+evm_mine → 块 ts 精确 = T；T 必须 > 父块 ts，否则 -32602
#   - 未再设置时下一块沿用上次值（不会跳回墙钟）
mine_next() { rpc "$L1_RPC" evm_setNextBlockTimestamp "[$1]" >/dev/null; mine; }
head_number() { dec "$(rpc_res "$L1_RPC" eth_blockNumber)"; }
# 块号必须作为 JSON 字符串传（"0x1a"），裸 0x1a 不是合法 JSON，anvil 会回 Invalid Request(-32600)
block_ts() { dec "$(rpc_res "$L1_RPC" eth_getBlockByNumber "$(printf '["0x%x", false]' "$1")" | jq -r '.timestamp')"; }
head_ts() { block_ts "$(head_number)"; }

# ---------- campaign：由基准 toml 派生临时 campaign toml ----------
# 只改四处：meta.name / l1.genesis_timestamp（锚 = 墙钟-500s）/ [forks]（压缩阶梯，可子集）/
# [accel].target_l2_blocks（信息用）。runtime_dir、端口、chain-id、角色、盐全部继承基准 ——
# status/down 与 txsource run.sh 用缺省 devnet.toml 即可操作 campaign 栈。
generate_campaign_toml() { # $1=base toml $2=out toml
  local base="$1" out="$2"
  [ -f "$base" ] || die "base config not found: $base"
  # 校验子集：只允许 FORKS 表内的 fork，保持标准顺序
  local want="${CAMPAIGN_FORKS:-$FORKS}" f
  for f in $want; do
    case " $FORKS " in *" $f "*) ;; *) die "unknown fork in --forks: $f (known: $FORKS)";; esac
  done
  local anchor_ts=$(( $(date +%s) - CAMPAIGN_ANCHOR_OFFSET_S ))
  python3 - "$base" "$out" "$want" "$CAMPAIGN_SEG_S" "$anchor_ts" <<'PYEOF'
import re, sys
base_p, out_p, want, seg_s, anchor_ts = sys.argv[1], sys.argv[2], sys.argv[3].split(), int(sys.argv[4]), int(sys.argv[5])
text = open(base_p).read()
# 0) 文件头打 campaign 标记（注释独占一行 —— 本 toml 的 awk 解析器约定）
text = "# GENERATED by opdevnet.sh up --campaign（勿手改；基准 devnet.toml 不受影响）\n" + text
# 1) meta.name
text = re.sub(r'(?m)^name\s*=\s*".*"$', 'name       = "d3-devnet-campaign"', text, count=1)
# 2) l1.genesis_timestamp -> 墙钟锚（campaign 特意不固定绝对值：每次 up 锚定当前墙钟）
text = re.sub(r'(?m)^genesis_timestamp(\s*)=\s*\d+(\s*#.*)?$',
              lambda m: f'genesis_timestamp{m.group(1)} = {anchor_ts}', text, count=1)
# 3) [forks] 段整体替换为压缩阶梯：第 i 个 fork 偏移 = i*seg_s
#    （canyon=300, delta=600, ... 严格递增；delta 恒严格介于 canyon 与 ecotone 之间）
order = ["canyon", "delta", "ecotone", "fjord", "granite", "holocene", "isthmus", "jovian"]
picks = [f for f in order if f in want]
sec = ["[forks]"] + [f"{f:<8} = {i * seg_s}" for i, f in enumerate(picks, start=1)]
text = re.sub(r'(?ms)^\[forks\]\n.*?(?=\n\[)', "\n".join(sec) + "\n", text)
# 4) [accel].target_l2_blocks -> 末 fork 激活块 + 余量（campaign catch-up 不用它，仅 status 展示）
last_act = (len(picks) * seg_s) // 2 + ((len(picks) * seg_s) % 2)
text = re.sub(r'(?m)^target_l2_blocks(\s*)=\s*\d+(\s*#.*)?$',
              lambda m: f'target_l2_blocks{m.group(1)} = {last_act + 75}', text, count=1)
open(out_p, "w").write(text)
print(f"campaign toml: forks={' '.join(picks)} seg={seg_s}s anchor_genesis_ts={anchor_ts} last_boundary={len(picks)*seg_s}s")
PYEOF
  # 与基准一致性自检：runtime_dir/端口/chain-id 必须相同（runner/down/status 靠它们）
  local k base_v camp_v
  for k in "meta runtime_dir" "l1 chain_id" "l2 chain_id" "l1.ports rpc" "l2.ports http" "l2.ports opnode_rpc"; do
    base_v="$(TOML="$base" toml_get $k)"; camp_v="$(TOML="$out" toml_get $k)"
    [ "$base_v" = "$camp_v" ] || die "campaign toml drift: [$k] $base_v != $camp_v"
  done
}

# ---------- up ----------
cmd_up() {
  # --campaign 变体参数（默认 up 不带任何旗标，路径完全不变）
  while [ $# -gt 0 ]; do
    case "$1" in
      --campaign)        CAMPAIGN=1; shift ;;
      --forks)           CAMPAIGN_FORKS="${2:?}"; shift 2 ;;
      --segment-seconds) CAMPAIGN_SEG_S="${2:?}"; shift 2 ;;
      *) die "unknown up flag: $1" ;;
    esac
  done
  if [ "$CAMPAIGN" -eq 1 ]; then
    generate_campaign_toml "$TOML" "$CAMPAIGN_TOML" | sed 's/^/[campaign] /'
    TOML="$CAMPAIGN_TOML"
    RUN_FORKS="${CAMPAIGN_FORKS:-$FORKS}"   # intent/normalize/verify/status 只认本次生效的 fork 集
  fi
  load_config
  log "devnet up: runtime=$RUNTIME  l2_chain_id=$L2_CHAIN_ID  l1_chain_id=$L1_CHAIN_ID"
  [ "$CAMPAIGN" -eq 0 ] || log "CAMPAIGN mode: seg=${CAMPAIGN_SEG_S}s anchor=now-${CAMPAIGN_ANCHOR_OFFSET_S}s forks=[${CAMPAIGN_FORKS:-$FORKS}] toml=${TOML}（链时间贴墙钟，边界按墙钟逐段到来）"
  log "anchor: anvil genesis ts=$L1_GENESIS_TS (fixed), pin L1 head at block $PIN_BLOCK -> expected rollup l2_time=$EXPECTED_L2_TIME"

  # ---- preflight ----
  STEP="preflight"
  for f in batcher opnode geth anvil miner; do
    if pid_alive "$RUN/$f.pid"; then die "already running (pid $(cat "$RUN/$f.pid") in $RUN/$f.pid); run ./opdevnet.sh down first"; fi
  done
  local p
  for p in "$P_ANVIL" "$P_GETH_HTTP" "$P_GETH_AUTH" "$P_GETH_WS" "$P_OPNODE" "$P_BATCHER"; do
    port_busy "$p" && die "port $p already in use by another process; free it or edit devnet.toml"
  done
  if [ -d "$L2DIR" ]; then die "stale L2 datadir at $L2DIR; run ./opdevnet.sh down first"; fi
  if [ ! -d "$CB/forge-artifacts/SuperchainConfig.sol" ]; then
    log "forge-artifacts incomplete -> building contracts (Task 1 解法: FOUNDRY_DENY=false, deny_warnings 会把 warning 当 error)"
    ( cd "$CB" && FOUNDRY_DENY=false forge build --force --skip "/**/test/**" ) >"$RUNTIME/forge-build.log" 2>&1 \
      || { mkdir -p "$LOGS"; tail_log "$RUNTIME/forge-build.log" 40; die "forge build failed; see $RUNTIME/forge-build.log"; }
  fi
  mkdir -p "$ART" "$LOGS" "$RUN"
  rm -rf "$RUN/deployer"    # deployer workdir 每次重建（init 需要干净目录）
  # 幂等对照：保留上一次产物 hash
  local prev_hash=""
  if [ -f "$ART/rollup.json" ]; then prev_hash="$(jq -r '.genesis.l2.hash' "$ART/rollup.json")"; fi

  # ---- 1) anvil（L1，genesis 时间固定在过去；--no-mining，块生产由脚本驱动） ----
  STEP="anvil"
  start_bg anvil "$LOGS/anvil.log" "$ANVIL" --chain-id "$L1_CHAIN_ID" --no-mining \
    --timestamp "$L1_GENESIS_TS" --port "$P_ANVIL"
  wait_listen "$P_ANVIL" 15 || { tail_log "$LOGS/anvil.log"; die "anvil did not listen on $P_ANVIL"; }
  wait_rpc "$L1_RPC" 15 || { tail_log "$LOGS/anvil.log"; die "anvil RPC not ready"; }
  local ts0; ts0="$(block_ts 0)"
  [ "$ts0" = "$L1_GENESIS_TS" ] || die "anvil genesis ts=$ts0 != configured $L1_GENESIS_TS"

  # ---- 2) 确定性锚点：把 L1 头逐块精确矿到 pin_block 并停住 ----
  # 每块 ts 精确 = genesis_ts + block_time*i（evm_setNextBlockTimestamp 指定），
  # apply 入口读到的 L1 起始块 ts 因此跨 run 完全一致 = 产物幂等的锚。
  STEP="pin-l1-head"
  local i
  for i in $(seq 1 "$PIN_BLOCK"); do
    mine_next $(( L1_GENESIS_TS + L1_BLOCK_TIME * i ))
  done
  local h; h=$(( $(head_number) ))
  [ "$h" -eq "$PIN_BLOCK" ] || die "pinned head=$h != $PIN_BLOCK"
  local pin_ts; pin_ts="$(head_ts)"
  [ "$pin_ts" = "$EXPECTED_L2_TIME" ] || die "pinned L1 head ts=$pin_ts != expected $EXPECTED_L2_TIME (config anchor drifted)"
  log "L1 head pinned: block $PIN_BLOCK ts=$pin_ts (=this run's future rollup genesis.l2_time)"

  # ---- 3) op-deployer init + intent 生成 + apply ----
  STEP="deployer-init"
  local WD="$RUN/deployer"
  "$OPDEPLOYER" init --l1-chain-id "$L1_CHAIN_ID" --l2-chain-ids "$L2_CHAIN_ID" \
    --intent-type custom --workdir "$WD" >"$LOGS/deployer-init.log" 2>&1 \
    || { tail_log "$LOGS/deployer-init.log"; die "op-deployer init failed"; }
  generate_intent "$WD/intent.toml"
  log "intent.toml generated (fork offsets + roles from devnet.toml; 无 customGasToken 段 —— 空 Name 会被校验拒)"
  # 固定 CREATE2 盐：init 写 0x0，apply 见零盐会随机生成并持久化 —— 代理地址（
  # L1CrossDomainMessengerProxy 等 3 个 storage 引用）每次漂移，是 genesis hash 幂等的头号杀手
  STEP="pin-create2-salt"
  local salt; salt="$(toml_get meta create2_salt)"
  [ -n "$salt" ] || die "meta.create2_salt not set in $TOML"
  python3 -c "
import json
p = '$WD/state.json'
s = json.load(open(p))
s['create2Salt'] = '$salt'
json.dump(s, open(p, 'w'))"
  log "create2Salt pinned: $salt"

  STEP="deployer-apply"
  nohup "$OPDEPLOYER" apply --workdir "$WD" --l1-rpc-url "$L1_RPC" --private-key "$DEPLOYER_KEY" \
    >"$LOGS/deployer-apply.log" 2>&1 &
  local apply_pid=$!
  sleep 2   # deployer 在入口读 L1 头作为「L1 起始块」——此刻头仍停在 pin_block
  ( while kill -0 "$apply_pid" 2>/dev/null; do mine_next $(( $(head_ts) + L1_BLOCK_TIME )); sleep 1; done ) &   # apply 代矿循环（ts 仍精确 parent+2）
  local miner_pid=$!
  echo "$miner_pid" >"$RUN/miner.pid"
  local rc=0
  wait "$apply_pid" || rc=$?
  kill "$miner_pid" 2>/dev/null || true; wait "$miner_pid" 2>/dev/null || true
  rm -f "$RUN/miner.pid"
  [ "$rc" -eq 0 ] || { tail_log "$LOGS/deployer-apply.log" 40; die "op-deployer apply failed (rc=$rc)"; }
  log "apply done: $(grep -c '' "$LOGS/deployer-apply.log") log lines; L1 head now block $(head_number)"

  # ---- 4) inspect 取产物 ----
  STEP="inspect"
  "$OPDEPLOYER" inspect genesis --workdir "$WD" "$L2_CHAIN_ID" 2>"$LOGS/inspect-genesis.err" >"$ART/genesis.json"
  "$OPDEPLOYER" inspect rollup  --workdir "$WD" "$L2_CHAIN_ID" 2>"$LOGS/inspect-rollup.err"  >"$ART/rollup.json"
  jq -e '.genesis.l2_time' "$ART/rollup.json" >/dev/null 2>&1 || { tail_log "$LOGS/inspect-rollup.err"; die "inspect rollup output invalid"; }

  # ---- 5) 产物归一化到 toml 锚点（README「post-edit 路径」定案：时间数值一致即等价）----
  # deployer 的 set-start-block 策略=live：读 apply 部署完成后的 L1 头作为起始块（实测 stage 日志），
  # 该时刻随代矿节奏漂移 → 产物 l2_time 每次 up 不同。归一化：
  #   fork 时间整体平移到 l2_time = anvil genesis ts + block_time*pin_block（toml 锚），
  #   并把 rollup genesis.l1 重指到 pin_block（其 ts 恰 = 新 l2_time，保持推导起点构造一致）。
  STEP="normalize-artifacts"
  local pin_hash; pin_hash="$(rpc_res "$L1_RPC" eth_getBlockByNumber "$(printf '["0x%x", false]' "$PIN_BLOCK")" | jq -r '.hash')"
  normalize_artifacts "$pin_hash"
  log "artifacts normalized to anchor l2_time=$EXPECTED_L2_TIME, rollup genesis.l1 -> block $PIN_BLOCK"

  write_l1_chain_config "$ART/l1-chain-config.json"

  # ---- 6) 产物校验：fork 时间 = apply 时刻 L1 起始块 ts + 各偏移（脚本锚） ----
  STEP="verify-artifacts"
  verify_artifacts

  # ---- 7) L1 时钟策略：默认 up 跳时钟把 L1 头推过最后边界；campaign 不跳（边界要留给墙钟）----
  STEP="l1-clock"
  local a b ok="" i
  if [ "$CAMPAIGN" -eq 1 ]; then
    # campaign：L1 保持墙钟节奏（genesis 在过去 500s，逐块 parent+2 逐步追平墙钟），
    # 不做跳时钟 —— 链时间贴墙钟走，fork 边界按墙钟逐段到来
    evm_setIntervalMining_quiet "$L1_BLOCK_TIME"   # tick 块 ts 恒 = parent+2（Task 1 实测与 --block-time 等价）
    for i in $(seq 1 20); do
      sleep 1
      b="$(head_ts)"; a="$(block_ts $(( $(head_number) - 1 )))"
      if [ $((b - a)) -eq "$L1_BLOCK_TIME" ]; then ok=1; break; fi
    done
    [ -n "$ok" ] || die "steady-state tick mining did not produce parent+2 blocks (last diff=$((b-a)), want $L1_BLOCK_TIME)"
    log "campaign: no L1 jump; steady interval mining ${L1_BLOCK_TIME}s, L1 head ts=$b (wall clock $(date +%s))"
  else
  local jovian_off; jovian_off="$(toml_get forks jovian)"
  local last_boundary=$(( $(jq -r '.genesis.l2_time' "$ART/rollup.json") + jovian_off ))
  local need=$(( last_boundary + MARGIN_S - pin_ts ))
  local k=$(( (need + JUMP_S - 1) / JUMP_S + 1 ))
  local nt jts; nt="$(head_ts)"
  i=0; while [ "$i" -lt "$k" ]; do nt=$(( nt + JUMP_S )); mine_next "$nt"; i=$((i+1)); done
  jts="$(head_ts)"
  [ "$jts" -gt "$last_boundary" ] || die "after jump L1 head ts=$jts not past last boundary $last_boundary"
  evm_setIntervalMining_quiet "$L1_BLOCK_TIME"   # 恢复稳态：与 Task 1 的 --block-time 2 等价（tick 块 ts 恒 = parent+2，Task 1 实测）
  for i in $(seq 1 20); do
    sleep 1
    b="$(head_ts)"; a="$(block_ts $(( $(head_number) - 1 )))"
    if [ $((b - a)) -eq "$L1_BLOCK_TIME" ]; then ok=1; break; fi
  done
  [ -n "$ok" ] || die "steady-state tick mining did not produce parent+2 blocks (last diff=$((b-a)), want $L1_BLOCK_TIME)"
  log "L1 head ts=$b past last boundary $last_boundary (jumped $k blocks x ${JUMP_S}s); steady interval mining ${L1_BLOCK_TIME}s"
  fi

  # ---- 7) L2: geth init + start ----
  STEP="geth"
  printf '%s\n' "$JWT" >"$RUN/jwt.txt"; chmod 600 "$RUN/jwt.txt"
  "$GETH" --datadir "$L2DIR" --state.scheme hash init "$ART/genesis.json" >"$LOGS/geth-init.log" 2>&1 \
    || { tail_log "$LOGS/geth-init.log"; die "geth init failed"; }
  start_bg geth "$LOGS/geth.log" "$GETH" --datadir "$L2DIR" \
    --http --http.port "$P_GETH_HTTP" --http.api eth,debug,net,web3 \
    --authrpc.port "$P_GETH_AUTH" --authrpc.jwtsecret "$RUN/jwt.txt" \
    --ws --ws.port "$P_GETH_WS" \
    --state.scheme hash --gcmode archive --syncmode full --nodiscover --port 0 \
    --rollup.disabletxpoolgossip
  # 注意 1：不给 --rollup.sequencerhttp。op-geth 的 SendTx 在该旗标存在时把用户 raw tx
  # 转发到指定端点（eth/api_backend.go SendTx）；单节点 devnet 指向 op-node 时转发必败
  # （op-node 无 eth_sendRawTransaction，-32601），用户 RPC 发交易整条不可用（P1-3 实测）。
  # 单 sequencer 语义 = 用户交易进本地 txpool，由本节点 sequencer 出块。
  # 注意 2（P3-1 实测定案）：--gcmode archive 单独不够。本 geth 缺省 state scheme=path，
  # 路径库的历史状态随机读只覆盖 recent diff-layer 窗口（实测恰 128 块：
  # head-129 的 debug_accountRange/eth_getBalance 即报 missing trie node /
  # historical state not available；--history.state 0 只扩 history journal，实测
  # 不解锁 RPC 随机读）。chainexport 需要在任意历史块上导全量状态，改用
  # --state.scheme hash + --gcmode archive（经典归档语义，全史可查）。
  # 改 scheme 必须重新 init：opdevnet down 已删 L2 datadir，up 每次全新 init，无残留。
  wait_rpc "$L2_HTTP" 30 || { tail_log "$LOGS/geth.log" 40; die "geth HTTP not ready on $P_GETH_HTTP"; }
  wait_listen "$P_GETH_AUTH" 10 || { tail_log "$LOGS/geth.log" 40; die "geth authrpc not listening on $P_GETH_AUTH"; }
  wait_listen "$P_GETH_WS" 10 || { tail_log "$LOGS/geth.log" 40; die "geth ws not listening on $P_GETH_WS"; }
  local gid; gid="$(rpc_res "$L2_HTTP" eth_chainId)"
  [ "$gid" = "$(printf '0x%x' "$L2_CHAIN_ID")" ] || die "geth chainId=$gid != $L2_CHAIN_ID"

  # ---- 8b) 回填 rollup genesis.l2.hash（幂等核心之二）----
  # 归一化改了 genesis.json 内容 → 实际 genesis 块 hash 变化；deployer 预计算的旧 hash 会让
  # op-node 报 "expected L2 genesis hash to match"（run8 实测）。不重新实现 RLP —— 让 geth
  # 自己算：读链上 block 0 真实 hash 回填 rollup.json，再起 op-node。
  STEP="patch-rollup-genesis-hash"
  local ghash; ghash="$(rpc_res "$L2_HTTP" eth_getBlockByNumber '["0x0", false]' | jq -r '.hash')"
  jq --arg h "$ghash" '.genesis.l2.hash = $h' "$ART/rollup.json" >"$ART/rollup.json.tmp" \
    && mv "$ART/rollup.json.tmp" "$ART/rollup.json"
  log "rollup genesis.l2.hash = geth block0 = $ghash"
  if [ -n "$prev_hash" ]; then
    if [ "$ghash" = "$prev_hash" ]; then
      log "IDEMPOTENT: L2 genesis hash identical to previous run: $ghash"
    else
      log "WARN: L2 genesis hash CHANGED vs previous run: $prev_hash -> $ghash"
    fi
  else
    log "first run with this anchor; re-up will compare genesis hash for idempotency"
  fi

  # ---- 9) op-node（sequencer）----
  STEP="op-node"
  start_bg opnode "$LOGS/opnode.log" "$OPNODE" \
    --l2 "http://127.0.0.1:$P_GETH_AUTH" --l2.jwt-secret "$RUN/jwt.txt" \
    --l1 "$L1_RPC" --l1.beacon.ignore \
    --rollup.l1-chain-config "$ART/l1-chain-config.json" \
    --sequencer.enabled --sequencer.l1-confs 0 \
    --rollup.config "$ART/rollup.json" --rpc.port "$P_OPNODE"
  wait_listen "$P_OPNODE" 30 || { tail_log "$LOGS/opnode.log" 40; die "op-node rpc not listening on $P_OPNODE"; }
  # op-node RPC 没有 eth_chainId，用 optimism_syncStatus 探活（run6 教训：探针方法要对组件语义）
  wait_rpc "$OPNODE_RPC" 30 optimism_syncStatus || { tail_log "$LOGS/opnode.log" 40; die "op-node rpc not ready"; }

  # ---- 9) op-batcher ----
  STEP="op-batcher"
  start_bg batcher "$LOGS/batcher.log" "$OPBATCHER" \
    --l1-eth-rpc "$L1_RPC" --l2-eth-rpc "$L2_HTTP" --rollup-rpc "$OPNODE_RPC" \
    --private-key "$BATCHER_KEY" \
    --max-channel-duration "$MAX_CHAN_DUR" --data-availability-type "$DA_TYPE" \
    --throttle.unsafe-da-bytes-lower-threshold 0 --rpc.port "$P_BATCHER"
  wait_listen "$P_BATCHER" 20 || { tail_log "$LOGS/batcher.log" 40; die "batcher rpc not listening on $P_BATCHER"; }
  # batcher 默认 --rpc.port 8545 与 anvil 撞（Task 1 实测），配置固定 8548

  write_env "$ghash"

  # ---- 10) 等待 L2 追到目标；campaign 以「头块时间追平墙钟」为准，默认按块数+激活块检查 ----
  STEP="l2-catchup"
  local deadline=$(( $(date +%s) + CATCHUP_TIMEOUT )) last_reported=0 blk
  if [ "$CAMPAIGN" -eq 1 ]; then
    # campaign：sequencer 以机器速度从 genesis 追墙钟，追平后转入实时出块。
    # 头块 ts 进入墙钟 ±DRIFT 即认为 catch-up 完成（不设块数目标 —— 边界留给墙钟逐段穿越）。
    log "campaign: waiting L2 head time within ${CAMPAIGN_CATCHUP_DRIFT_S}s of wall clock ..."
    local hts wall
    while :; do
      for f in anvil geth opnode batcher; do
        pid_alive "$RUN/$f.pid" || { tail_log "$LOGS/$f.log" 40; die "$f died during catch-up"; }
      done
      blk=$(( $(rpc_res "$L2_HTTP" eth_blockNumber 2>/dev/null || echo 0) ))
      hts=$(( $(rpc_res "$L2_HTTP" eth_getBlockByNumber "$(printf '["0x%x", false]' "$blk")" 2>/dev/null | jq -r '.timestamp // 0') ))
      wall="$(date +%s)"
      if [ "$blk" -gt 0 ] && [ $(( wall - hts )) -le "$CAMPAIGN_CATCHUP_DRIFT_S" ]; then
        log "campaign catch-up done: head=$blk head_ts=$hts wall=$wall (behind $((wall-hts))s)"
        break
      fi
      [ "$(date +%s)" -gt "$deadline" ] && { tail_log "$LOGS/opnode.log" 30; die "campaign catch-up timeout after ${CATCHUP_TIMEOUT}s (head=$blk head_ts=${hts:-?})"; }
      sleep 2
    done
    STEP="final-sync"
    local sync safe unsafe
    sync="$(rpc_res "$OPNODE_RPC" optimism_syncStatus)"
    safe=$(( $(printf '%s' "$sync" | jq -r '.safe_l2.number') ))
    unsafe=$(( $(printf '%s' "$sync" | jq -r '.unsafe_l2.number') ))
    log "sync: safe=$safe unsafe=$unsafe (gap $((unsafe-safe)))"
    log "CAMPAIGN UP OK — 激活块计数检查跳过（边界未到，属预期；逐段覆盖由 txsource runner 验证）"
    echo
    log "UP OK (campaign) — L2 genesis hash $ghash"
    log "  L2 RPC      $L2_HTTP   (ws :$P_GETH_WS, engine :$P_GETH_AUTH)"
    log "  op-node RPC $OPNODE_RPC"
    log "  artifacts   $ART (genesis.json / rollup.json / l1-chain-config.json)"
    log "  logs        $LOGS"
    log "  fork 段表（相对 l2_time=$(jq -r '.genesis.l2_time' "$ART/rollup.json")）："
    local f t
    for f in $RUN_FORKS; do
      t="$(jq -r ".${f}_time // empty" "$ART/rollup.json" 2>/dev/null || true)"
      [ -n "$t" ] && log "    $f @ ts $t (block ~$((( t - $(jq -r '.genesis.l2_time' "$ART/rollup.json") ) / 2)))"
    done
    log "  next: ./opdevnet.sh status | txsource: cd ../txsource && ./run.sh --plugin ..."
    return 0
  fi
  log "waiting L2 catch-up to block $TARGET_BLOCKS (machine speed ~115 blk/s after boundaries crossed) ..."
  while :; do
    for f in anvil geth opnode batcher; do
      pid_alive "$RUN/$f.pid" || { tail_log "$LOGS/$f.log" 40; die "$f died during catch-up"; }
    done
    blk=$(( $(rpc_res "$L2_HTTP" eth_blockNumber 2>/dev/null || echo 0) ))
    if [ $(( blk - last_reported )) -ge 500 ] || [ "$blk" -ge "$TARGET_BLOCKS" ]; then
      log "L2 head: $blk / $TARGET_BLOCKS"; last_reported="$blk"
    fi
    [ "$blk" -ge "$TARGET_BLOCKS" ] && break
    [ "$(date +%s)" -gt "$deadline" ] && { tail_log "$LOGS/opnode.log" 30; die "catch-up timeout after ${CATCHUP_TIMEOUT}s (head=$blk)"; }
    sleep 2
  done

  STEP="activation-check"
  log "running activation-block check (expect upgrade tx counts: $UPGRADE_COUNTS)"
  local chk_rc=0
  python3 "$DIR/check_activations.py" --rollup "$ART/rollup.json" --rpc "$L2_HTTP" \
    | tee "$LOGS/activation-check.log" || chk_rc=$?
  [ "$chk_rc" -eq 0 ] || { tail_log "$LOGS/opnode.log" 20; die "activation check failed (rc=$chk_rc), see $LOGS/activation-check.log"; }

  # ---- safe/unsafe 收尾快照 ----
  STEP="final-sync"
  local sync safe unsafe
  sync="$(rpc_res "$OPNODE_RPC" optimism_syncStatus)"
  safe=$(( $(printf '%s' "$sync" | jq -r '.safe_l2.number') ))
  unsafe=$(( $(printf '%s' "$sync" | jq -r '.unsafe_l2.number') ))
  log "sync: safe=$safe unsafe=$unsafe (gap $((unsafe-safe)))"

  echo
  log "UP OK — L2 genesis hash $ghash"
  log "  L2 RPC      $L2_HTTP   (ws :$P_GETH_WS, engine :$P_GETH_AUTH)"
  log "  op-node RPC $OPNODE_RPC"
  log "  artifacts   $ART (genesis.json / rollup.json / l1-chain-config.json)"
  log "  logs        $LOGS"
  log "  next: ./opdevnet.sh status | ./opdevnet.sh down"
}

evm_setIntervalMining_quiet() { rpc "$L1_RPC" evm_setIntervalMining "[$1]" >/dev/null; }

generate_intent() { # $1=output path
  local out="$1"
  local cb_esc="$CB"
  {
    echo 'configType = "custom"'
    echo 'opDeployerVersion = "v0.0.0-dev"'
    echo "l1ChainID = $L1_CHAIN_ID"
    echo 'fundDevAccounts = false'
    echo "l1ContractsLocator = \"file://$cb_esc\""
    echo "l2ContractsLocator = \"file://$cb_esc\""
    echo
    echo '[globalDeployOverrides]'
    local f off
    for f in $RUN_FORKS; do
      off="$(toml_get forks "$f")"
      # CanyonTimeOffset -> l2GenesisCanyonTimeOffset
      printf '  l2Genesis%sTimeOffset = "0x%x"\n' "$(printf '%s' "$f" | awk '{print toupper(substr($0,1,1)) substr($0,2)}')" "$off"
    done
    echo
    echo '[superchainRoles]'
    printf '  SuperchainProxyAdminOwner = "%s"\n' "$(toml_get superchain_roles superchain_proxy_admin_owner)"
    printf '  SuperchainGuardian = "%s"\n'       "$(toml_get superchain_roles superchain_guardian)"
    printf '  ProtocolVersionsOwner = "%s"\n'     "$(toml_get superchain_roles protocol_versions_owner)"
    printf '  Challenger = "%s"\n'                "$(toml_get superchain_roles challenger)"
    echo
    echo '[[chains]]'
    printf '  id = "0x%064x"\n' "$L2_CHAIN_ID"
    printf '  baseFeeVaultRecipient = "%s"\n'      "$(toml_get recipients base_fee_vault)"
    printf '  l1FeeVaultRecipient = "%s"\n'        "$(toml_get recipients l1_fee_vault)"
    printf '  sequencerFeeVaultRecipient = "%s"\n' "$(toml_get recipients sequencer_fee_vault)"
    printf '  operatorFeeVaultRecipient = "%s"\n'  "$(toml_get recipients operator_fee_vault)"
    printf '  eip1559DenominatorCanyon = %s\n'     "$(toml_get l2 eip1559_denominator_canyon)"
    printf '  eip1559Denominator = %s\n'           "$(toml_get l2 eip1559_denominator)"
    printf '  eip1559Elasticity = %s\n'            "$(toml_get l2 eip1559_elasticity)"
    printf '  gasLimit = %s\n'                     "$(toml_get l2 gas_limit)"
    printf '  operatorFeeScalar = %s\n'            "$(toml_get l2 operator_fee_scalar)"
    printf '  operatorFeeConstant = %s\n'          "$(toml_get l2 operator_fee_constant)"
    printf '  chainFeesRecipient = "%s"\n'         "$(toml_get recipients chain_fees)"
    printf '  minBaseFee = %s\n'                   "$(toml_get l2 min_base_fee)"
    printf '  daFootprintGasScalar = %s\n'         "$(toml_get l2 da_footprint_gas_scalar)"
    echo '  [chains.roles]'
    printf '    l1ProxyAdminOwner = "%s"\n' "$(toml_get chain_roles l1_proxy_admin_owner)"
    printf '    l2ProxyAdminOwner = "%s"\n' "$(toml_get chain_roles l2_proxy_admin_owner)"
    printf '    systemConfigOwner = "%s"\n' "$(toml_get chain_roles system_config_owner)"
    printf '    unsafeBlockSigner = "%s"\n' "$(toml_get chain_roles unsafe_block_signer)"
    printf '    batcher = "%s"\n'           "$(toml_get chain_roles batcher)"
    printf '    proposer = "%s"\n'          "$(toml_get chain_roles proposer)"
    printf '    challenger = "%s"\n'        "$(toml_get chain_roles challenger)"
  } >"$out"
  # Task 1 实测：显式不给 [chains.customGasToken]（空 Name 会 fail: CustomGasToken.Name must be set）
  if grep -q 'customGasToken' "$out"; then die "intent template unexpectedly contains customGasToken"; fi
  return 0
}

write_l1_chain_config() { # $1=out —— Task 1 实测文件原样（anvil ChainConfig，须含 blobSchedule，
                          # chain-id 31337 不在 op-node 已知注册表）
  cat >"$1" <<'EOF'
{
  "chainId": 31337,
  "homesteadBlock": 0,
  "eip150Block": 0,
  "eip155Block": 0,
  "eip158Block": 0,
  "byzantiumBlock": 0,
  "constantinopleBlock": 0,
  "petersburgBlock": 0,
  "istanbulBlock": 0,
  "berlinBlock": 0,
  "londonBlock": 0,
  "arrowGlacierBlock": 0,
  "grayGlacierBlock": 0,
  "mergeNetsplitBlock": 0,
  "terminalTotalDifficulty": 0,
  "shanghaiTime": 0,
  "cancunTime": 0,
  "pragueTime": 0,
  "ethash": {},
  "blobSchedule": {
    "cancun": { "target": 3, "max": 6, "baseFeeUpdateFraction": 3338477 },
    "prague": { "target": 6, "max": 9, "baseFeeUpdateFraction": 5007716 }
  }
}
EOF
}

normalize_artifacts() { # $1=pin_block hash —— 平移 fork 时间到锚点 + 重指 genesis.l1（幂等核心）
  local offsets_json="{" first=1 f off
  for f in $RUN_FORKS; do
    off="$(toml_get forks "$f")"
    [ $first -eq 1 ] || offsets_json+=","
    offsets_json+="\"$f\":$off"; first=0
  done
  offsets_json+="}"
  python3 - "$ART/rollup.json" "$ART/genesis.json" "$offsets_json" "$EXPECTED_L2_TIME" "$PIN_BLOCK" "$1" <<'PYEOF'
import json, sys
rollup_p, genesis_p, offsets, expected, pin_n, pin_h = (
    sys.argv[1], sys.argv[2], json.loads(sys.argv[3]), int(sys.argv[4]), int(sys.argv[5]), sys.argv[6])
r = json.load(open(rollup_p)); g = json.load(open(genesis_p)); cfg = g["config"]
actual = r["genesis"]["l2_time"]
shift = expected - actual
if shift:
    for name in offsets:
        k = f"{name}_time"
        if r.get(k) is not None:
            r[k] += shift
    # geth 侧: 阶梯 fork + shanghai/cancun/prague 映射（Task 1: shanghai=canyon, cancun=ecotone, prague=isthmus）
    for name in offsets:
        k = f"{name}Time"
        if k in cfg:
            cfg[k] += shift
    for k in ("shanghaiTime", "cancunTime", "pragueTime"):
        if k in cfg:
            cfg[k] += shift
r["genesis"]["l2_time"] = expected
r["genesis"]["l1"]["number"] = pin_n
r["genesis"]["l1"]["hash"] = pin_h
# geth genesis 头部 timestamp 也是 apply 时刻值（run7 教训：漏掉它则整条 L2 链阶梯相对 rollup 平移，
# 激活块全部错位）；保持原十六进制格式
g["timestamp"] = hex(expected)
json.dump(r, open(rollup_p, "w"), indent=2)
json.dump(g, open(genesis_p, "w"))
print(f"normalized: l2_time {actual} -> {expected} (shift {shift}), genesis.l1 -> ({pin_n}, {pin_h[:18]}...), geth genesis timestamp -> {hex(expected)}")
PYEOF
}

verify_artifacts() { # 校验 fork 时间锚：rollup.<f>_time == genesis.l2_time + toml 偏移，
                     # 且 geth genesis.json config 的 <f>Time 与 rollup 完全一致
  local offsets_json="{" first=1 f off
  for f in $RUN_FORKS; do
    off="$(toml_get forks "$f")"
    [ $first -eq 1 ] || offsets_json+=","
    offsets_json+="\"$f\":$off"; first=0
  done
  offsets_json+="}"
  python3 - "$ART/rollup.json" "$ART/genesis.json" "$offsets_json" "$EXPECTED_L2_TIME" <<'PYEOF'
import json, sys
rollup_p, genesis_p, offsets, expected_l2t = sys.argv[1], sys.argv[2], json.loads(sys.argv[3]), int(sys.argv[4])
r = json.load(open(rollup_p)); cfg = json.load(open(genesis_p))["config"]
l2t = r["genesis"]["l2_time"]
bad = 0
print(f"{'fork':10} {'offset':>6} {'expected':>11} {'rollup':>11} {'gethCfg':>11}  verdict")
if l2t != expected_l2t:
    print(f"ANCHOR MISMATCH: rollup genesis.l2_time={l2t} != expected {expected_l2t} "
          f"(anvil genesis ts + block_time * pin_block)"); bad += 1
for name, off in offsets.items():
    exp = l2t + off
    rt = r.get(f"{name}_time")
    # Task 1 定案 2: geth chain config 没有 deltaTime 字段（delta 只在 rollup.json）
    gt_exp = None if name == "delta" else exp
    gt = cfg.get(f"{name}Time")
    ok = (rt == exp) and (gt == gt_exp)
    if not ok: bad += 1
    print(f"{name:10} {off:>6} {exp:>11} {str(rt):>11} {str(gt if gt is not None else '-'):>11}  {'OK' if ok else 'FAIL'}")
maps = [("shanghaiTime","canyonTime"),("cancunTime","ecotoneTime"),("pragueTime","isthmusTime")]
for a, b in maps:
    if cfg.get(a) != cfg.get(b):
        print(f"MAPPING FAIL: {a}={cfg.get(a)} != {b}={cfg.get(b)}"); bad += 1
if cfg.get("deltaTime") is not None:
    print("geth config has deltaTime but Task 1 定案: 无该字段（delta 只在 rollup.json）"); bad += 1
if r.get("delta_time") is None:
    print("rollup.json missing delta_time — op-node checkFork 会报 prior fork delta missing"); bad += 1
if bad:
    print(f"ARTIFACT VERIFY: {bad} failure(s)"); sys.exit(1)
print("ARTIFACT VERIFY: all fork times = l2_time + offset, rollup/geth consistent, delta present")
PYEOF
}

write_env() { # $1=genesis hash
  cat >"$RUN/env" <<EOF
RUNTIME=$RUNTIME
ART=$ART
LOGS=$LOGS
L2DIR=$L2DIR
P_ANVIL=$P_ANVIL
P_GETH_HTTP=$P_GETH_HTTP
P_GETH_AUTH=$P_GETH_AUTH
P_GETH_WS=$P_GETH_WS
P_OPNODE=$P_OPNODE
P_BATCHER=$P_BATCHER
L1_RPC=$L1_RPC
L2_HTTP=$L2_HTTP
OPNODE_RPC=$OPNODE_RPC
BATCHER_RPC=$BATCHER_RPC
L1_CHAIN_ID=$L1_CHAIN_ID
L2_CHAIN_ID=$L2_CHAIN_ID
L2_TIME=$(jq -r '.genesis.l2_time' "$ART/rollup.json")
GENESIS_HASH=$1
TARGET_BLOCKS=$TARGET_BLOCKS
CAMPAIGN=$CAMPAIGN
TOML=$TOML
EOF
}

# ---------- status ----------
cmd_status() {
  load_config
  if [ ! -f "$RUN/env" ]; then
    echo "opdevnet: DOWN (no run state under $RUN)"
    echo "runtime: $RUNTIME (artifacts: $([ -d "$ART" ] && echo present || echo none))"
    exit 0
  fi
  # shellcheck disable=SC1091
  . "$RUN/env"
  local fmt='{"alive":%s,"pid":%s,"port":%s}\n'
  local overall=0
  echo "opdevnet ($RUNTIME)"
  echo "  genesis hash : $GENESIS_HASH"
  echo "  rollup l2_time: $L2_TIME"
  printf '  %-9s %-8s %-7s %-6s %s\n' COMPONENT PID ALIVE PORT NOTE
  local comp pidfile pid alive note port
  for comp in anvil geth opnode batcher; do
    pidfile="$RUN/$comp.pid"
    pid="-"; alive="no"; note=""
    if [ -f "$pidfile" ]; then
      pid="$(cat "$pidfile")"
      kill -0 "$pid" 2>/dev/null && alive="yes" || { alive="DEAD"; overall=1; }
    else
      overall=1
    fi
    case "$comp" in
      anvil)   port=$P_ANVIL ;;
      geth)    port=$P_GETH_HTTP; port_busy "$P_GETH_AUTH" && note="engine:$P_GETH_AUTH ok"; port_busy "$P_GETH_WS" && note="$note ws:$P_GETH_WS ok" ;;
      opnode)  port=$P_OPNODE ;;
      batcher) port=$P_BATCHER ;;
    esac
    [ -n "$port" ] && port_busy "$port" && note="listen:$port ${note:+$note}"
    [ "$alive" = "yes" ] && [ -z "$note" ] && { note="port $port NOT listening"; overall=1; }
    printf '  %-9s %-8s %-7s %-6s %s\n' "$comp" "$pid" "$alive" "${port:-}" "$note"
  done

  # L1 / L2 链状态
  local l1n l1ts l2n l2ts
  l1n="$(rpc_res "$L1_RPC" eth_blockNumber 2>/dev/null || true)"
  if [ -n "$l1n" ]; then
    l1n="$(dec "$l1n")"
    l1ts="$(dec "$(rpc_res "$L1_RPC" eth_getBlockByNumber "$(printf '["0x%x", false]' "$l1n")" 2>/dev/null | jq -r '.timestamp' || true)")"
    echo "  L1 head   : block $l1n ts=$l1ts (anvil :$P_ANVIL)"
  fi
  l2n="$(rpc_res "$L2_HTTP" eth_blockNumber 2>/dev/null || true)"
  if [ -n "$l2n" ]; then
    l2n="$(dec "$l2n")"
    l2ts="$(dec "$(rpc_res "$L2_HTTP" eth_getBlockByNumber "$(printf '["0x%x", false]' "$l2n")" 2>/dev/null | jq -r '.timestamp' || true)")"
    local seg next_name next_t
    seg="Bedrock"; next_name="-"; next_t=""
    local f t prev_t=0
    for f in $RUN_FORKS; do
      t="$(jq -r ".${f}_time // empty" "$ART/rollup.json" 2>/dev/null || true)"
      [ -z "$t" ] && continue
      if [ "$l2ts" -ge "$t" ]; then seg="$(printf '%s' "$f" | awk '{print toupper(substr($0,1,1)) substr($0,2)}')"; prev_t="$t"; else next_name="$f"; next_t="$t"; break; fi
    done
    echo "  L2 unsafe : block $l2n ts=$l2ts fork=$seg"
    if [ -n "$next_t" ]; then
      echo "  next fork : $next_name at ts=$next_t (block ~$((( next_t - L2_TIME ) / 2)), in $(( next_t - l2ts ))s of chain time)"
    fi
  fi
  local sync safe unsafe
  sync="$(rpc_res "$OPNODE_RPC" optimism_syncStatus 2>/dev/null || true)"
  if [ -n "$sync" ]; then
    safe="$(dec "$(printf '%s' "$sync" | jq -r '.safe_l2.number')")"
    unsafe="$(dec "$(printf '%s' "$sync" | jq -r '.unsafe_l2.number')")"
    echo "  L2 safe   : block $safe   (unsafe-safe gap: $((unsafe - safe)))"
    echo "  batcher   : rpc :$P_BATCHER $(port_busy "$P_BATCHER" && echo ok || echo down)"
  else
    echo "  op-node sync status: UNAVAILABLE"; overall=1
  fi
  if [ "$overall" -eq 0 ]; then echo "  overall   : HEALTHY"; else echo "  overall   : DEGRADED"; fi
  exit "$overall"
}

# ---------- down ----------
cmd_down() {
  load_config
  STEP="down"
  if [ ! -d "$RUN" ]; then log "nothing to down (no run state)"; return 0; fi
  # 反序 kill：batcher -> op-node -> geth -> anvil（+ 可能残留的 apply 代矿循环）
  kill_pidfile "$RUN/batcher.pid" batcher
  kill_pidfile "$RUN/opnode.pid"  opnode
  kill_pidfile "$RUN/geth.pid"    geth
  kill_pidfile "$RUN/miner.pid"   miner
  kill_pidfile "$RUN/anvil.pid"   anvil
  # 端口清空核验（socket 释放比进程死亡略慢，给宽限重试）
  local p stuck="" i
  for i in $(seq 1 10); do
    stuck=""
    for p in "$P_BATCHER" "$P_OPNODE" "$P_GETH_HTTP" "$P_GETH_AUTH" "$P_GETH_WS" "$P_ANVIL"; do
      if port_busy "$p"; then stuck="$stuck $p"; fi
    done
    if [ -z "$stuck" ]; then break; fi
    sleep 0.5
  done
  if [ -n "$stuck" ]; then
    # 注意：${stuck} 必须加花括号——$stuck 后紧跟全角字符会被 bash 并进变量名，set -u 下崩（实测）
    log "WARN: ports still listening after kill:[${stuck}]（非本工具 pid 不动，请人工核查 lsof）"
  else
    log "all component ports released"
  fi
  # 清理：L2 datadir（链态可由产物重建）+ run 状态；产物与日志保留供导出
  rm -rf "$L2DIR" "$RUN"
  log "removed: L2 datadir + run state"
  log "kept:    $ART (genesis.json / rollup.json / l1-chain-config.json)"
  log "kept:    $LOGS"
}

cmd_clean() {
  cmd_down || true
  load_config
  rm -rf "$RUNTIME"
  log "cleaned entire runtime dir $RUNTIME"
}

case "${1:-}" in
  up)     shift; cmd_up "$@" ;;
  status) shift; cmd_status "$@" ;;
  down)   shift; cmd_down "$@" ;;
  clean)  shift; cmd_clean "$@" ;;
  -h|--help|help|"") usage ;;
  *) echo "unknown command: $1" >&2; usage ;;
esac
