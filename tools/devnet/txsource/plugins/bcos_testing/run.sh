#!/usr/bin/env bash
# bcos_testing/run.sh —— bcos-testing（Hardhat 套件）适配为 txsource 插件（P2 Task 4-B）
#
# 插件契约（见 ../../README.md）：
#   <plugin> --endpoint <L2 RPC> --chain-id <id> --funding-key <hex私钥> --segment <fork名> --round <n>
#   stdout 仅一行 JSON 报告；日志一律 stderr；nonce/签名插件自担；不做 settle、不做资金。
#
# 适配原则：**不改动 bcos-testing 上游文件**。
#   - 套件 clone 到 SUITE_DIR（缺省 /tmp/bcos-testing，可用环境变量 BCOS_TESTING_DIR 覆盖），
#     上游文件零修改；适配走外部 wrapper：生成 hardhat 配置（--config 指向），
#     require 上游 hardhat.config.js（插件注册等副作用照常生效），仅覆写
#     bcosnet 网络（url/chainId/accounts）与 paths。
#   - 为什么必须 wrapper：上游 hardhat.config.js 把 bcosnet.chainId 硬编码为 20200，而
#     hardhat 的 ChainIdValidatorProvider 会在首个 RPC 请求时校验 config.chainId == 节点
#     eth_chainId，否则抛 INVALID_GLOBAL_CHAIN_ID —— 环境变量无法覆盖它，devnet(901) 必须
#     由外部配置提供 chainId=901（即任务书的「chainId 用 devnet 的 901」）。
#   - 执行：逐文件 `npx hardhat test --config <wrapper> --network bcosnet <file>`，
#     收集每文件 mocha passing/failing/pending + 区块扫描出的该账户交易（入块/回执状态/类型）。
#   - 断言失败 = 允许噪音（C.4）：文件级 assert_fail 不判轮失败；「交易未入块」由报告的
#     tx_total/included/reverted 区分（入块=出现在区块中，included=回执 status 0x1）。
#   - 静态跳过表 SKIP：仅结构性不兼容文件（会在对以太语义链跑时必然无意义/挂死）；断言类
#     失败不跳过（跑出真实 sequencer 信号）。当前为空 —— 首轮实测未发现结构性不兼容文件。
set -euo pipefail

# ---------- 契约参数 ----------
ENDPOINT="" CHAIN_ID="" FUNDING_KEY="" SEGMENT="" ROUND="1"

# ---------- 适配参数（环境可覆盖） ----------
SUITE_DIR="${BCOS_TESTING_DIR:-/tmp/bcos-testing}"
REPO_HTTPS="${BCOS_TESTING_REPO:-https://github.com/FISCO-BCOS/bcos-testing}"
REPO_SSH="git@github.com:FISCO-BCOS/bcos-testing.git"
FILE_TIMEOUT="${BCOS_TESTING_FILE_TIMEOUT:-240}"    # 单文件 hardhat 进程上限（秒）
ROUND_BUDGET="${BCOS_TESTING_ROUND_BUDGET:-270}"    # 整轮执行预算（秒）；超时后剩余文件记 skipped_budget
ROUND_ROTATE="${BCOS_TESTING_ROUND_ROTATE:-5}"      # 轮间起始文件轮转步长（预算截断时保证跨轮覆盖面不同）
TXSCAN="$(cd "$(dirname "$0")" && pwd)/txscan.py"

# 静态跳过表（basename:原因；结构性不兼容才进表，断言噪音不跳 —— 见头注释）
# 例：'Foo-test.js:依赖 FISCO 私有预编译 0x5000'
SKIP_TABLE=""

usage() { sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
log() { printf '[bcos_testing r%s/%s] %s\n' "$ROUND" "$SEGMENT" "$*" >&2; }
die() { log "FAIL: $*"; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --endpoint)    ENDPOINT="${2:?}"; shift 2 ;;
    --chain-id)    CHAIN_ID="${2:?}"; shift 2 ;;
    --funding-key) FUNDING_KEY="${2:?}"; shift 2 ;;
    --segment)     SEGMENT="${2:?}"; shift 2 ;;
    --round)       ROUND="${2:?}"; shift 2 ;;
    --file-timeout)   FILE_TIMEOUT="${2:?}"; shift 2 ;;
    --round-budget)   ROUND_BUDGET="${2:?}"; shift 2 ;;
    -h|--help)     usage ;;
    *) echo "unknown arg: $1" >&2; usage ;;
  esac
