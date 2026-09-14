#!/usr/bin/env python3
# rpc_stub.py —— 调度器单测用假 JSON-RPC 桩（不依赖真链）
#
# 按预设"事件脚本"应答：每次收到 eth_blockNumber 或 optimism_syncStatus 轮询请求，
# 事件指针前进一步（最后一个事件常驻重复）；eth_getBlockByNumber / eth_chainId /
# eth_getBalance / eth_sendRawTransaction 等读当前事件应答、不推进指针。
# 因此轮询序列与脚本条目一一对应，测试完全确定。
#
# 用法：rpc_stub.py <scenario.json>   # stdout 打印 "PORT=<port>"，然后写 "<poll#> <method>" 日志行到 stderr
#
# scenario.json 格式：
# {
#   "ts0": 1000,          # block0 时间戳（= rollup genesis.l2_time）
#   "block_time": 2,
#   "chain_id": 901,
#   "balance_wei": "1000000000000000000",
#   "events": [
#     {"head": 0, "safe": 0, "unsafe": 0},   # 每个 JSON-RPC 轮询事件
#     {"head": 626, "safe": 600, "unsafe": 626},
#     ...
#   ]
# }
# 块 n 的时间戳 = ts0 + n*block_time（全事件共享同一线性模型；历史/越界查询同式外推）。
import json
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def main():
    scen = json.load(open(sys.argv[1]))
    ts0 = int(scen["ts0"])
    bt = int(scen.get("block_time", 2))
    events = scen["events"]
    lock = threading.Lock()
    state = {"i": 0, "polls": 0}

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *a):  # 静默默认访问日志
            pass

        def do_POST(self):
            n = int(self.headers.get("Content-Length", 0))
            req = json.loads(self.rfile.read(n) or b"{}")
            method = req.get("method", "")
            params = req.get("params", [])
            advance = method in ("eth_blockNumber", "optimism_syncStatus")
            with lock:
                ev = events[state["i"]]           # 第 k 次轮询 = 第 k 个事件（先应答后推进）
                if advance and state["i"] < len(events) - 1:
                    state["i"] += 1
                state["polls"] += 1
                print(f"{state['polls']:3d} {method}", file=sys.stderr, flush=True)
                result = self.reply(method, params, ev)
            body = json.dumps({"jsonrpc": "2.0", "id": req.get("id", 1), "result": result}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def reply(self, method, params, ev):
            head = int(ev["head"])
            if method == "eth_blockNumber":
                return hex(head)
            if method == "eth_chainId":
                return hex(int(scen.get("chain_id", 901)))
            if method == "eth_getBlockByNumber":
                blk = params[0]
                n = head if blk == "latest" else int(blk, 16)
                return {"number": hex(n), "timestamp": hex(ts0 + n * bt), "hash": "0x" + "ab" * 32}
            if method == "optimism_syncStatus":
                return {
                    "safe_l2": {"number": int(ev.get("safe", ev["head"]))},
                    "unsafe_l2": {"number": int(ev.get("unsafe", ev["head"]))},
                }
            if method == "eth_getBalance":
                return hex(int(scen.get("balance_wei", 10**18)))
            if method == "eth_sendRawTransaction":
                return "0x" + "cd" * 32
            if method == "eth_getTransactionReceipt":
                return {"status": "0x1", "blockNumber": "0x1"}
            if method == "eth_estimateGas":
                return "0x5208"
            if method == "eth_gasPrice":
                return "0x1"
            return None

    srv = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    print(f"PORT={srv.server_address[1]}", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
