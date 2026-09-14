#!/usr/bin/env bash
# demo_transfer.sh —— 演示插件（插件契约的程序生成型原型：自构造交易，不依赖套件）
#
# 行为：用资金桥账户向固定 sink 发 3 笔 0.001 ether 转账，逐笔等回执后输出交易报告。
# 契约（见 ../README.md）：
#   入参：--endpoint <L2 RPC> --chain-id <id> --funding-key <hex私钥> --segment <fork名> --round <n>
#   出参：stdout 仅一行 JSON 报告 {"tx_total":..,"included":..,"reverted":..,"by_type":{...}}；
#         全部日志走 stderr。nonce 管理、签名由插件自担（这里逐笔串行发送，cast 自增 nonce）。
set -euo pipefail

ENDPOINT="" CHAIN_ID="" FUNDING_KEY="" SEGMENT="" ROUND=""
TX_COUNT=3
AMOUNT="0.001ether"
# 固定 sink（anvil acct#6，与 devnet.toml 任何角色不冲突；仅作转账接收地址）
SINK="0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC"

while [ $# -gt 0 ]; do
  case "$1" in
    --endpoint)    ENDPOINT="${2:?}"; shift 2 ;;
    --chain-id)    CHAIN_ID="${2:?}"; shift 2 ;;
    --funding-key) FUNDING_KEY="${2:?}"; shift 2 ;;
    --segment)     SEGMENT="${2:?}"; shift 2 ;;
    --round)       ROUND="${2:?}"; shift 2 ;;
    --tx-count)    TX_COUNT="${2:?}"; shift 2 ;;
    -h|--help) sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

for v in ENDPOINT FUNDING_KEY SEGMENT ROUND; do
  [ -n "${!v}" ] || { echo "missing --${v,,}" >&2; exit 2; }
done
command -v cast >/dev/null || { echo "cast required" >&2; exit 2; }

log() { printf '[demo_plugin r%s/%s] %s\n' "$ROUND" "$SEGMENT" "$*" >&2; }

FROM="$(cast wallet address --private-key "$FUNDING_KEY")"
log "endpoint=$ENDPOINT chain_id=$CHAIN_ID from=$FROM sink=$SINK tx_count=$TX_COUNT"

included=0; reverted=0
TXDIR="${TXSOURCE_ROUND_DIR:-/tmp}"
i=1
while [ "$i" -le "$TX_COUNT" ]; do
  rc="0x0"
  rc="$(cast send "$SINK" --private-key "$FUNDING_KEY" --rpc-url "$ENDPOINT" \
        --value "$AMOUNT" --json </dev/null 2>"$TXDIR/tx-$i.err" \
    | jq -r '.status // "0x0"')" || rc="0x0"
  if [ "$rc" = "0x1" ]; then
    included=$((included+1)); log "tx $i/$TX_COUNT included"
  else
    reverted=$((reverted+1)); log "tx $i/$TX_COUNT REVERTED (status=$rc)"
  fi
  i=$((i+1))
done

# 契约规定：stdout 仅输出 JSON 交易报告
printf '{"tx_total":%d,"included":%d,"reverted":%d,"by_type":{"transfer":%d}}\n' \
  "$TX_COUNT" "$included" "$reverted" "$TX_COUNT"