done
for v in ENDPOINT CHAIN_ID FUNDING_KEY SEGMENT ROUND; do
  [ -n "${!v}" ] || { echo "missing --${v,,}" >&2; exit 2; }
done
command -v node >/dev/null && command -v npm >/dev/null || die "node/npm required"
command -v python3 >/dev/null || die "python3 required"
command -v cast >/dev/null || die "cast required（资金地址推导用；runner 本身也依赖 cast）"

ROUND_DIR="${TXSOURCE_ROUND_DIR:-$(mktemp -d /tmp/bcos_testing-plugin.XXXXXX)}"
mkdir -p "$ROUND_DIR/files" "$ROUND_DIR/logs"
T0=$(date +%s)

# ---------- prepare：clone + npm ci + 端点/余额自检 ----------
if [ ! -d "$SUITE_DIR/.git" ]; then
  log "cloning bcos-testing -> $SUITE_DIR"
  git clone --depth 1 "$REPO_HTTPS" "$SUITE_DIR" >>"$ROUND_DIR/prepare.log" 2>&1 \
    || git clone --depth 1 "$REPO_SSH" "$SUITE_DIR" >>"$ROUND_DIR/prepare.log" 2>&1 \
    || die "git clone failed (both https and ssh), see $ROUND_DIR/prepare.log"
fi
if [ ! -d "$SUITE_DIR/node_modules/hardhat" ]; then
  log "npm ci in $SUITE_DIR (首次 ~1min)"
  ( cd "$SUITE_DIR" && npm ci ) >>"$ROUND_DIR/prepare.log" 2>&1 \
    || die "npm ci failed, see $ROUND_DIR/prepare.log"
fi
# 上游 package.json 缺直接依赖（实测）：scripts/utils/transactionCreator.js require 了
# ethereum-cryptography/utils 与 @ethereumjs/rlp，但上游只声明了 hardhat-toolbox/dotenv/hardhat；
# 顶层被传递依赖钉在 ethereum-cryptography@0.1.3（无 utils 子路径）→ 3 个 tx 测试文件
# MODULE_NOT_FOUND。补装 v2：--no-save 不触碰上游 package.json/lock（git 树保持零改动）。
if ! ( cd "$SUITE_DIR" && node -e "require('ethereum-cryptography/utils')" ) >/dev/null 2>&1; then
  log "installing missing upstream dep: ethereum-cryptography@^2 (--no-save)"
  ( cd "$SUITE_DIR" && npm install --no-save ethereum-cryptography@^2 ) >>"$ROUND_DIR/prepare.log" 2>&1 \
    || die "missing dep install failed, see $ROUND_DIR/prepare.log"
fi
SUITE_COMMIT="$(git -C "$SUITE_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)"
# 只查 tracked 文件改动（适配 wrapper 是未跟踪文件，运行结束即清理）。
# 踩坑：本机 git 2.40 的 git diff 不吃 --porcelain（exit 255），2>/dev/null 吞掉报错，
# set -e+pipefail 下脚本静默死亡（插件零输出 rc=255 的根因）。substitution 一律 || 兜底。
DIRTY="$(git -C "$SUITE_DIR" diff --name-only 2>/dev/null | wc -l | tr -d ' ')" || DIRTY=99
[ "$DIRTY" = "0" ] || log "WARN: suite has $DIRTY tracked-file modifications（应保持上游原样）"

# 端点连通 + chainId 一致性（fail-fast，避免烧完预算才发现指错链）
res="$(curl -s -m 10 -X POST -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}' "$ENDPOINT" || true)"
got="$(printf '%s' "$res" | python3 -c 'import json,sys
try: print(int(json.load(sys.stdin)["result"], 16))
except Exception: print("")')" || got=""
[ -n "$got" ] || die "endpoint $ENDPOINT not reachable (eth_chainId failed)"
[ "$got" = "$CHAIN_ID" ] || die "endpoint chainId=$got != --chain-id $CHAIN_ID"

