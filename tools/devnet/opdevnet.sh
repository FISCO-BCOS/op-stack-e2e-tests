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
#                         --forks 只接受规范序 [regolith, canyon, ecotone, fjord, granite,
#                         holocene, isthmus, jovian] 从 regolith 开始的连续前缀（大小写不敏感，
#                         逗号/空格分隔）：真实链不能跳过 fork，前缀子集 = 合法真实链形态。
#                         前缀内的 fork 按压缩阶梯激活；前缀外的 fork 写「远未来」偏移
#                         （intent 物理上不能省略任何 fork —— op-deployer 对缺字段按 0 处理，
#                         会被严格递增校验拒绝），链在观察窗口内即一条合法前缀链。
#                         非前缀（跳变/乱序/未知名）→ 结构化报错，不留半状态。
#                         runtime_dir/端口/chain-id 与默认 up 相同 → status/down、txsource run.sh
#                         用缺省 devnet.toml 即可操作 campaign 栈。
#   ./opdevnet.sh status  组件进程/端口 + L2 块高 + 所在 fork 段 + safe/unsafe 差
#   ./opdevnet.sh down    反序 kill + 清理（产物 artifacts/ 与日志 logs/ 默认保留）
#   ./opdevnet.sh clean   down 后连运行时目录一起删除
#   ./opdevnet.sh export [chainexport 参数…]
#                         薄分发：调 chainexport/chainexport.bin 把运行中的链导出为 t8n 差分
#                         向量；参数（--from-block/--to-block 块范围、--poststate 等）见
#                         `opdevnet.sh export -h`。二进制不存在 → 提示按 chainexport/build.sh 重建。
#   ./opdevnet.sh check activations|distribution [参数…]
#                         薄分发：activations = 激活块升级交易计数检查（--min-user-txs 阈值等
#                         参数见 `opdevnet.sh check activations -h`）；distribution = 用户交易
#                         跨 fork 段分布检查（参数见 `opdevnet.sh check distribution -h`）。
#   ./opdevnet.sh txsource [run.sh 参数…]
#                         薄分发：调 txsource/run.sh 可插拔交易源执行器（参数见
#                         `opdevnet.sh txsource -h`）。up/status/down/clean 行为不变。
#   ./opdevnet.sh snapshot [--out-dir D] [chainexport 参数…]
#                         导出 + 自动注册一条龙（P5）：chainexport 导出（缺省 boundary
#                         采样；full 序列化超 95MiB 由其体积守卫自动降级）→ stem 命名 →
#                         写入 vectors/ → 替换旧 devnet 注册（旧文件 git rm + manifest/
#                         SHA256SUMS 整块 upsert，注册面只留最新一枚）→ out-dir 暂存区
#                         滚动保留最近 2 个导出 → git add -f 例外文件。提交仍手动。
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
#
# 健壮性加固（P4 审计定案，逐项有失败注入证据，见提交信息）：
#   - up 预检 fail-fast：versions.lock sha256 使用时断言、必需工具、devnet.toml 结构
#     （缺字段/坏值/端口重复/fork 阶梯乱序——旧版会静默算出垃圾锚点或穿透到 inspect 才炸）、
#     磁盘余量阈值（geth 磁余 <1.6GiB 自杀，默认 5GiB，DEVNET_MIN_FREE_DISK_GB 覆盖）、
#     端口空闲、up 互斥锁（mkdir + 陈旧 pid 抢占，封 preflight TOCTOU 窗口）。
#   - 失败传播：catch-up 完成前任何失败 → 自动清理本次启动的组件 + 非零退出 + 指向日志
#     （旧版留孤儿 anvil 监听）；catch-up 完成后的失败（激活计数不符）保留现场供取证。
#   - down/status/clean 不再要求二进制存在（/tmp 二进制重启即失，恢复路径不得被卡）；
#     pidfile 记 "pid comm"，kill 前校验进程身份，PID 复用时跳过并告警。
#   - inspect 步骤显式守卫（旧版 set -e 静默 exit 1，报错只落在 .err 侧文件）；
#     apply 记 pidfile（旧版 Ctrl-C 后成孤儿且 preflight 不可见）；apply 代矿循环监督
#     anvil 存活（旧版 anvil 死亡后 apply 无限挂死）。
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
TOML="${DEVNET_TOML:-$DIR/devnet.toml}"
LOCK_FROM_TOML=""
STEP="init"
FORKS="canyon delta ecotone fjord granite holocene isthmus jovian"
RUN_FORKS="$FORKS"                # 本次 up 生效的 fork 集合（恒为全表：campaign 前缀外 fork 也必须
                                  # 显式给偏移 —— 远未来值，见 generate_campaign_toml）
CANON_FORKS="regolith canyon ecotone fjord granite holocene isthmus jovian"
                                  # 规范 fork 序（= --forks 前缀校验的基准；regolith 恒在 genesis @0
                                  # 不在 [forks] 表内；delta 是本 worktree 的非标准 fork，不可点名，
                                  # 隐式跟随前缀：前缀含 ecotone 则入选，否则远未来）
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
CAMPAIGN_FORKS=""                 # 空 = 全 8 fork（= 最长前缀）；否则规范序连续前缀（逗号/空格分隔，
                                  # 大小写不敏感；parse_campaign_forks 校验后写入 CAMPAIGN_SEL/CAMPAIGN_FAR）
CAMPAIGN_SEL=""                   # 规范序入选前缀（含 regolith；空 = 未解析）
CAMPAIGN_FAR=""                   # 前缀之外的表内 fork（远未来偏移，运行窗口内不激活）
CAMPAIGN_SEG_S="300"
CAMPAIGN_ANCHOR_OFFSET_S="500"    # genesis_ts = now - 500
CAMPAIGN_TOML="${OPDEVNET_CAMPAIGN_TOML:-/tmp/opdevnet-campaign.toml}"
CAMPAIGN_CATCHUP_DRIFT_S="30"     # L2 头块时间追平到墙钟该误差内 = catch-up 完成

