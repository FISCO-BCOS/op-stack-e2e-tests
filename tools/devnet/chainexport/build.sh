#!/usr/bin/env bash
# chainexport/build.sh —— 拷入 op-geth 树构建（模块归属定案，与 generator/regen.sh 的
# cmd/opt8n-ref 同一模式）：
#
#   - 语料仓（本目录 main.go）是**源码唯一真相**；构建时把目录拷进
#     $OPGETH/cmd/chainexport，用 op-geth 的 go.mod 解析编译，产物落到本目录
#     chainexport.bin，随后删除临时目录——op-geth tracked 树保持干净。
#   - 为什么仍走 op-geth 树（即便 main.go 目前只用 stdlib）：① 与 regen.sh 的
#     既有构建仪式一致；② 构建环境被 pin 钉死（HEAD 必须 == e8800cffe…，tracked
#     工作树必须干净），chainexport 消费的 RPC 字段形状正是该 commit 的
#     internal/ethapi MarshalReceipt / txJSON 暴露面——「同 pin 构建」即这层
#     耦合的显式化。
#
# 用法:
#   [OPGETH=/path/to/op-geth] bash build.sh [输出二进制路径（缺省 ./chainexport.bin）]
set -euo pipefail
OPGETH="${OPGETH:-/Users/octopus/octo/code/blockchain-impl/op-geth}"
PIN="e8800cffe53d459cde8a07c8e8f1de9d86e79e07"
DIR="$(cd "$(dirname "$0")" && pwd)"
OUT="${1:-$DIR/chainexport.bin}"
SCRATCH="$OPGETH/cmd/chainexport"

# 错误统一格式（P4 可观测性）：[chainexport][ERROR] <component>: <message>；本脚本无落盘日志，省略 (log: …) 后缀
[ "$(git -C "$OPGETH" rev-parse HEAD)" = "$PIN" ] || { echo "[chainexport][ERROR] build: op-geth HEAD != $PIN (expected $PIN; fix OPGETH checkout or the PIN in build.sh)" >&2; exit 1; }
# 与 regen.sh 同一纪律：只拦「有人手工把东西丢进 op-geth 工作树」；本脚本自己的
# SCRATCH 在 cleanup trap 中删除。
[ -z "$(git -C "$OPGETH" status --porcelain)" ] || { echo "[chainexport][ERROR] build: op-geth worktree dirty (tracked tree must be clean; SCRATCH $SCRATCH is handled by the cleanup trap)" >&2; exit 1; }

cleanup() {
  rm -rf "$SCRATCH"
  exit $?
}
trap cleanup EXIT

rm -rf "$SCRATCH"; mkdir -p "$SCRATCH"; cp "$DIR/main.go" "$SCRATCH/main.go"
( cd "$OPGETH" && go build -o "$OUT" ./cmd/chainexport )
echo "built $OUT (op-geth @ $PIN)"