FADDR="$(cast wallet address --private-key "$FUNDING_KEY" 2>/dev/null)" || die "funding key malformed"
# 余额自检：runner 契约是每轮先注资；余额 0 说明资金桥未生效 —— fail-fast 报告根因
BAL="$(python3 "$TXSCAN" --rpc "$ENDPOINT" --balance "$FADDR")" || die "balance query failed"
[ "${BAL:-0}" -gt 0 ] || die "funding account $FADDR L2 balance = 0 —— runner 资金桥未生效（契约：runner 负责注资）"
log "endpoint=$ENDPOINT chainId=$CHAIN_ID suite=$SUITE_DIR@$SUITE_COMMIT balance=${BAL}wei from=$FADDR"

# ---------- wrapper 配置（外部适配，上游零改动） ----------
# 注意：wrapper 必须放在套件目录内 —— hardhat v2 的项目根 = --config 文件所在目录，
# 放外面会把上游 paths 判成 HH1007「outside the project」。它是本次运行生成的未跟踪
# 文件，运行结束由 trap 清理，套件 git 状态保持干净（tracked 文件零改动）。
WRAPPER="$SUITE_DIR/.hardhat.devnet.config.js"
cat >"$WRAPPER" <<'EOF'
// GENERATED by txsource/plugins/bcos_testing —— 适配层产物，不属于上游。
// require 上游 hardhat.config.js：usePlugin/编译设置等副作用照常生效；
// 仅覆写 bcosnet（上游硬编码 chainId=20200，会被 hardhat ChainIdValidatorProvider 拒绝非
// 20200 链 —— devnet 901 必须由此提供）与 paths（绝对路径显式回指上游目录）。
const path = require("path");
const SUITE = require("fs").realpathSync(process.env.BCOS_TESTING_DIR); // /tmp 是符号链接，hardhat 对 config dir realpath —— 路径必须对齐，否则 HH1007
const upstream = require(path.join(SUITE, "hardhat.config.js"));
const config = Object.assign({}, upstream, {
  paths: {
    sources: path.join(SUITE, "contracts"),
    tests: path.join(SUITE, "test"),
    cache: path.join(SUITE, "cache"),
    artifacts: path.join(SUITE, "artifacts"),
    ignition: path.join(SUITE, "ignition"),
  },
  networks: Object.assign({}, upstream.networks, {
    bcosnet: {
      url: process.env.BCOS_HOST_URL,
      chainId: parseInt(process.env.BCOS_CHAIN_ID, 10),
      accounts: [process.env.PRIVATE_KEY],
    },
  }),
  // 活网络 2s 出块，mocha 默认 40s 对多笔交易的用例太紧；上游自行声明 timeout 的文件不受影响
  mocha: Object.assign({}, upstream.mocha, { timeout: 300000 }),
});
module.exports = config;
EOF
cleanup() { rm -f "$WRAPPER"; }
trap cleanup EXIT

# ---------- pre-ecotone 发送闸门（P2 实测发现） ----------
# 本 op-geth 构建在 pre-ecotone 段（bedrock/canyon/delta：走 bedrock L1-cost 槽位读取路径，
# rollup_cost.go L1BaseFeeSlot=1/OverheadSlot=5/ScalarSlot=6）校验用户交易时，L1Block 槽位
# 与部署的（jovian 代）L1Block 存储布局错位（实测 slot6 = ecotone 打包 scalars = 2.8e70，
# 被当作裸 scalar）→ panic("overflow in total rollup cost: l1Cost")（rollup_cost.go:373）→
# eth_sendRawTransaction 崩溃且 geth payload builder 死锁（链永久卡死，实测）。
# ecotone 起走 calldata 解码路径不受影响（jovian 段实测 106 笔全通过）。
# 闸门 = pre-ecotone 段整轮不发交易：轮照常触发、报告如实记录（tx_gate + skipped 原因）。
GATED=0
case " bedrock canyon delta " in *" $SEGMENT "*) GATED=1 ;; esac