log()  { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
# 错误统一格式（P4 可观测性）：[opdevnet][ERROR] <step>: <message> (log: <path>)
#   <log> 来自最近一次 tail_log 的文件；die 前没 tail 过日志则省略 (log: …) 后缀。
#   只改错误行的呈现，退出码语义不变（仍 exit 1）。
_LAST_LOG=""
die()  {
  if [ -n "$_LAST_LOG" ]; then
    printf '[opdevnet][ERROR] %s: %s (log: %s)\n' "$STEP" "$*" "$_LAST_LOG" >&2
  else
    printf '[opdevnet][ERROR] %s: %s\n' "$STEP" "$*" >&2
  fi
  exit 1
}
stage() { # $1=k(1..6) $2=label —— up 六阶段标记（anvil→deployer→geth init→geth start→op-node→batcher）
  log "[stage $1/6] $2"
}
usage() { sed -n '2,43p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

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
  LOCK_FILE="$lock"
  # 任一二进制路径需要从 versions.lock 解析时，lock 文件必须存在（否则 awk 裸崩，
  # 报错不可读 —— robustness 审计 2.3）
  if [ ! -f "$lock" ]; then
    for k in anvil geth op_node op_batcher op_deployer; do
      [ -n "$(toml_get paths "$k")" ] || die "versions.lock not found: $lock (needed to resolve [paths].$k; fix paths.versions_lock or $DIR/versions.lock)"
    done
  fi
  ANVIL="$(toml_get paths anvil)";     [ -n "$ANVIL" ]     || ANVIL="$(lock_path anvil "$lock")"
  GETH="$(toml_get paths geth)";       [ -n "$GETH" ]      || GETH="$(lock_path geth "$lock")"
  OPNODE="$(toml_get paths op_node)";  [ -n "$OPNODE" ]    || OPNODE="$(lock_path op-node "$lock")"
  OPBATCHER="$(toml_get paths op_batcher)"; [ -n "$OPBATCHER" ] || OPBATCHER="$(lock_path op-batcher "$lock")"
  OPDEPLOYER="$(toml_get paths op_deployer)"; [ -n "$OPDEPLOYER" ] || OPDEPLOYER="$(lock_path op-deployer "$lock")"
  # 二进制可执行性只在 up 的 check_binaries 里断言（robustness 审计 MF1）：
  # down/status/clean 是重启后（/tmp 二进制必失）的恢复路径，不得因二进制缺失而罢工。
  [ -d "$CB" ] || die "contracts-bedrock not found: $CB"
  L2_HTTP="http://127.0.0.1:$P_GETH_HTTP"; L1_RPC="http://127.0.0.1:$P_ANVIL"
  OPNODE_RPC="http://127.0.0.1:$P_OPNODE"; BATCHER_RPC="http://127.0.0.1:$P_BATCHER"
  # EXPECTED_L2_TIME 在 cmd_up 的 validate_config 之后才计算（审计 4.1：坏 toml 若在此处
  # 做算术，bash 会以裸 arithmetic error 退出，结构校验永远轮不到跑）
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
    echo "$! $(basename "$1")" >"$RUN/$name.pid"   # "pid comm"：kill_pidfile 的进程身份校验依据
  )
  log "started $name (pid $(awk '{print $1}' "$RUN/$name.pid"), log $logf)"
}
# pidfile 格式（robustness 审计 MF4）："<pid> <comm-basename>"，如 "67551 geth"。
# kill 前用 ps -o comm 校验进程身份，防 PID 重启复用后杀无辜进程（实测：把无辜 sleep
# 的 pid 写进 geth.pid，旧 down 直接 SIGTERM+SIGKILL）。
pid_alive() { [ -f "$1" ] && kill -0 "$(awk '{print $1}' "$1")" 2>/dev/null; }
proc_comm_of_pid() { # 空 = 进程不存在（ps rc=1 经 pipefail 会传给 substitution，必须 || true 兜住，
                     # 否则 set -e 下 down 在「进程已死」的恢复路径上静默退出 1 —— 回归实测踩坑）
  ps -p "$1" -o comm= 2>/dev/null | awk -F/ '{print $NF}' || true
}
kill_pidfile() { # $1=pidfile $2=期望 comm basename（opnode→op-node 等；miner→bash）
  [ -f "$1" ] || return 0
  local pid want have
  pid="$(awk '{print $1}' "$1")"; want="$(awk '{print $2}' "$1")"
  [ -n "$want" ] || want="$2"
  have="$(proc_comm_of_pid "$pid")"
  rm -f "$1"
  if [ -z "$have" ]; then return 0; fi   # 进程已不存在
  if [ "$have" != "$want" ]; then
    log "WARN: pid $pid (from $1) now runs '$have', expected '$want' —— PID 已被复用，跳过 kill（如确需清理请人工 lsof）"
    return 0
  fi
  kill "$pid" 2>/dev/null || true
  local i=0
  while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 10 ]; do sleep 0.5; i=$((i+1)); done
  kill -9 "$pid" 2>/dev/null || true
}
# up 互斥锁（robustness 审计 1.1）：preflight 检查与首个 pidfile 落盘之间有 TOCTOU 窗口，
# 同秒并发双 up 会双双通过并互相覆盖 anvil.pid。mkdir 原子锁 + 陈旧检测（持锁 pid 死亡可抢）。
UP_LOCK=""
acquire_up_lock() {
  [ -n "$RUN" ] || die "up lock: RUNTIME not resolved"
  if mkdir "$RUN/.up-lock" 2>/dev/null; then echo "$$" >"$RUN/.up-lock/pid"; UP_LOCK="$RUN/.up-lock"; return 0; fi
  local lpid; lpid="$(awk '{print $1}' "$RUN/.up-lock/pid" 2>/dev/null || true)"   # pid 文件缺失时 awk rc=2，防 set -e 静默死
  if [ -n "$lpid" ] && ! kill -0 "$lpid" 2>/dev/null; then
    log "stale up-lock (holder pid $lpid gone) —— stealing"
    rm -rf "$RUN/.up-lock"
    if mkdir "$RUN/.up-lock" 2>/dev/null; then echo "$$" >"$RUN/.up-lock/pid"; UP_LOCK="$RUN/.up-lock"; return 0; fi
  fi
  die "another 'up' appears in progress (lock $RUN/.up-lock, holder pid ${lpid:-?}); 若确认无并发 up，可 rm -rf $RUN/.up-lock"
}
release_up_lock() { [ -n "$UP_LOCK" ] && rm -rf "$UP_LOCK"; UP_LOCK=""; }
# 失败自动清理（robustness 审计 MF3）：up 中途失败不再留孤儿进程/半状态（实测两场景：
# 坏 fork 阶梯 inspect 失败、坏 geth 二进制 —— 旧版都把 anvil 留在后台监听）。
# 只清理本次 up 启动的组件；artifacts/ 与 logs/ 保留（与 down 同语义）。
STARTED=""
mark_started() { STARTED="$1 $STARTED"; }   # 反序追加 → STARTED 天然就是 kill 顺序
UP_STACK_OK=0   # 1 = 栈完整起来且 catch-up 完成（此后失败保留现场，见 up_exit_trap）
up_failure_cleanup() {
  log "up failed —— auto-cleaning components started by this invocation:[ ${STARTED} ]"
  local f
  for f in $STARTED; do
    case "$f" in
      batcher) kill_pidfile "$RUN/batcher.pid" op-batcher ;;
      opnode)  kill_pidfile "$RUN/opnode.pid"  op-node ;;
      geth)    kill_pidfile "$RUN/geth.pid"    geth ;;
      miner)   kill_pidfile "$RUN/miner.pid"   bash ;;
      apply)   kill_pidfile "$RUN/apply.pid"   op-deployer ;;
      anvil)   kill_pidfile "$RUN/anvil.pid"   anvil ;;
    esac
  done
  rm -rf "$L2DIR" "$RUN"
  log "cleaned: started components + L2 datadir + run state; logs kept in $LOGS"
}
up_exit_trap() {
  local rc=$?
  trap - EXIT
  release_up_lock
  if [ "$rc" -ne 0 ] && [ -n "$STARTED" ]; then
    if [ "${UP_STACK_OK:-0}" -eq 1 ]; then
      # 栈已完整起来、catch-up 已完成（如激活块计数不符）：链在跑但语义异常 —— 保留现场
      # 供 fork 审计取证（README：链不匹配说明链有问题），只提示清理方式。
      log "up failed AFTER the stack fully came up —— stack left RUNNING for forensics; ./opdevnet.sh down to clean"
    else
      up_failure_cleanup
    fi
  fi
  exit "$rc"
}
# up 前置预检（robustness 审计 MF2/MF5/MF6）：fail-fast 带清晰错误
is_uint() { case "${1:-}" in ''|*[!0-9]*) return 1 ;; *) return 0 ;; esac; }
sha256_of() { # $1=file -> hash
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" 2>/dev/null | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" 2>/dev/null | awk '{print $1}'
  else die "no sha256 tool available (need sha256sum or shasum)"; fi
}
lock_sha() { awk -v k="$1" '$1==k { for (i=2;i<=NF;i++) if ($i ~ /^sha256=/) { sub(/^sha256=/,"",$i); print $i; exit } }' "$LOCK_FILE"; }
check_binaries() {
  local b hint
  for b in "$ANVIL" "$GETH" "$OPNODE" "$OPBATCHER" "$OPDEPLOYER"; do
    [ -x "$b" ] || {
      hint=""
      case "$b" in /tmp/*) hint=" —— /tmp 下二进制重启即失，按 versions.lock 的 src= 字段重建" ;; esac
      die "binary not executable: $b (check versions.lock / devnet.toml [paths])$hint"
    }
  done
  # 内容 pin 校验：versions.lock 的 sha256= 从「记录」升级为「使用时断言」（审计 2.1：
  # 截断/替换过的二进制旧版一路放行，直到组件崩溃才间接暴露）
  local key bin want got
  for pair in "geth:$GETH" "op-node:$OPNODE" "op-batcher:$OPBATCHER" "op-deployer:$OPDEPLOYER"; do
    key="${pair%%:*}"; bin="${pair#*:}"
    want="$(lock_sha "$key")"; [ -n "$want" ] || continue   # anvil/cast 等无 sha pin 的条目跳过
    got="$(sha256_of "$bin")"
    [ "$got" = "$want" ] || die "binary sha256 mismatch for $key: $bin
  lock expects $want
  actual     ${got:-<unreadable>}
  rebuild the binary per versions.lock src=, or fix versions.lock"
  done
  local t
  for t in curl jq python3 lsof; do
    command -v "$t" >/dev/null 2>&1 || die "required tool missing: $t"
  done
}
check_disk() { # 磁余阈值（审计 5.1：README 实测 geth 磁余 <1.6GiB 自杀；默认 5GiB 含运行时体量余量）
  local need_gb="${DEVNET_MIN_FREE_DISK_GB:-5}" avail_kb
  is_uint "$need_gb" || die "DEVNET_MIN_FREE_DISK_GB must be an integer, got '$need_gb'"
  mkdir -p "$RUNTIME"
  avail_kb="$(df -k "$RUNTIME" 2>/dev/null | awk 'NR==2{print $4}')"
  is_uint "$avail_kb" || die "cannot read free disk space for $RUNTIME"
  [ "$avail_kb" -ge $(( need_gb * 1024 * 1024 )) ] || \
    die "free disk on $RUNTIME filesystem: $(( avail_kb / 1024 ))MiB < required ${need_gb}GiB
  geth gracefully shuts down below ~1.6GiB free (README 运维踩坑); free space or override with DEVNET_MIN_FREE_DISK_GB"
}
validate_config() { # devnet.toml 结构校验（审计 4.1/4.2：缺字段旧版静默算出垃圾锚点
                    # l2_time=52；fork 阶梯乱序穿透到 inspect 才炸且报错不可见）
  local f off prev=0 p v
  for v in "meta.runtime_dir:$RUNTIME" "meta.create2_salt:$(toml_get meta create2_salt)" \
           "l1.chain_id:$L1_CHAIN_ID" "l1.genesis_timestamp:$L1_GENESIS_TS" "l1.block_time:$L1_BLOCK_TIME" \
           "l1.pin_block:$PIN_BLOCK" "l1.private_key:$DEPLOYER_KEY" "l1.batcher_private_key:$BATCHER_KEY" \
           "l2.chain_id:$L2_CHAIN_ID" "l2.jwt:$JWT" \
           "accel.jump_interval_s:$JUMP_S" "accel.activation_margin_s:$MARGIN_S" \
           "accel.target_l2_blocks:$TARGET_BLOCKS" "accel.catchup_timeout_s:$CATCHUP_TIMEOUT" \
           "batcher.max_channel_duration:$MAX_CHAN_DUR" "batcher.da_type:$DA_TYPE" \
           "paths.contracts_bedrock:$CB"; do
    [ -n "${v#*:}" ] || die "config: ${v%%:*} missing or empty"
  done
  case "$(toml_get meta create2_salt)" in
    0x[0-9a-fA-F]*) : ;;
    *) die "config: [meta].create2_salt must be 0x-prefixed hex" ;;
  esac
  case "$JWT" in
    *[!0-9a-fA-F]*|"") die "config: [l2].jwt must be 64 hex chars (got ${#JWT})" ;;
  esac
  [ "${#JWT}" -eq 64 ] || die "config: [l2].jwt must be 64 hex chars (got len=${#JWT})"
  case "$DA_TYPE" in calldata|blobs) : ;; *) die "config: [batcher].da_type must be calldata|blobs (got '$DA_TYPE')" ;; esac
  for v in "$L1_CHAIN_ID" "$L1_GENESIS_TS" "$L1_BLOCK_TIME" "$PIN_BLOCK" "$L2_CHAIN_ID" \
           "$JUMP_S" "$MARGIN_S" "$TARGET_BLOCKS" "$CATCHUP_TIMEOUT" "$MAX_CHAN_DUR"; do
    is_uint "$v" || die "config: numeric field has bad value '$v'"
  done
  [ "$L1_BLOCK_TIME" -ge 1 ] || die "config: l1.block_time must be >= 1"
  [ "$PIN_BLOCK" -ge 1 ] || die "config: l1.pin_block must be >= 1"
  # 端口：范围 + 互不冲突（toml 内部自撞旧版不查，运行期才以 geth 绑定失败暴露）
  local ports=("$P_ANVIL" "$P_GETH_HTTP" "$P_GETH_AUTH" "$P_GETH_WS" "$P_OPNODE" "$P_BATCHER") dups
  for p in "${ports[@]}"; do
    is_uint "$p" || die "config: port field has bad value '$p'"
    [ "$p" -ge 1 ] && [ "$p" -le 65535 ] || die "config: port $p out of range 1-65535"
  done
  dups="$(printf '%s\n' "${ports[@]}" | sort | uniq -d)"
  [ -z "$dups" ] || die "config: duplicate port(s) in devnet.toml:[ ${dups//$'\n'/ }] — components would fight over the socket"
  # fork 阶梯：本次生效集合内严格递增（op-deployer 只在 genesis 生成期才查，乱序会穿透）
  for f in $RUN_FORKS; do
    off="$(toml_get forks "$f")"
    is_uint "$off" || die "config: [forks].$f missing or not a non-negative integer ('$off')"
    [ "$off" -gt "$prev" ] || die "config: fork ladder not strictly increasing at $f (offset $off, prior $prev)"
    prev="$off"
  done
}
tail_log() { _LAST_LOG="$1"; printf -- '---- tail %s ----\n' "$1"; tail -n "${2:-25}" "$1" 2>/dev/null || true; }
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

# ---------- fork 边界穿越事件（P4 可观测性；计数逻辑复用 check_activations.py） ----------
L1INFO_FROM="0xdeaddeaddeaddeaddeaddeaddeaddeaddead0001"
L1INFO_TO="0x4200000000000000000000000000000000000015"
boundary_table() { # -> stdout TSV: fork \t 激活块号 \t 边界 ts
  # 激活块号公式与 check_activations.py 一致：(boundary - l2_time + 1) / block_time（整除）
  local l2 bt f t
  l2="$(jq -r '.genesis.l2_time' "$ART/rollup.json")"
  bt="$(jq -r '.block_time' "$ART/rollup.json")"
  for f in $RUN_FORKS; do
    t="$(jq -r ".${f}_time // empty" "$ART/rollup.json" 2>/dev/null || true)"
    [ -n "$t" ] || continue
    printf '%s\t%d\t%d\n' "$f" "$(( (t - l2 + 1) / bt ))" "$t"
  done
}
upgrade_tx_count() { # $1=block number -> stdout: 该块升级交易数（type 0x7E 且非 L1-attributes；
                     # 判定式与 check_activations.py 的 l1info/upgrade 二分一致）
  local hex; hex="$(printf '0x%x' "$1")"
  rpc "$L2_HTTP" eth_getBlockByNumber "[\"$hex\", true]" 2>/dev/null | \
    jq -r --arg from "$L1INFO_FROM" --arg to "$L1INFO_TO" '
      [ (.result.transactions // [])[]?
        | select(((.type // "0x0") | ascii_downcase) == "0x7e")
        | select((((.from // "") | ascii_downcase) == $from)
                 and (((.to // "") | ascii_downcase) == $to) | not) ]
      | length' 2>/dev/null || echo 0
}
expected_upgrade_of() { # $1=fork -> UPGRADE_COUNTS 表里的预期升级交易数
  awk -v k="$1" 'BEGIN{RS=" "; FS="="} $1==k {print $2}' <<<"$UPGRADE_COUNTS"
}
report_crossed() { # $1=head 块号 $2=boundary TSV —— 每跨过一个 fork 激活块主动打一条 CROSSED
  local head="$1" f a t cnt exp
  while IFS=$'\t' read -r f a t; do
    [ -n "$f" ] || continue
    case "$CROSSED" in *" $f "*) continue ;; esac
    [ "$a" -le "$head" ] || continue
    cnt="$(upgrade_tx_count "$a")"
    exp="$(expected_upgrade_of "$f")"
    if [ "$cnt" = "$exp" ]; then
      log "CROSSED $f @block $a (upgrade txs: $cnt ✓)"
    else
      log "CROSSED $f @block $a (upgrade txs: $cnt != expected $exp)"
    fi
    CROSSED="$CROSSED$f "   # 记录前后包空格，供 *" $f "* 去重匹配（无尾空格会漏判 → 同一边界双打点，实测踩坑）
  done <<<"$2"
}
fork_at_block() { # $1=block $2=boundary TSV -> 当前 fork 段名（最后一个激活块 <= 该块的 fork；否则 bedrock）
  local n="$1" seg="bedrock" f a t
  while IFS=$'\t' read -r f a t; do
    [ -n "$f" ] || continue
    [ "$a" -le "$n" ] && seg="$f"
  done <<<"$2"
  printf '%s' "$seg"
}
CROSSED=" "   # 首尾空格哨兵：去重匹配依赖 *" $f "*，无前导空格会漏判（实测踩坑）

# ---------- campaign：由基准 toml 派生临时 campaign toml ----------
# 只改四处：meta.name / l1.genesis_timestamp（锚 = 墙钟-500s）/ [forks]（压缩阶梯 + 远未来尾）/
# [accel].target_l2_blocks（信息用）。runtime_dir、端口、chain-id、角色、盐全部继承基准 ——
# status/down 与 txsource run.sh 用缺省 devnet.toml 即可操作 campaign 栈。
#
# --forks 前缀子集语义（P4 修复定案；旧版未入选 fork 从 [forks] 落空 → intent 缺字段落 0 →
# op-deployer 校验拒绝 `fork delta set to 0, but prior fork canyon has higher offset`）：
#   - 合法输入 = 规范序 CANON_FORKS 从 regolith 开始的连续前缀（大小写不敏感，逗号/空格分隔）。
#     理由：真实链不能跳过 fork，前缀子集 = 合法真实链形态；非前缀（跳变）有已登记的
#     L1-fee cross-check 限制，不支持 → 结构化报错（不留半状态，此校验在任何组件启动前）。
#   - 生成规则（[forks] 恒含全表 8 fork，物理上不能省略 —— deployer 缺字段落 0 被拒）：
#     前缀内 fork = 压缩阶梯（第 i 个表内入选者 = i × seg_s）；前缀外 fork = 远未来偏移
#     （base = max(0xffff, (入选数+1)×seg_s) 起逐个 +1，保持严格递增且 ≫ 观察窗口）——
#     不能写 0（递增校验拒绝）、不能省略（回落 deployer 默认「出生即 jovian」）。
#     ⇒ 链在观察窗口内是一条合法的前缀链，远未来 fork 的边界永不到来。
#   - Delta 特殊处理（本 worktree 非标准 fork，位于 Canyon↔Ecotone 之间，deployer 要求
#     严格介于两者之间）：前缀含 ecotone 时 delta 入选、占自己的阶梯档位（= canyon 档 +
#     seg_s，恒严格介于 canyon 与 ecotone 之间，任意 seg_s ≥ 1 都成立）；前缀只到 canyon
#     时 delta 与 ecotone 一同写远未来值。delta 不是 --forks 的合法名字（隐式跟随前缀）。
canon_pos() { # $1=fork -> 1-based position in CANON_FORKS（0 = 不在表内）
  local i=1 f
  for f in $CANON_FORKS; do [ "$f" = "$1" ] && { printf '%s' "$i"; return 0; }; i=$((i+1)); done
  printf '0'
}
canon_nth() { awk -v i="$1" '{print $i}' <<<"$CANON_FORKS"; }
parse_campaign_forks() { # 解析/校验 CAMPAIGN_FORKS -> CAMPAIGN_SEL（规范序前缀）+ CAMPAIGN_FAR
  local list f pos at=0 j skipped seen=" "
  list="$(printf '%s' "${CAMPAIGN_FORKS:-$CANON_FORKS}" | tr ',\t' '  ' | tr 'A-Z' 'a-z' | awk '{$1=$1}1')"
  [ -n "$list" ] || list="$CANON_FORKS"          # --forks "" 等价缺省
  CAMPAIGN_SEL=""; CAMPAIGN_FAR=""
  # 1) 名字合法 + 不重复（fail-fast，先于任何组件启动 —— 无半状态）
  for f in $list; do
    case " $CANON_FORKS " in *" $f "*) ;;
      *) if [ "$f" = "delta" ]; then
           die "--forks: 'delta' is not selectable —— 本 worktree 的非标准 fork 隐式跟随前缀：前缀含 ecotone 则 delta 落 canyon 与 ecotone 之间，否则远未来 (canonical order: $CANON_FORKS)"
         fi
         die "--forks: unknown fork '$f' (canonical order: $CANON_FORKS)" ;;
    esac
    case "$seen" in *" $f "*) die "--forks: duplicate fork '$f'" ;; esac
    seen="$seen$f "
  done
  # 2) 连续前缀：从 regolith 开始逐项命中规范序；断链/乱序即结构化报错（列合法示例）
  for f in $list; do
    pos="$(canon_pos "$f")"
    if [ "$pos" -eq "$((at+1))" ]; then
      CAMPAIGN_SEL="$CAMPAIGN_SEL$f "; at="$pos"
    elif [ "$pos" -le "$at" ]; then
      die "--forks: out-of-order entry '$f'（规范序中它已在位置 $pos 出现过，必须紧跟 '$(canon_nth "$at")' 之后; canonical order: $CANON_FORKS)"
    else
      skipped=""
      for (( j=at+1; j<pos; j++ )); do skipped+="$(canon_nth "$j") "; done
      skipped="${skipped% }"
      die "--forks must be a contiguous prefix of the canonical fork order starting at regolith
  got:            [$list]
  gap:            '$f' selected but skipped fork(s) before it: ${skipped:-<none>}
  legal prefixes: \"regolith\" / \"regolith,canyon\" / \"regolith,canyon,ecotone,fjord,granite,holocene\" / \"regolith,canyon,ecotone,fjord,granite,holocene,isthmus,jovian\"（= 缺省全表）"
    fi
  done
  # 3) 前缀之外的表内 fork（远未来）：delta 入选当且仅当前缀到达 ecotone
  local sel=" $CAMPAIGN_SEL"
  for f in $FORKS; do
    case "$sel" in *" $f "*) continue ;; esac                 # 入选 → 阶梯档位
    if [ "$f" = "delta" ] && case "$sel" in *" ecotone "*) true ;; *) false ;; esac; then
      continue                                                # delta 隐式入选（前缀含 ecotone）→ 阶梯档位
    fi
    CAMPAIGN_FAR="$CAMPAIGN_FAR$f "
  done
  CAMPAIGN_FAR="${CAMPAIGN_FAR% }"
}
generate_campaign_toml() { # $1=base toml $2=out toml
  local base="$1" out="$2"
  [ -f "$base" ] || die "base config not found: $base"
  parse_campaign_forks
  local anchor_ts=$(( $(date +%s) - CAMPAIGN_ANCHOR_OFFSET_S ))
  python3 - "$base" "$out" "$CAMPAIGN_SEL" "$CAMPAIGN_FAR" "$CAMPAIGN_SEG_S" "$anchor_ts" <<'PYEOF' || die "campaign toml generation failed (见上方 python 报错)"
import re, sys
base_p, out_p, sel, far, seg_s, anchor_ts = (
    sys.argv[1], sys.argv[2], sys.argv[3].split(), sys.argv[4].split(), int(sys.argv[5]), int(sys.argv[6]))
text = open(base_p).read()
# 0) 文件头打 campaign 标记（注释独占一行 —— 本 toml 的 awk 解析器约定）
text = "# GENERATED by opdevnet.sh up --campaign（勿手改；基准 devnet.toml 不受影响）\n" + text
# 1) meta.name
text = re.sub(r'(?m)^name\s*=\s*".*"$', 'name       = "d3-devnet-campaign"', text, count=1)
# 2) l1.genesis_timestamp -> 墙钟锚（campaign 特意不固定绝对值：每次 up 锚定当前墙钟）
text = re.sub(r'(?m)^genesis_timestamp(\s*)=\s*\d+(\s*#.*)?$',
              lambda m: f'genesis_timestamp{m.group(1)} = {anchor_ts}', text, count=1)
# 3) [forks] 段整体替换：恒含全表 8 fork（deployer 对 intent 缺字段按 0 处理 → 被严格递增
#    校验拒绝，物理上不能省略）。前缀内 = 压缩阶梯（第 i 个表内入选者 = i*seg_s；delta 入选
#    时占自己的档位，恒严格介于 canyon 与 ecotone 之间）；前缀外 = 远未来偏移（base 起 +1
#    保持严格递增，≫ 观察窗口 → 边界永不到来）。
order = ["canyon", "delta", "ecotone", "fjord", "granite", "holocene", "isthmus", "jovian"]
active = [f for f in order if f not in far]
far_base = max(0xFFFF, (len(active) + 1) * seg_s)
offsets = {f: (i + 1) * seg_s for i, f in enumerate(active)}
for j, f in enumerate(far):
    offsets[f] = far_base + j
assert list(offsets) == order and len(set(offsets.values())) == len(order), "fork schedule not a strict ladder"
sec = ["[forks]",
       "# 生效调度：前缀内 fork = 第 i 个表内入选者 x seg_s（delta 入选时占自己的档位，严格介于",
       "# canyon 与 ecotone 之间）；前缀外 fork = 远未来偏移（观察窗口内不激活）。全表 8 fork 必须",
       "# 显式给值：op-deployer 对 intent 缺字段按 0 处理，会被严格递增校验拒绝。",
       "# regolith 不在表内 = 恒在 genesis（@0）"]
sec += [f"{f:<8} = {offsets[f]}" for f in order]
text = re.sub(r'(?ms)^\[forks\]\n.*?(?=\n\[)', "\n".join(sec) + "\n", text)
# 4) [accel].target_l2_blocks -> 末个入选 fork 激活块 + 余量（campaign catch-up 不用它，仅展示）
last_boundary = len(active) * seg_s
last_act = last_boundary // 2 + (last_boundary % 2)
text = re.sub(r'(?m)^target_l2_blocks(\s*)=\s*\d+(\s*#.*)?$',
              lambda m: f'target_l2_blocks{m.group(1)} = {last_act + 75}', text, count=1)
open(out_p, "w").write(text)
print(f"forks: prefix=[{' '.join(sel)}] active=[{' '.join(active)}] far_future=[{' '.join(far) or '-'}] "
      f"(far offset base={far_base}s ≫ window {last_boundary}s) seg={seg_s}s "
      f"anchor_genesis_ts={anchor_ts} last_boundary={last_boundary}s")
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
  # campaign 数值参数 fail-fast（审计 4.5：非数字旧版落到 python traceback）
  if [ "$CAMPAIGN" -eq 1 ]; then
    is_uint "$CAMPAIGN_SEG_S" || die "--segment-seconds must be a positive integer (got '$CAMPAIGN_SEG_S')"
    [ "$CAMPAIGN_SEG_S" -ge 1 ] || die "--segment-seconds must be >= 1"
  fi
  if [ "$CAMPAIGN" -eq 1 ]; then
    parse_campaign_forks   # 父 shell 先解析（管道里的 generate 在子 shell 跑，变量传不出来）
    generate_campaign_toml "$TOML" "$CAMPAIGN_TOML" | sed 's/^/[campaign] /'
    TOML="$CAMPAIGN_TOML"
    # RUN_FORKS 恒为全表 8 fork：intent 必须显式覆盖全部（deployer 对缺字段按 0 处理 → 被严格
    # 递增校验拒绝）。前缀外 fork = 远未来偏移（CAMPAIGN_FAR），运行窗口内不激活。
    log "CAMPAIGN mode: seg=${CAMPAIGN_SEG_S}s anchor=now-${CAMPAIGN_ANCHOR_OFFSET_S}s prefix=[${CAMPAIGN_SEL% }] far_future=[${CAMPAIGN_FAR:-none}] toml=${TOML}（链时间贴墙钟，边界按墙钟逐段到来）"
  fi
  load_config
  log "devnet up: runtime=$RUNTIME  l2_chain_id=$L2_CHAIN_ID  l1_chain_id=$L1_CHAIN_ID"

  # ---- preflight（fail-fast：二进制/内容 pin/工具/配置结构/磁盘/端口/互斥） ----
  STEP="preflight"
  trap up_exit_trap EXIT
  mkdir -p "$RUN" 2>/dev/null || true
  acquire_up_lock
  check_binaries
  validate_config
  check_disk
  EXPECTED_L2_TIME=$((L1_GENESIS_TS + L1_BLOCK_TIME * PIN_BLOCK))
  log "anchor: anvil genesis ts=$L1_GENESIS_TS (fixed), pin L1 head at block $PIN_BLOCK -> expected rollup l2_time=$EXPECTED_L2_TIME"
  for f in batcher opnode geth anvil miner apply; do
    if pid_alive "$RUN/$f.pid"; then die "already running (pid $(awk '{print $1}' "$RUN/$f.pid") in $RUN/$f.pid); run ./opdevnet.sh down first"; fi
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
  stage 1 "anvil (L1)"
  STEP="anvil"
  start_bg anvil "$LOGS/anvil.log" "$ANVIL" --chain-id "$L1_CHAIN_ID" --no-mining \
    --timestamp "$L1_GENESIS_TS" --port "$P_ANVIL"
  mark_started anvil
  wait_listen "$P_ANVIL" 15 || { tail_log "$LOGS/anvil.log"; die "anvil did not listen on $P_ANVIL"; }
  wait_rpc "$L1_RPC" 15 || { tail_log "$LOGS/anvil.log"; die "anvil RPC not ready"; }
  local ts0; ts0="$(block_ts 0)"
  [ "$ts0" = "$L1_GENESIS_TS" ] || die "anvil genesis ts=$ts0 != configured $L1_GENESIS_TS"

  # ---- 2) 确定性锚点：把 L1 头逐块精确矿到 pin_block 并停住 ----
  stage 2 "deployer (pin L1 head -> init/apply/inspect -> normalize/verify)"
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
  echo "$apply_pid op-deployer" >"$RUN/apply.pid"   # 旧版无 pidfile：Ctrl-C 中断 up 后 apply 成孤儿且 preflight 不可见（审计 1.4/3）
  mark_started apply
  sleep 2   # deployer 在入口读 L1 头作为「L1 起始块」——此刻头仍停在 pin_block
  # apply 代矿循环（ts 仍精确 parent+2）。监督 anvil 存活（审计 3.4）：anvil 死亡时代矿若
  # 静默空转，apply 会无限挂死等块 —— 最坏失败模式；此处杀 apply 让失败立刻传播。
  ( while kill -0 "$apply_pid" 2>/dev/null; do
      if ! pid_alive "$RUN/anvil.pid"; then kill "$apply_pid" 2>/dev/null; exit 1; fi
      mine_next $(( $(head_ts) + L1_BLOCK_TIME )); sleep 1
    done ) &
  local miner_pid=$!
  echo "$miner_pid bash" >"$RUN/miner.pid"
  mark_started miner
  local rc=0
  wait "$apply_pid" || rc=$?
  kill "$miner_pid" 2>/dev/null || true; wait "$miner_pid" 2>/dev/null || true
  rm -f "$RUN/miner.pid" "$RUN/apply.pid"
  [ "$rc" -eq 0 ] || { tail_log "$LOGS/deployer-apply.log" 40; die "op-deployer apply failed (rc=$rc)"; }
  log "apply done: $(grep -c '' "$LOGS/deployer-apply.log") log lines; L1 head now block $(head_number)"

  # ---- 4) inspect 取产物 ----
  # 守卫必须显式（审计 3.2：坏 fork 阶梯实测 inspect genesis 失败时脚本 set -e 静默 exit 1，
  # 报错只落在 .err 侧文件里，用户零输出）
  STEP="inspect"
  "$OPDEPLOYER" inspect genesis --workdir "$WD" "$L2_CHAIN_ID" 2>"$LOGS/inspect-genesis.err" >"$ART/genesis.json" \
    || { tail_log "$LOGS/inspect-genesis.err" 25; die "op-deployer inspect genesis failed (fork ladder ordering / intent 校验? see $LOGS/inspect-genesis.err)"; }
  "$OPDEPLOYER" inspect rollup  --workdir "$WD" "$L2_CHAIN_ID" 2>"$LOGS/inspect-rollup.err"  >"$ART/rollup.json" \
    || { tail_log "$LOGS/inspect-rollup.err" 25; die "op-deployer inspect rollup failed (see $LOGS/inspect-rollup.err)"; }
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
  stage 3 "geth init"
  printf '%s\n' "$JWT" >"$RUN/jwt.txt"; chmod 600 "$RUN/jwt.txt"
  "$GETH" --datadir "$L2DIR" --state.scheme hash init "$ART/genesis.json" >"$LOGS/geth-init.log" 2>&1 \
    || { tail_log "$LOGS/geth-init.log"; die "geth init failed"; }
  stage 4 "geth start"
  start_bg geth "$LOGS/geth.log" "$GETH" --datadir "$L2DIR" \
    --http --http.port "$P_GETH_HTTP" --http.api eth,debug,net,web3 \
    --authrpc.port "$P_GETH_AUTH" --authrpc.jwtsecret "$RUN/jwt.txt" \
    --ws --ws.port "$P_GETH_WS" \
    --state.scheme hash --gcmode archive --syncmode full --nodiscover --port 0 \
    --rollup.disabletxpoolgossip
  mark_started geth
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
  stage 5 "op-node (sequencer)"
  STEP="op-node"
  start_bg opnode "$LOGS/opnode.log" "$OPNODE" \
    --l2 "http://127.0.0.1:$P_GETH_AUTH" --l2.jwt-secret "$RUN/jwt.txt" \
    --l1 "$L1_RPC" --l1.beacon.ignore \
    --rollup.l1-chain-config "$ART/l1-chain-config.json" \
    --sequencer.enabled --sequencer.l1-confs 0 \
    --rollup.config "$ART/rollup.json" --rpc.port "$P_OPNODE"
  mark_started opnode
  wait_listen "$P_OPNODE" 30 || { tail_log "$LOGS/opnode.log" 40; die "op-node rpc not listening on $P_OPNODE"; }
  # op-node RPC 没有 eth_chainId，用 optimism_syncStatus 探活（run6 教训：探针方法要对组件语义）
  wait_rpc "$OPNODE_RPC" 30 optimism_syncStatus || { tail_log "$LOGS/opnode.log" 40; die "op-node rpc not ready"; }

  # ---- 9) op-batcher ----
  stage 6 "op-batcher"
  STEP="op-batcher"
  start_bg batcher "$LOGS/batcher.log" "$OPBATCHER" \
    --l1-eth-rpc "$L1_RPC" --l2-eth-rpc "$L2_HTTP" --rollup-rpc "$OPNODE_RPC" \
    --private-key "$BATCHER_KEY" \
    --max-channel-duration "$MAX_CHAN_DUR" --data-availability-type "$DA_TYPE" \
    --throttle.unsafe-da-bytes-lower-threshold 0 --rpc.port "$P_BATCHER"
  mark_started batcher
  wait_listen "$P_BATCHER" 20 || { tail_log "$LOGS/batcher.log" 40; die "batcher rpc not listening on $P_BATCHER"; }
  # batcher 默认 --rpc.port 8545 与 anvil 撞（Task 1 实测），配置固定 8548

  write_env "$ghash"

  # ---- 10) 等待 L2 追到目标；campaign 以「头块时间追平墙钟」为准，默认按块数+激活块检查 ----
  STEP="l2-catchup"
  local deadline=$(( $(date +%s) + CATCHUP_TIMEOUT )) last_reported=0 blk
  # 标记「栈已完整、catch-up 完成」：此前的失败自动清理已启动组件；此后的失败
  # （激活块计数不符）保留运行现场供取证（见 up_exit_trap）。
  UP_STACK_OK=0
  if [ "$CAMPAIGN" -eq 1 ]; then
    # campaign：sequencer 以机器速度从 genesis 追墙钟，追平后转入实时出块。
    # 头块 ts 进入墙钟 ±DRIFT 即认为 catch-up 完成（不设块数目标 —— 边界留给墙钟逐段穿越）。
    log "campaign: waiting L2 head time within ${CAMPAIGN_CATCHUP_DRIFT_S}s of wall clock ..."
    local hts wall last_prog=0 now
    local camp_boundaries; camp_boundaries="$(boundary_table)"
    while :; do
      for f in anvil geth opnode batcher; do
        pid_alive "$RUN/$f.pid" || { tail_log "$LOGS/$f.log" 40; die "$f died during catch-up"; }
      done
      blk=$(( $(rpc_res "$L2_HTTP" eth_blockNumber 2>/dev/null || echo 0) ))
      report_crossed "$blk" "$camp_boundaries"
      # 每 10 秒一条追赶进度行（与按块数 catch-up 同格式）
      now="$(date +%s)"
      if [ $(( now - last_prog )) -ge 10 ]; then
        log "CATCH-UP block=$blk fork=$(fork_at_block "$blk" "$camp_boundaries")"
        last_prog="$now"
      fi
      hts=$(( $(rpc_res "$L2_HTTP" eth_getBlockByNumber "$(printf '["0x%x", false]' "$blk")" 2>/dev/null | jq -r '.timestamp // 0') ))
      wall="$(date +%s)"
      if [ "$blk" -gt 0 ] && [ $(( wall - hts )) -le "$CAMPAIGN_CATCHUP_DRIFT_S" ]; then
        log "campaign catch-up done: head=$blk head_ts=$hts wall=$wall (behind $((wall-hts))s)"
        UP_STACK_OK=1
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
      [ -n "$t" ] || continue
      case " $CAMPAIGN_FAR " in *" $f "*)
        log "    $f @ ts $t (block ~$((( t - $(jq -r '.genesis.l2_time' "$ART/rollup.json") ) / 2))) —— 远未来：观察窗口内不激活（前缀外 fork）"
        ;;
      *)
        log "    $f @ ts $t (block ~$((( t - $(jq -r '.genesis.l2_time' "$ART/rollup.json") ) / 2)))"
        ;;
      esac
    done
    log "  next: ./opdevnet.sh status | 激活计数：check_activations.py --forks <前缀内表内 fork 逗号表> | txsource: cd ../txsource && ./run.sh --plugin ..."
    return 0
  fi
  log "waiting L2 catch-up to block $TARGET_BLOCKS (machine speed ~115 blk/s after boundaries crossed) ..."
  local boundaries; boundaries="$(boundary_table)"
  local last_prog=0 now
  while :; do
    for f in anvil geth opnode batcher; do
      pid_alive "$RUN/$f.pid" || { tail_log "$LOGS/$f.log" 40; die "$f died during catch-up"; }
    done
    blk=$(( $(rpc_res "$L2_HTTP" eth_blockNumber 2>/dev/null || echo 0) ))
    if [ $(( blk - last_reported )) -ge 500 ] || [ "$blk" -ge "$TARGET_BLOCKS" ]; then
      log "L2 head: $blk / $TARGET_BLOCKS"; last_reported="$blk"
    fi
    # 边界穿越主动打点（升级交易计数复用 check_activations.py 逻辑）
    report_crossed "$blk" "$boundaries"
    # 每 10 秒一条追赶进度行
    now="$(date +%s)"
    if [ $(( now - last_prog )) -ge 10 ]; then
      log "CATCH-UP block=$blk fork=$(fork_at_block "$blk" "$boundaries")"
      last_prog="$now"
    fi
    [ "$blk" -ge "$TARGET_BLOCKS" ] && { UP_STACK_OK=1; break; }
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
      pid="$(awk '{print $1}' "$pidfile")"
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
    local seg next_name next_t bt
    seg="Bedrock"; next_name="-"; next_t=""
    bt="$(jq -r '.block_time // 2' "$ART/rollup.json" 2>/dev/null || echo 2)"
    local f t prev_t=0
    for f in $RUN_FORKS; do
      t="$(jq -r ".${f}_time // empty" "$ART/rollup.json" 2>/dev/null || true)"
      [ -z "$t" ] && continue
      if [ "$l2ts" -ge "$t" ]; then seg="$(printf '%s' "$f" | awk '{print toupper(substr($0,1,1)) substr($0,2)}')"; prev_t="$t"; else next_name="$f"; next_t="$t"; break; fi
    done
    echo "  L2 unsafe : block $l2n ts=$l2ts fork=$seg"
    if [ -n "$next_t" ]; then
      echo "  next fork : $next_name at ts=$next_t (block ~$(( (next_t - L2_TIME) / bt )), in $(( next_t - l2ts ))s of chain time ≈ $(( (next_t - l2ts) / bt )) blocks)"
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
  # 反序 kill：batcher -> op-node -> geth -> 代矿循环 -> apply -> anvil
  # （第二参数 = 期望进程名，kill 前校验 PID 身份，防 PID 复用误杀 —— 审计 1.4）
  kill_pidfile "$RUN/batcher.pid" op-batcher
  kill_pidfile "$RUN/opnode.pid"  op-node
  kill_pidfile "$RUN/geth.pid"    geth
  kill_pidfile "$RUN/miner.pid"   bash
  kill_pidfile "$RUN/apply.pid"   op-deployer
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

# ---------- 薄分发层：收编 chainexport / check_* / txsource（P4） ----------
# 原则：只归一入口（一处记住全部工具入口），不复制被调工具的任何逻辑/默认值；
# 参数原样透传、退出码经 exec 直通。各子命令参数的唯一权威 = 被调工具自己的 -h。

CHAINEXPORT_BIN="${OPDEVNET_CHAINEXPORT:-$DIR/chainexport/chainexport.bin}"

cmd_export() { # opdevnet.sh export [chainexport 参数…]
  if [ ! -x "$CHAINEXPORT_BIN" ]; then
    STEP="export"
    die "chainexport binary missing: $CHAINEXPORT_BIN —— rebuild it first: bash $DIR/chainexport/build.sh"
  fi
  exec "$CHAINEXPORT_BIN" "$@"
}

check_usage() { # opdevnet.sh check [-h] —— 两个检查目标的一行说明
  cat <<'EOF'
usage: opdevnet.sh check activations|distribution [参数…]

  activations   激活块升级交易计数检查（6/3/8/5 等；--min-user-txs 阈值可调，
                参数明细: opdevnet.sh check activations -h）
  distribution  用户（非 L1-attributes）交易跨 fork 段分布检查
                （参数明细: opdevnet.sh check distribution -h）
EOF
  exit 2
}

cmd_check() { # opdevnet.sh check activations|distribution [参数…]
  local sub="${1:-}"
  case "$sub" in
    -h|--help|help|"") check_usage ;;
    activations)  shift; exec "$DIR/check_activations.py" "$@" ;;
    distribution) shift; exec "$DIR/txsource/check_distribution.py" "$@" ;;
    *) echo "unknown check target: $sub (known: activations, distribution)" >&2; check_usage ;;
  esac
}

cmd_txsource() { # opdevnet.sh txsource [run.sh 参数…]
  exec bash "$DIR/txsource/run.sh" "$@"
}

# ---------- snapshot：chainexport 导出 + vectors/ 自动注册（P5） ----------
# 把「导出 → stem 命名（既有 digest 规则）→ 写入 vectors/ → 替换旧 devnet 快照 →
# manifest/SHA256SUMS upsert → git add 例外文件」收口为一条命令（Task 6 手工步骤固化）。
# 注册面硬约束（都是既有事实的机制化，不是新政策）：
#   - 体积上限 SNAPSHOT_MAX_BYTES（95MiB，= chainexport --poststate full 自动降级阈值）：
#     GitHub 拒收 >=100MiB 的 push blob，超限文件拒绝注册——推不上去的入库物没有意义；
#   - vectors/ 只注册最新一枚 devnet 快照：DIVERGENCES.md 的 ALLOWLIST 行绑定单一 stem，
#     旧 stem 快照若留在注册面而登记行已重钉到新 stem = stale exemption，直接把 FISCO
#     重放门打红；被替换的旧文件由 git rm 删除（315MB full 模式旧快照即在此被替换）；
#   - 滚动保留最近 SNAPSHOT_KEEP(2) 个导出文件在 --out-dir 暂存区（vectors/ 注册面之外）。
CORPUS_ROOT="$(cd "$DIR/../.." && pwd)"
VECTORS_DIR="$CORPUS_ROOT/opstack-executor/tests/t8n/vectors"
SNAPSHOT_MAX_BYTES=$((95 * 1024 * 1024))
SNAPSHOT_KEEP=2

snapshot_file_size() { stat -f%z "$1" 2>/dev/null || stat -c%s "$1"; }

# run_export [chainexport 参数…] —— 跑一次导出并把 stdout 机读行解到 X_* 变量
#   X_STEM/X_FILE/X_SHA/X_SIZE/X_DOWN；rc≠0 或机读行缺失 = die。不做体积判断。
run_export() {
  local tmpout rc=0
  tmpout="$(mktemp /tmp/opdevnet-snapshot.XXXXXX)"
  # --force：out-dir 是 snapshot 自己的暂存区（重跑同 stem 覆盖重导），非用户数据
  "$CHAINEXPORT_BIN" --out-dir "$out_dir" --force "$@" >"$tmpout" || rc=$?
  if [ "$rc" -ne 0 ]; then
    cat "$tmpout" >&2; rm -f "$tmpout"
    die "chainexport failed (rc=$rc)"
  fi
  X_STEM="$(sed -n 's/^DEVNET-STEM //p' "$tmpout" | tail -1)"
  X_FILE="$(sed -n 's/^WROTE //p' "$tmpout" | tail -1)"
  X_SHA="$(sed -n 's/^SHA256 //p' "$tmpout" | tail -1)"
  if grep -q '^DOWNGRADED full->boundary$' "$tmpout"; then X_DOWN=1; else X_DOWN=""; fi
  rm -f "$tmpout"
  [ -n "$X_STEM" ] && [ -n "$X_FILE" ] && [ -n "$X_SHA" ] || \
    die "chainexport stdout missing DEVNET-STEM/WROTE/SHA256 machine lines (rc=0 but unparseable)"
  X_SIZE="$(snapshot_file_size "$X_FILE")" || die "cannot stat $X_FILE"
}

cmd_snapshot() { # opdevnet.sh snapshot [--out-dir D] [chainexport 参数…]
  STEP="snapshot"
  local out_dir="" args=()
  while [ $# -gt 0 ]; do
    case "$1" in
      --out-dir)   out_dir="${2:-}"; shift 2 ;;
      --out-dir=*) out_dir="${1#--out-dir=}"; shift ;;
      --from-block|--to-block) die "snapshot 自动管理导出范围（自动合规），请勿手工传 $1；范围导出请用 opdevnet.sh export" ;;
      *) args+=("$1"); shift ;;
    esac
  done
  if [ ! -x "$CHAINEXPORT_BIN" ]; then
    die "chainexport binary missing: $CHAINEXPORT_BIN —— rebuild it first: bash $DIR/chainexport/build.sh"
  fi
  [ -f "$VECTORS_DIR/manifest.txt" ] && [ -f "$VECTORS_DIR/SHA256SUMS" ] || \
    die "vectors registry missing under $VECTORS_DIR (run inside the corpus checkout)"
  if [ -z "$out_dir" ]; then
    out_dir="/tmp/opdevnet-snapshots"
  fi
  mkdir -p "$out_dir"

  # 1) 全链导出（--poststate 缺省 boundary；显式传 full 由 chainexport 体积守卫自动降级）
  log "chainexport: exporting full chain (out-dir=$out_dir${args[*]:+ args=${args[*]}})…"
  run_export ${args[@]+"${args[@]}"}
  local stem=$X_STEM file=$X_FILE sha=$X_SHA size=$X_SIZE
  [ -n "$X_DOWN" ] && log "SIZE GUARD: full 超限，已自动降级为 boundary 采样（chainexport 已重序列化）"

  # 1b) 自动合规第二级：boundary 全链仍超限（状态 dump ≈9.4MB/个 × 采样数——8 fork 链
  #     的激活±1 三元组就 24 个 dump ≈226MB，数学上不可能全链入限）→ 自动二分「最大
  #     可注册尾窗 [W, to]」：stem 按既有 ranged 规则带范围（devnet_<from>-<to>_<digest8>），
  #     采样规则一字不改；尾窗优先保住晚期段的 bcos-testing create 交易（登记命中）。
  #     每轮 campaign 自动滑到最新窗口 = 快照持续更新且永远可注册可推送。
  if [ "$size" -gt "$SNAPSHOT_MAX_BYTES" ]; then
    log "SIZE GUARD: boundary 全链导出 $size bytes > $SNAPSHOT_MAX_BYTES —— auto-fit：二分最大可注册尾窗"
    local initial_file="$file"
    local rpc="http://127.0.0.1:9545" i prev=""
    # 从透传参数里取 --rpc（形如 --rpc URL / --rpc=URL），供 eth_blockNumber 用
    for i in ${args[@]+"${args[@]}"}; do
      case "$i" in
        --rpc) prev="--rpc" ;;
        --rpc=*) rpc="${i#--rpc=}" ;;
        *) if [ "$prev" = "--rpc" ]; then rpc="$i"; prev=""; fi ;;
      esac
    done
    local head_hex to
    head_hex="$(curl -fsS -X POST -H 'Content-Type: application/json' \
      --data '{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}' "$rpc" \
      | sed -n 's/.*"result":[[:space:]]*"0x\([0-9a-fA-F]*\)".*/\1/p')" \
      || die "eth_blockNumber at $rpc failed（auto-fit 需要链头）"
    [ -n "$head_hex" ] || die "cannot parse eth_blockNumber reply from $rpc"
    to="$(printf '%d' "0x$head_hex")"
    log "auto-fit: chain head = ${to}（窗口右端冻结于此）"

    local span=199 w fit_span=0 fail_span=0
    fit_stem=""; fit_file=""; fit_sha=""; fit_size=""
    while [ $((to - span)) -ge 2 ]; do
      w=$((to - span))
      log "auto-fit: probing window [$w, $to] …"
      run_export --from-block "$w" --to-block "$to" ${args[@]+"${args[@]}"}
      if [ "$X_SIZE" -le "$SNAPSHOT_MAX_BYTES" ]; then
        # 放行：更宽的窗被尝试前，窄窗文件先让位
        [ -n "$fit_file" ] && rm -f "$fit_file"
        [ -n "$initial_file" ] && rm -f "$initial_file" && initial_file=""
        fit_span=$span; fit_stem=$X_STEM; fit_file=$X_FILE; fit_sha=$X_SHA; fit_size=$X_SIZE
        log "auto-fit: window fits ($X_SIZE bytes)"
        span=$((span * 2 + 1))
      else
        fail_span=$span
        rm -f "$X_FILE"
        log "auto-fit: window over ceiling ($X_SIZE bytes) — narrowing"
        break
      fi
    done
    if [ "$fit_span" -eq 0 ]; then
      die "auto-fit: 连最小窗（[2, $to]）也找不到可用拟合起点——链状态异常，拒绝注册"
    fi
    if [ "$fail_span" -gt 0 ]; then
      # 在 (fit_span 放行, fail_span 超限) 间二分，窗宽分辨率 100 块（采样点粒度）
      while [ $((fail_span - fit_span)) -gt 100 ]; do
        span=$(( (fit_span + fail_span) / 2 ))
        w=$((to - span))
        log "auto-fit: bisect window [$w, $to] …"
        run_export --from-block "$w" --to-block "$to" ${args[@]+"${args[@]}"}
        if [ "$X_SIZE" -le "$SNAPSHOT_MAX_BYTES" ]; then
          [ -n "$fit_file" ] && rm -f "$fit_file"
          [ -n "$initial_file" ] && rm -f "$initial_file" && initial_file=""
          fit_span=$span; fit_stem=$X_STEM; fit_file=$X_FILE; fit_sha=$X_SHA; fit_size=$X_SIZE
        else
          fail_span=$span; rm -f "$X_FILE"
        fi
      done
    fi
    stem=$fit_stem file=$fit_file sha=$fit_sha size=$fit_size
    log "auto-fit: chosen window [$((to - fit_span)), $to] → $stem.json ($size bytes)"
  fi

  local stem_base
  stem_base="$(printf '%s' "$stem" | sed -E 's/^devnet_(.+)_[0-9a-f]{8}$/\1/')"
  [ "$size" -le "$SNAPSHOT_MAX_BYTES" ] || \
    die "export $stem.json = $size bytes > $SNAPSHOT_MAX_BYTES register ceiling —— 拒绝注册（推不上 GitHub 的文件不入库）"

  # 2) 注册：写入 vectors/ + manifest/SHA256SUMS 整块 upsert
  local vec="$VECTORS_DIR/$stem.json"
  local manifest="$VECTORS_DIR/manifest.txt" sums="$VECTORS_DIR/SHA256SUMS"
  cp -f "$file" "$vec"
  # manifest：旧 devnet 注册块（注释缓冲 + devnet_*.json 文件名行）整块删除，其余原样
  awk '
    /^[ \t]*#/           { buf = buf $0 "\n"; next }
    /^devnet_.*\.json$/  { buf = ""; next }
    { if (buf != "") { printf "%s", buf; buf = "" } print }
    END { if (buf != "") printf "%s", buf }
  ' "$manifest" > "$manifest.tmp"
  cat >> "$manifest.tmp" <<EOF

# Real-derivation devnet snapshot (P3-2; auto-registered by tools/devnet/opdevnet.sh
# snapshot): $stem_base-block chainexport export of a real campaign devnet chain (stem rule
# devnet_<blocks>_<digest8>, digest8 = sha256 over the actual-header activation spec --
# single source of truth in chainexport/main.go, same convention as the ladder stem).
# Not a regen.sh product: regen.sh registers it read-only from the vectors/ glob and
# never regenerates it. Its bytes are pinned in vectors/SHA256SUMS; the create-output
# divergences registered in DIVERGENCES.md (P3-1b) bind to this stem.
$stem.json
EOF
  mv "$manifest.tmp" "$manifest"
  grep -v '  devnet_.*\.json$' "$sums" > "$sums.tmp"
  printf '%s  %s.json\n' "$sha" "$stem" >> "$sums.tmp"
  mv "$sums.tmp" "$sums"

  # 3) 替换旧 devnet 快照：vectors/ 只留新注册的这枚
  local old
  for old in "$VECTORS_DIR"/devnet_*.json; do
    [ -e "$old" ] || continue
    [ "$old" = "$vec" ] && continue
    ( cd "$CORPUS_ROOT" && git rm -q -f -- "opstack-executor/tests/t8n/vectors/$(basename "$old")" ) || rm -f "$old"
    log "replaced old devnet snapshot: $(basename "$old")（vectors/ 只注册最新一枚，ALLOWLIST 绑定单一 stem）"
  done

  # 4) 滚动保留：--out-dir 暂存区只留最近 SNAPSHOT_KEEP 个导出
  local keep=$SNAPSHOT_KEEP f
  while IFS= read -r f; do
    [ -z "$f" ] && continue
    if [ "$keep" -gt 0 ]; then keep=$((keep - 1)); continue; fi
    rm -f "$f"
    log "rolling retention: pruned staging export $(basename "$f")（out-dir 保留最近 $SNAPSHOT_KEEP 个）"
  done < <(ls -t "$out_dir"/devnet_*.json 2>/dev/null)

  # 5) git add 例外文件（.gitignore ! 规则已放行 devnet_*.json / SHA256SUMS）
  ( cd "$CORPUS_ROOT" && git add -f \
      "opstack-executor/tests/t8n/vectors/$stem.json" \
      "opstack-executor/tests/t8n/vectors/manifest.txt" \
      "opstack-executor/tests/t8n/vectors/SHA256SUMS" ) || die "git add failed"
  log "REGISTERED $stem.json ($size bytes, sha256 $sha) —— manifest/SHA256SUMS 已 upsert、暂存待提交"
  log "next: FISCO 重放将报未登记 DIVERGE → 用其 want/got 重钉 DIVERGENCES.md ALLOWLIST 行到 $stem 后重跑"
}

case "${1:-}" in
  up)       shift; cmd_up "$@" ;;
  status)   shift; cmd_status "$@" ;;
  down)     shift; cmd_down "$@" ;;
  clean)    shift; cmd_clean "$@" ;;
  export)   shift; cmd_export "$@" ;;
  check)    shift; cmd_check "$@" ;;
  txsource) shift; cmd_txsource "$@" ;;
  snapshot) shift; cmd_snapshot "$@" ;;
  -h|--help|help|"") usage ;;
  *) echo "unknown command: $1" >&2; usage ;;
esac
