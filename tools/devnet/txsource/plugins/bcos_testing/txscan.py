#!/usr/bin/env python3
"""txscan.py —— bcos_testing 插件的区块扫描账本（无第三方依赖，纯 JSON-RPC）。

模式一（区块扫描）：
    txscan.py --rpc URL --from ADDR --from-block N --to-block M [--out PATH]
  扫描 (from_block, to_block] 的每个区块（full tx objects），筛出 from==ADDR 的交易，
  逐笔取回执，输出（并可选写入 PATH）：
    {"tx_total": n, "included": n1, "reverted": n2, "by_type": {"legacy":..,"eip2930":..,"eip1559":..},
     "txs": [{"hash":..,"block":..,"ts":..,"type":..,"status":..,"to":..,"gas_used":..}, ...]}
  语义（与 C.4 对齐）：入块 = 出现在区块（tx_total）；included = 回执 status 0x1；
  reverted = 回执 status 0x0。合约部署的 to=null 原样保留。

模式二（余额）：
    txscan.py --rpc URL --balance ADDR   ->  stdout: 十进制 wei
"""
import argparse
import json
import sys
import time
import urllib.request

TYPE_NAMES = {"0x0": "legacy", "0x1": "eip2930", "0x2": "eip1559", "0x3": "eip4844"}


def rpc(url, method, params):
    req = urllib.request.Request(
        url,
        data=json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params}).encode(),
        headers={"Content-Type": "application/json"},
    )
    last = None
    for attempt in range(4):  # 小重试：devnet 出块间隙偶发连接抖动
        try:
            with urllib.request.urlopen(req, timeout=15) as resp:
                out = json.loads(resp.read())
            if "error" in out:
                raise RuntimeError(f"{method}: {out['error']}")
            return out["result"]
        except Exception as e:  # noqa: BLE001
            last = e
            time.sleep(0.5 * (attempt + 1))
    raise RuntimeError(f"rpc {method} failed after retries: {last}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--rpc", required=True)
    ap.add_argument("--from", dest="from_addr")
    ap.add_argument("--from-block", type=lambda x: int(x, 0))
    ap.add_argument("--to-block", type=lambda x: int(x, 0))
    ap.add_argument("--balance", dest="balance_addr")
    ap.add_argument("--out")
    a = ap.parse_args()

    if a.balance_addr:
        bal = rpc(a.rpc, "eth_getBalance", [a.balance_addr, "latest"])
        print(int(bal, 16))
        return

    if not (a.from_addr and a.from_block is not None and a.to_block is not None):
        ap.error("need --from/--from-block/--to-block or --balance")
    addr = a.from_addr.lower()
    txs, by_type = [], {}
    for n in range(a.from_block + 1, a.to_block + 1):
        blk = rpc(a.rpc, "eth_getBlockByNumber", [hex(n), True])
        if not blk:
            continue  # 链头尚未到该块（文件运行窗口估算偏大时正常）
        ts = int(blk.get("timestamp", "0x0"), 16)
        for tx in blk.get("transactions", []):
            if (tx.get("from") or "").lower() != addr:
                continue
            h = tx["hash"]
            rcpt = rpc(a.rpc, "eth_getTransactionReceipt", [h]) or {}
            status = rcpt.get("status", "")
            t = tx.get("type", "0x0")
            name = TYPE_NAMES.get(t, f"type{t}")
            by_type[name] = by_type.get(name, 0) + 1
            txs.append({
                "hash": h,
                "block": int(blk["number"], 16),
                "ts": ts,
                "type": name,
                "status": status,
                "to": tx.get("to"),
                "gas_used": int(rcpt["gasUsed"], 16) if rcpt.get("gasUsed") else None,
            })
    report = {
        "tx_total": len(txs),
        "included": sum(1 for t in txs if t["status"] == "0x1"),
        "reverted": sum(1 for t in txs if t["status"] == "0x0"),
        "by_type": by_type,
        "txs": txs,
    }
    # 语义（与 C.4 对齐）：入块 = 出现在区块（tx_total）；included = 回执 status 0x1；
    # reverted = 回执 status 0x0。status 空串仅在回执缺失时出现，两者均不计。
    if a.out:
        with open(a.out, "w") as f:
            json.dump(report, f, indent=1)
    print(json.dumps({k: report[k] for k in ("tx_total", "included", "reverted", "by_type")}))


if __name__ == "__main__":
    main()