# ---------- 测试文件清单（上游 test/ 全部 .js，排除 .bak；固定排序 + 轮间轮转） ----------
mapfile -t FILES < <(cd "$SUITE_DIR" && find test -name '*.js' ! -name '*.bak' -type f | sort)
N=${#FILES[@]}
[ "$N" -gt 0 ] || die "no test files found under $SUITE_DIR/test"
OFFSET=$(( (ROUND - 1) * ROUND_ROTATE % N ))

is_skipped() { # $1=basename -> 原因（空=不跳）
  local b="$1" row
  for row in $SKIP_TABLE; do
    case "$row" in "$1:"*) printf '%s' "${row#*:}"; return 0 ;; esac
  done
  return 1
}

# 可移植超时执行（darwin 无 GNU timeout）：超时 -> RUN_RC=124，并尽量清掉 npx 的子进程
RUN_RC=0
run_timeout() { # $1=timeout_s rest=cmd（stdout/stderr 已由调用方重定向）
  local t="$1"; shift
  "$@" &
  local pid=$! waited=0
  while kill -0 "$pid" 2>/dev/null && [ "$waited" -lt "$t" ]; do sleep 1; waited=$((waited+1)); done
  if kill -0 "$pid" 2>/dev/null; then
    log "TIMEOUT after ${t}s: $*"
    pkill -TERM -P "$pid" 2>/dev/null || true
    kill -TERM "$pid" 2>/dev/null || true
    sleep 1
    pkill -KILL -P "$pid" 2>/dev/null || true
    kill -KILL "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
    RUN_RC=124
  else
    wait "$pid" 2>/dev/null; RUN_RC=$?
  fi
}

# ---------- 逐文件执行 ----------
if [ "$GATED" -eq 1 ]; then
  log "TX GATE: segment=$SEGMENT 在 pre-ecotone（op-geth bedrock L1-cost 布局错位 bug），整轮跳过发交易"
  for ((gi=0; gi<N; gi++)); do
    printf '{"file":"%s","status":"skipped_tx_gate","reason":"pre-ecotone op-geth l1Cost overflow panic would wedge the sequencer"}\n' "${FILES[$gi]}" >"$ROUND_DIR/files/$(printf '%02d' "$gi").json"
  done
  python3 - "$ROUND_DIR" "$ENDPOINT" "$CHAIN_ID" "$SEGMENT" "$ROUND" "$SUITE_DIR" "$SUITE_COMMIT" "$FADDR" <<'PYGATE'
import glob, json, os, sys
round_dir, endpoint, chain_id, segment, round_no, suite, commit, faddr = sys.argv[1:9]
skipped = []
for p in sorted(glob.glob(os.path.join(round_dir, "files", "*.json"))):
    d = json.load(open(p))
    skipped.append({"file": d.get("file","?"), "reason": d.get("reason","")})
report = {
    "plugin": "bcos_testing", "segment": segment, "round": int(round_no),
    "endpoint": endpoint, "chain_id": int(chain_id),
    "suite": {"dir": suite, "commit": commit, "files_total": len(skipped)},
    "funding_addr": faddr,
    "tx_total": 0, "included": 0, "reverted": 0, "by_type": {},
    "files_run": 0, "files_passed": 0, "files_assert_fail": 0, "files_error": 0,
    "files_timeout": 0, "files_skipped": len(skipped), "budget_exhausted": False,
    "tests_passed": 0, "tests_failed": 0, "tests_pending": 0, "assertion_failures": 0,
    "files": [], "skipped": skipped, "tx_gate": "pre_ecotone_no_send",
}
print(json.dumps(report))
sys.exit(0)
PYGATE
  exit 0
fi
i=0
files_run=0
budget_hit=""
while [ "$i" -lt "$N" ]; do
  idx=$(( (i + OFFSET) % N ))
  f="${FILES[$idx]}"
  b="$(basename "$f")"

  if reason="$(is_skipped "$b")"; then
    printf '{"file":"%s","status":"skipped","reason":"%s"}\n' "$f" "$reason" >"$ROUND_DIR/files/$(printf '%02d' "$idx").json"
    log "SKIP $f ($reason)"
    i=$((i+1)); continue
  fi

  elapsed=$(( $(date +%s) - T0 ))
  if [ "$elapsed" -ge "$ROUND_BUDGET" ]; then
    budget_hit=1
    printf '{"file":"%s","status":"skipped_budget","reason":"round budget %ss exhausted"}\n' "$f" "$ROUND_BUDGET" >"$ROUND_DIR/files/$(printf '%02d' "$idx").json"
    log "BUDGET: $f deferred (elapsed ${elapsed}s >= budget ${ROUND_BUDGET}s)"
    i=$((i+1)); continue
  fi

  head_before="$(curl -s -m 10 -X POST -H 'Content-Type: application/json' \
    --data '{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}' "$ENDPOINT" \
    | python3 -c 'import json,sys
try: print(int(json.load(sys.stdin)["result"],16))
except Exception: print(0)')"

  log "[$((i+1))/$N] hardhat test $f (head_before=$head_before)"
  flog="$ROUND_DIR/logs/$(printf '%02d' "$idx")-$b.log"
  set +e
  # cwd 必须是套件目录：npx 从 cwd 解析 hardhat/node_modules，位置参数相对 cwd 解析。
  # 不能用 ( cd ... ) 子壳：RUN_RC 由 run_timeout 在子壳内设置会传不出来。
  SUITE_PWD="$PWD"; cd "$SUITE_DIR"
  BCOS_TESTING_DIR="$SUITE_DIR" BCOS_HOST_URL="$ENDPOINT" BCOS_CHAIN_ID="$CHAIN_ID" \
    PRIVATE_KEY="$FUNDING_KEY" run_timeout "$FILE_TIMEOUT" \
    npx hardhat test --config .hardhat.devnet.config.js --network bcosnet "$f" >"$flog" 2>&1
  rc=$RUN_RC
  cd "$SUITE_PWD"
  set -e

  head_after="$(curl -s -m 10 -X POST -H 'Content-Type: application/json' \
    --data '{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}' "$ENDPOINT" \
    | python3 -c 'import json,sys
try: print(int(json.load(sys.stdin)["result"],16))
except Exception: print(0)')"

  # 区块扫描账本：该账户在本文件窗口内入块的交易（含回执状态/类型）
  python3 "$TXSCAN" --rpc "$ENDPOINT" --from "$FADDR" --from-block "$head_before" --to-block "$head_after" \
    --out "$ROUND_DIR/files/$(printf '%02d' "$idx")-txs.json" >"$ROUND_DIR/files/$(printf '%02d' "$idx").scan" 2>>"$flog" \
    || printf '{"tx_total":0,"included":0,"reverted":0,"by_type":{},"scan_error":true}\n' >"$ROUND_DIR/files/$(printf '%02d' "$idx")-txs.json"

  # mocha 汇总解析：passing/failing/pending 各取最后一次出现的数值
  read -r np nf npe <<<"$(python3 - "$flog" <<'PYEOF'
import re, sys
try: text = open(sys.argv[1], errors="replace").read()
except Exception: text = ""
def last(pat):
    m = re.findall(r'(\d+)\s+' + pat, text)
    return int(m[-1]) if m else 0
print(last("passing"), last("failing"), last("pending"))
PYEOF
)"
  if [ "$rc" -eq 0 ]; then status="pass"
  elif [ "$rc" -eq 124 ]; then status="timeout"
  elif [ "${nf:-0}" -gt 0 ]; then status="assert_fail"
  else status="error"; fi
  files_run=$((files_run+1))
  printf '{"file":"%s","status":"%s","exit_code":%d,"tests_passed":%d,"tests_failed":%d,"tests_pending":%d,"head_before":%d,"head_after":%d}\n' \
    "$f" "$status" "$rc" "${np:-0}" "${nf:-0}" "${npe:-0}" "$head_before" "$head_after" \
    >"$ROUND_DIR/files/$(printf '%02d' "$idx").json"
  log "[$((i+1))/$N] $f -> $status (pass=$np fail=$nf pend=$npe rc=$rc txs=$(jq -r '.tx_total // 0' "$ROUND_DIR/files/$(printf '%02d' "$idx")-txs.json" 2>/dev/null || echo ?))"
  i=$((i+1))
done

# ---------- 汇总（stdout 仅此一行 JSON；明细写入 ROUND_DIR 供导出） ----------
python3 - "$ROUND_DIR" "$ENDPOINT" "$CHAIN_ID" "$SEGMENT" "$ROUND" "$SUITE_DIR" "$SUITE_COMMIT" "$FADDR" "$budget_hit" <<'PYEOF'
import glob, json, os, sys
round_dir, endpoint, chain_id, segment, round_no, suite, commit, faddr, budget_hit = sys.argv[1:10]
files, skipped, txs_all, by_type = [], [], [], {}
def jload(p):
    try: return json.load(open(p))
    except Exception: return None
sums = {"tx_total":0,"included":0,"reverted":0,"tests_passed":0,"tests_failed":0,"tests_pending":0}
statuses = {}
for p in sorted(glob.glob(os.path.join(round_dir, "files", "*.json"))):
    if p.endswith("-txs.json"): continue
    d = jload(p)
    if d is None: continue
    f = d.get("file","?")
    status = d.get("status","?")
    statuses[status] = statuses.get(status,0)+1
    t = jload(p.replace(".json","-txs.json")) or {}
    for k in ("tx_total","included","reverted"): sums[k] += int(t.get(k) or 0)
    for k, v in (t.get("by_type") or {}).items(): by_type[k] = by_type.get(k,0)+v
    for tx in (t.get("txs") or []): txs_all.append(tx)
    if status in ("skipped","skipped_budget"):
        skipped.append({"file": f, "reason": d.get("reason","")})
    else:
        files.append({
            "file": f, "status": status, "exit_code": d.get("exit_code"),
            "tests_passed": int(d.get("tests_passed") or 0),
            "tests_failed": int(d.get("tests_failed") or 0),
            "tests_pending": int(d.get("tests_pending") or 0),
            "tx_total": int(t.get("tx_total") or 0),
            "included": int(t.get("included") or 0),
            "reverted": int(t.get("reverted") or 0),
            "by_type": t.get("by_type") or {},
            "head_before": d.get("head_before"), "head_after": d.get("head_after"),
        })
        sums["tests_passed"] += int(d.get("tests_passed") or 0)
        sums["tests_failed"] += int(d.get("tests_failed") or 0)
        sums["tests_pending"] += int(d.get("tests_pending") or 0)
json.dump({"txs": txs_all}, open(os.path.join(round_dir, "txs.json"), "w"), indent=1)
report = {
    "plugin": "bcos_testing",
    "segment": segment, "round": int(round_no),
    "endpoint": endpoint, "chain_id": int(chain_id),
    "suite": {"dir": suite, "commit": commit, "files_total": len(files)+len(skipped)},
    "funding_addr": faddr,
    "tx_total": sums["tx_total"], "included": sums["included"], "reverted": sums["reverted"],
    "by_type": by_type,
    "files_run": len(files), "files_passed": statuses.get("pass",0),
    "files_assert_fail": statuses.get("assert_fail",0), "files_error": statuses.get("error",0),
    "files_timeout": statuses.get("timeout",0), "files_skipped": len(skipped),
    "budget_exhausted": bool(budget_hit),
    "tests_passed": sums["tests_passed"], "tests_failed": sums["tests_failed"],
    "tests_pending": sums["tests_pending"],
    "assertion_failures": sums["tests_failed"],
    "files": files, "skipped": skipped,
}
# 轮判定：断言失败是允许噪音（C.4），不算轮失败；但全部文件都 error/timeout（套件根本
# 没跑起来，属基础设施故障）或一笔交易都没入块时判轮失败。
usable = sum(1 for f in files if f["status"] in ("pass", "assert_fail"))
print(json.dumps(report))
sys.exit(0 if (usable > 0 or sums["tx_total"] > 0) else 1)
PYEOF
