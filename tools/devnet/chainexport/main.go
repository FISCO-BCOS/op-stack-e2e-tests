// chainexport — real-devnet chain → t8n-vector exporter (P3-1, Task 5).
//
// Exports a running opdevnet L2 chain as a chain-mode differential vector that
// is field-for-field isomorphic to the ladder vectors
// (opstack-executor/tests/t8n/vectors/ladder_*.json, schema v3-block):
//
//	{ "_op_test_vectors": {version, generator, generator_commit},
//	  "<stem>": { "blocks": [ { _info, env(8 fields), pre?(block0 only),
//	    block:{transactions}, postState?, _op_expected:{header, receipts} } ],
//	    "sampledBlocks"? } }
//
// Data sources (op-geth RPC @ pin e8800cffe):
//   - eth_getBlockByNumber(n, true)   → header fields (gasUsed/receiptsRoot/
//     logsBloom/stateRoot/withdrawalsRoot/requestsHash/blobGasUsed all present
//     per fork) + full tx objects
//   - eth_getBlockReceipts(n)         → receipts incl. the OP-only fields the
//     pin's internal/ethapi MarshalReceipt exposes (l1Fee/l1GasPrice/l1GasUsed/
//     l1BaseFeeScalar/l1BlobBaseFeeScalar/l1FeeScalar/operatorFeeScalar/
//     operatorFeeConstant/daFootprintGasScalar/depositNonce/
//     depositReceiptVersion/blobGasUsed)
//   - eth_getRawTransactionByBlockNumberAndIndex(n, idx) → _op_raw for every
//     NON-deposit tx (deposits are re-encoded from the structured fields)
//   - debug_traceBlockByNumber(n, callTracer) → per-tx return data (the
//     receipt `output` field; the RPC receipt shape carries none)
//   - debug_accountRange (archive node REQUIRED: opdevnet must run geth with
//     --state.scheme hash --gcmode archive; the default path scheme keeps only
//     a ~128-block recent-state window) → pre (full genesis state) + postState
//     dumps (boundary sampling by default)
//
// accountRange completeness (P3-1b fix): the dump call passes incompletes=true
// (this op-geth pin maps it to DumpConfig.OnlyWithAddresses=false) so accounts
// WITHOUT address preimages are included — under the hash state scheme that is
// the entire genesis alloc minus execution-touched accounts, and dropping them
// (the old incompletes=false behavior) silently removed the predeploy
// implementation contracts from `pre` (12 of ~2340 accounts exported). Two
// hash-scheme consequences are repaired state-side, not by the RPC:
//
//   - unaddressed accounts come back keyed "pre(0x<addrHash>)" (core/state/
//     dump.go OnAccount); they are resolved via keccak256(addr)==addrHash
//     against the --genesis alloc addresses (preimage-less ⇒ never
//     execution-written ⇒ must be a genesis account). Unresolvable keys are a
//     hard error, never a silent drop.
//   - storage slots without storage-key preimages are dropped by the dump
//     (dump.go `key == nil → continue`) for EVERY account, and preimage-less
//     accounts additionally dump storage against the zero address. Invariant
//     making the join exact: any slot whose value differs from genesis was
//     execution-written, and execution records storage-key preimages
//     (rawdb.WritePreimages), so it appears in the dump with its raw key.
//     Therefore storage = genesis-alloc slots ∪ dump slots (dump wins) is the
//     complete slot set; the OpT8nReplay postState bidirectional compare
//     (want=0x0 vs got≠0 on a missed slot) is the acceptance gate.
//
// Emission rules mirror opstack-executor/tests/t8n/generator (main.go
// buildExpectedReceipts + assembleOutput) exactly:
//   - nil-able receipt fields → absent keys (omitempty semantics)
//   - _op_l1_fee_scalar only when the RPC decimal scalar is an exact integer
//     (i.e. the raw Bedrock scalar is a 1e6 multiple), emitted as hex of that
//     scaled value (generator: r.FeeScalar.Int(nil) == big.Exact)
//   - _op_operator_fee only when operator-fee params are non-zero (devnet
//     default = 0 → absent both sides); formula per rollup_cost.go
//   - _op_da_footprint (Jovian) = the receipt's blobGasUsed;
//     _op_da_footprint_gas_scalar = the RPC daFootprintGasScalar
//   - deposit _op_deposit: is_system_tx defaults to false when the RPC omits
//     it (op-geth only emits it when true); value is emitted only when
//     non-zero (generator's deposit arms never carried value≠0, but real L1
//     portal deposits do and the FISCO re-encode needs it)
//
// _info.hardfork is decided per block from the rollup.json fork time table
// (the same segment rule the Task 3 txsource runner uses): first block with
// timestamp >= <fork>_time starts the segment; base = regolith.
//
// postState granularity (--poststate): "boundary" (default) samples first,
// last, every 100th block and every activation block ±1 and writes the
// "sampledBlocks" key; "full" emits postState on every block (no
// sampledBlocks key). Consumers treat absent sampledBlocks = every block
// sampled (legacy shape).
//
// Size guard (P5): the exported file is a git-TRACKED corpus input, and GitHub
// rejects any pushed blob >= 100MiB (the 315MB full-mode snapshot registered in
// dc9b945 blocked the branch push outright). A full-postState export whose
// serialization exceeds maxOutputBytes (95 MiB) is therefore auto-downgraded to
// boundary sampling at write time (the state dumps are already in memory — no
// re-fetch) with a "full 超限，已自动降级" notice; a boundary export over the
// ceiling cannot shrink further and only warns loudly (the register/push-side
// big-blob check is the backstop). Default output is thus always < 100MiB.
//
// Block range (--from-block/--to-block): export only blocks in [from, to]
// (1-based, inclusive on both ends; block 0 is the chain's genesis pre and is
// never an exported block). Defaults: from 0 → 1, to 0 → chain head at export
// start. Both error cases are hard errors, no silent clamping: from > to is a
// caller bug; to > head means the chain hasn't produced the requested blocks
// yet (wait or lower the value). Fork activation blocks are still derived
// chain-authoritatively from actual headers 1..to — pre-range blocks are
// header-scanned only (receipts/traces/raws/state dumps stay limited to the
// range) — so per-block hardfork labels and the stem digest are identical to
// a full export of the same prefix; a fork whose boundary is not reached by
// block `to` is excluded from the activation spec (it never labels a range
// block). The stem carries the range: devnet_<from>-<to>_<digest8> for a
// ranged export vs devnet_<head>_<digest8> for a full one. postState boundary
// sampling applies to the range's first/last/every-100th/activation±1 blocks
// (range-relative indices); the first exported block's `pre` is the state
// dump at from-1 (the genesis alloc when from=1, with the self-check below).
//
// Stem rule (mirrors ladderStem): devnet_<blocks>_<digest8> where digest8 =
// sha256("<0:regolith>,<actBlock>:<fork>,...")[:8] — activation block numbers
// are 1-based, taken from the ACTUAL chain headers.
//
// Output: <out-dir>/<stem>.json (json.MarshalIndent, "  ", trailing newline —
// same envelope convention as the generator's writeChainVector) plus a
// DEVNET-STEM line on stdout (the regen.sh LADDER-STEM convention) and the
// file's sha256.
//
// Build: copy-into-op-geth-tree pattern (see build.sh) — the same module
// ownership as generator/regen.sh's cmd/opt8n-ref.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// ----------------------------------------------------------------------------
// fork table
// ----------------------------------------------------------------------------

var forkOrder = []string{"canyon", "delta", "ecotone", "fjord", "granite", "holocene", "isthmus", "jovian"}

// rollupJSON is the subset of op-deployer `inspect rollup` output we consume.
type rollupJSON struct {
	Genesis struct {
		L2Time uint64 `json:"l2_time"`
	} `json:"genesis"`
	BlockTime    uint64             `json:"block_time"`
	ForkTimes    map[string]uint64  `json:"-"`
	RawForkTimes map[string]*uint64 `json:"-"`
}

func loadRollup(path string) (*rollupJSON, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("rollup.json: %w", err)
	}
	r := &rollupJSON{RawForkTimes: map[string]*uint64{}}
	var gen struct {
		Genesis struct {
			L2Time uint64 `json:"l2_time"`
		} `json:"genesis"`
		BlockTime    *uint64           `json:"block_time"`
		ForkTimeList map[string]uint64 `json:"-"`
	}
	if err := json.Unmarshal(data, &gen); err != nil {
		return nil, err
	}
	if gen.Genesis.L2Time == 0 {
		return nil, fmt.Errorf("rollup.json: genesis.l2_time missing/zero")
	}
	r.Genesis.L2Time = gen.Genesis.L2Time
	if gen.BlockTime != nil {
		r.BlockTime = *gen.BlockTime
	} else {
		r.BlockTime = 2
	}
	// fork times: "<fork>_time" integer keys > l2_time (regolith_time==0 stays
	// the genesis base; block_time and friends are not <fork>_time shaped).
	for k, v := range raw {
		if !strings.HasSuffix(k, "_time") {
			continue
		}
		fork := strings.TrimSuffix(k, "_time")
		var tv uint64
		if err := json.Unmarshal(v, &tv); err != nil {
			continue // non-integer *_time key
		}
		r.RawForkTimes[fork] = &tv
	}
	// validate: base must be regolith at genesis (regolith_time == 0). The
	// task scope is the opdevnet 8-fork ladder; anything else is reported, not
	// silently remapped.
	reg, ok := r.RawForkTimes["regolith"]
	if !ok || *reg != 0 {
		return nil, fmt.Errorf("rollup.json: regolith_time missing or != 0 (base fork derivation is out of scope): %v", r.RawForkTimes["regolith"])
	}
	for _, f := range forkOrder {
		if t, ok := r.RawForkTimes[f]; ok && t != nil {
			if *t <= r.Genesis.L2Time {
				return nil, fmt.Errorf("rollup.json: %s_time %d <= l2_time %d", f, *t, r.Genesis.L2Time)
			}
		}
	}
	return r, nil
}

// activeForks returns the forks with a boundary > l2_time, in order.
func (r *rollupJSON) activeForks() []struct {
	name string
	ts   uint64
} {
	var out []struct {
		name string
		ts   uint64
	}
	for _, f := range forkOrder {
		if t, ok := r.RawForkTimes[f]; ok && t != nil && *t > r.Genesis.L2Time {
			out = append(out, struct {
				name string
				ts   uint64
			}{f, *t})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ts < out[j].ts })
	return out
}

// ----------------------------------------------------------------------------
// minimal JSON-RPC client
// ----------------------------------------------------------------------------

type rpcClient struct {
	url   string
	next  int64
	mu    sync.Mutex
	http  *http.Client
	label string
}

func (c *rpcClient) call(result interface{}, method string, params ...interface{}) error {
	c.mu.Lock()
	id := c.next
	c.next++
	c.mu.Unlock()
	req := map[string]interface{}{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	// Robustness hardening (P4 audit 6.1/6.2): the client previously had no
	// timeout (a hung node blocked the export forever — reproduced with an
	// accept-no-reply listener) and no retry (one blip mid-export discarded
	// the whole run). Transport-level failures are retried with backoff;
	// deterministic RPC error results are NOT retried.
	const attempts = 3
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(time.Duration(i) * time.Second)
		}
		err := c.callOnce(result, body, method)
		if err == nil {
			return nil
		}
		lastErr = err
		if isRpcErrorResult(err) { // node answered with a json-rpc error: deterministic
			return err
		}
	}
	return lastErr
}

func isRpcErrorResult(err error) bool {
	var re *rpcError
	return errors.As(err, &re)
}

type rpcError struct {
	Code int
	Msg  string
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Msg) }

func (c *rpcClient) callOnce(result interface{}, body []byte, method string) error {
	resp, err := c.http.Post(c.url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s: read reply: %w", method, err)
	}
	if resp.StatusCode >= 500 || resp.StatusCode == 429 {
		return fmt.Errorf("%s: http %d: %s", method, resp.StatusCode, truncate(string(raw), 200))
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("%s: bad rpc reply %s", method, truncate(string(raw), 200))
	}
	if out.Error != nil {
		return &rpcError{Code: out.Error.Code, Msg: out.Error.Message}
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(out.Result, result)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ----------------------------------------------------------------------------
// canonical quantity/hash formatting (generator hexutil semantics)
// ----------------------------------------------------------------------------

func hexQtyU64(v uint64) string { return "0x" + strconv.FormatUint(v, 16) }

func hexQtyBig(v *big.Int) string { return "0x" + v.Text(16) }

// parseQty parses a JSON-RPC quantity ("0x"-prefixed hex) into a big.Int.
func parseQty(s string) (*big.Int, error) {
	if s == "" {
		return nil, fmt.Errorf("empty quantity")
	}
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		// decimal string (big.Float dumps arrive decimal via a separate path)
		v, ok := new(big.Int).SetString(s, 10)
		if !ok {
			return nil, fmt.Errorf("not a quantity: %q", s)
		}
		return v, nil
	}
	h := s[2:]
	if h == "" {
		return new(big.Int), nil
	}
	v, ok := new(big.Int).SetString(h, 16)
	if !ok {
		return nil, fmt.Errorf("not a quantity: %q", s)
	}
	return v, nil
}

// canonQty re-emits a quantity in the generator's minimal-hex form.
func canonQty(s string) (string, error) {
	v, err := parseQty(s)
	if err != nil {
		return "", err
	}
	return hexQtyBig(v), nil
}

// canonSlotValue re-emits a storage slot VALUE as exactly 32 bytes of hex
// ("0x" + 64 lowercase digits): the replayer parses slot values as bytes32
// (evmc::from_hex rejects odd-length — minimal hex like "0x123" would throw).
// Input is raw hex with an OPTIONAL 0x prefix: dump values arrive without it
// (common.Bytes2Hex), genesis-alloc values usually with it. Slot KEYS go
// through canonHash (already fixed-width).
func canonSlotValue(s string) (string, error) {
	h := strings.ToLower(strings.TrimPrefix(s, "0x"))
	if h == "" {
		h = "0"
	}
	v, ok := new(big.Int).SetString(h, 16)
	if !ok {
		return "", fmt.Errorf("not a hex quantity: %q", s)
	}
	h = v.Text(16)
	if len(h) > 64 {
		return "", fmt.Errorf("slot value exceeds 32 bytes: %q", s)
	}
	return "0x" + strings.Repeat("0", 64-len(h)) + h, nil
}

func canonHash(s string) (string, error) {
	l := strings.ToLower(s)
	if !strings.HasPrefix(l, "0x") || len(l) != 66 {
		return "", fmt.Errorf("not a 32-byte hash: %q", s)
	}
	if _, err := hex.DecodeString(l[2:]); err != nil {
		return "", fmt.Errorf("not a 32-byte hash: %q", s)
	}
	return l, nil
}

func canonAddr(s string) (string, error) {
	l := strings.ToLower(s)
	if !strings.HasPrefix(l, "0x") || len(l) != 42 {
		return "", fmt.Errorf("not a 20-byte address: %q", s)
	}
	if _, err := hex.DecodeString(l[2:]); err != nil {
		return "", fmt.Errorf("not a 20-byte address: %q", s)
	}
	return l, nil
}

// ----------------------------------------------------------------------------
// RPC object shapes (subset actually consumed)
// ----------------------------------------------------------------------------

type rpcAccessTuple struct {
	Address     string   `json:"address"`
	StorageKeys []string `json:"storageKeys"`
}

type rpcTx struct {
	Type                 string           `json:"type"`
	From                 string           `json:"from"`
	To                   *string          `json:"to"`
	Nonce                string           `json:"nonce"`
	Gas                  string           `json:"gas"`
	Value                string           `json:"value"`
	Input                string           `json:"input"`
	ChainID              *string          `json:"chainId"`
	GasPrice             *string          `json:"gasPrice"`
	MaxFeePerGas         *string          `json:"maxFeePerGas"`
	MaxPriorityFeePerGas *string          `json:"maxPriorityFeePerGas"`
	AccessList           []rpcAccessTuple `json:"accessList"`
	Hash                 string           `json:"hash"`
	// deposit-only fields (op-geth txJSON @ pin)
	SourceHash *string `json:"sourceHash"`
	Mint       *string `json:"mint"`
	IsSystemTx *bool   `json:"isSystemTx"`
	// setcode-only
	AuthorizationList []rpcAuthorization `json:"authorizationList"`
}

type rpcAuthorization struct {
	Address   string  `json:"address"`
	ChainID   *string `json:"chainId"`
	Nonce     string  `json:"nonce"`
	YParity   *string `json:"yParity"`
	V         *string `json:"v"`
	R         string  `json:"r"`
	S         string  `json:"s"`
	Authority *string `json:"authority"`
}

type rpcBlock struct {
	Number                string   `json:"number"`
	Hash                  string   `json:"hash"`
	ParentHash            string   `json:"parentHash"`
	Nonce                 *string  `json:"nonce"`
	Timestamp             string   `json:"timestamp"`
	Miner                 string   `json:"miner"`
	MixHash               string   `json:"mixHash"`
	StateRoot             string   `json:"stateRoot"`
	ReceiptsRoot          string   `json:"receiptsRoot"`
	LogsBloom             string   `json:"logsBloom"`
	GasLimit              string   `json:"gasLimit"`
	GasUsed               string   `json:"gasUsed"`
	BaseFeePerGas         *string  `json:"baseFeePerGas"`
	ParentBeaconBlockRoot *string  `json:"parentBeaconBlockRoot"`
	WithdrawalsRoot       *string  `json:"withdrawalsRoot"`
	RequestsHash          *string  `json:"requestsHash"`
	BlobGasUsed           *string  `json:"blobGasUsed"`
	Transactions          []*rpcTx `json:"transactions"`
}

// UnmarshalJSON accepts BOTH response shapes of eth_getBlockByNumber: full
// (transactions = tx objects) and header-only (second arg false — the ranged
// export's pre-range header scan — where transactions = tx HASH strings).
// Decoding a header response into []*rpcTx is a hard unmarshal error, so the
// hash-array shape is detected and dropped: pre-range blocks never assemble,
// they only supply linkage hashes and activation timestamps.
func (b *rpcBlock) UnmarshalJSON(data []byte) error {
	type alias rpcBlock
	aux := struct {
		Transactions json.RawMessage `json:"transactions"`
		*alias
	}{alias: (*alias)(b)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	trimmed := bytes.TrimSpace(aux.Transactions)
	if len(trimmed) == 0 {
		return nil // no transactions field at all
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(aux.Transactions, &arr); err != nil {
		return nil // not an array (null etc.) — treat as header-only shape
	}
	if len(arr) == 0 || arr[0][0] != '{' {
		// header-only response: elements are tx-hash strings, not objects —
		// pre-range blocks never assemble, so drop the list entirely
		b.Transactions = nil
		return nil
	}
	return json.Unmarshal(aux.Transactions, &b.Transactions)
}

type rpcLog struct {
	Address string   `json:"address"`
	Topics  []string `json:"topics"`
	Data    string   `json:"data"`
}

type rpcReceipt struct {
	Type                  string    `json:"type"`
	Status                *string   `json:"status"`
	Root                  *string   `json:"root"`
	GasUsed               string    `json:"gasUsed"`
	CumulativeGasUsed     string    `json:"cumulativeGasUsed"`
	Logs                  []*rpcLog `json:"logs"`
	DepositNonce          *string   `json:"depositNonce"`
	DepositReceiptVersion *string   `json:"depositReceiptVersion"`
	L1GasPrice            *string   `json:"l1GasPrice"`
	L1BlobBaseFee         *string   `json:"l1BlobBaseFee"`
	L1GasUsed             *string   `json:"l1GasUsed"`
	L1Fee                 *string   `json:"l1Fee"`
	L1FeeScalar           *string   `json:"l1FeeScalar"` // decimal string (big.Float)
	L1BaseFeeScalar       *string   `json:"l1BaseFeeScalar"`
	L1BlobBaseFeeScalar   *string   `json:"l1BlobBaseFeeScalar"`
	OperatorFeeScalar     *string   `json:"operatorFeeScalar"`
	OperatorFeeConstant   *string   `json:"operatorFeeConstant"`
	DAFootprintGasScalar  *string   `json:"daFootprintGasScalar"`
	BlobGasUsed           *string   `json:"blobGasUsed"`
}

// ----------------------------------------------------------------------------
// vector output shapes — field ORDER mirrors the generator's structs
// ----------------------------------------------------------------------------

type caseInfo struct {
	Hardfork    string `json:"hardfork"`
	Description string `json:"description"`
}

type outputEnv struct {
	CurrentCoinbase       string `json:"currentCoinbase"`
	CurrentNumber         string `json:"currentNumber"`
	CurrentTimestamp      string `json:"currentTimestamp"`
	CurrentGasLimit       string `json:"currentGasLimit"`
	CurrentBaseFee        string `json:"currentBaseFee"`
	CurrentRandom         string `json:"currentRandom"`
	ParentBeaconBlockRoot string `json:"parentBeaconBlockRoot"`
	ParentHash            string `json:"parentHash"`
}

type depOut struct {
	From       string  `json:"from"`
	To         *string `json:"to"`
	Mint       *string `json:"mint,omitempty"`
	Value      *string `json:"value,omitempty"`
	Gas        string  `json:"gas"`
	IsSystemTx bool    `json:"is_system_tx"`
	SourceHash string  `json:"source_hash"`
}

type depositTxOut struct {
	OpType    string `json:"_op_type"`
	OpDeposit depOut `json:"_op_deposit"`
	Data      string `json:"data"`
}

type accessTupleOut struct {
	Address     string   `json:"address"`
	StorageKeys []string `json:"storageKeys"`
}

type authOut struct {
	ChainID string `json:"chainId"`
	Address string `json:"address"`
	Nonce   string `json:"nonce"`
	YParity string `json:"yParity"`
	R       string `json:"r"`
	S       string `json:"s"`
}

type legacyTxOut struct {
	OpType   string  `json:"_op_type"`
	OpRaw    string  `json:"_op_raw"`
	ChainID  string  `json:"chainId"`
	Nonce    string  `json:"nonce"`
	To       *string `json:"to"`
	Gas      string  `json:"gas"`
	GasPrice string  `json:"gasPrice"`
	Value    string  `json:"value"`
	Data     string  `json:"data"`
	Sender   string  `json:"sender"`
}

type accessListTxOut struct {
	OpType     string           `json:"_op_type"`
	OpRaw      string           `json:"_op_raw"`
	ChainID    string           `json:"chainId"`
	Nonce      string           `json:"nonce"`
	To         *string          `json:"to"`
	Gas        string           `json:"gas"`
	GasPrice   string           `json:"gasPrice"`
	Value      string           `json:"value"`
	Data       string           `json:"data"`
	AccessList []accessTupleOut `json:"accessList,omitempty"`
	Sender     string           `json:"sender"`
}

type eip1559TxOut struct {
	OpType               string  `json:"_op_type"`
	OpRaw                string  `json:"_op_raw"`
	ChainID              string  `json:"chainId"`
	Nonce                string  `json:"nonce"`
	To                   *string `json:"to"`
	Gas                  string  `json:"gas"`
	MaxFeePerGas         string  `json:"maxFeePerGas"`
	MaxPriorityFeePerGas string  `json:"maxPriorityFeePerGas"`
	Value                string  `json:"value"`
	Data                 string  `json:"data"`
	Sender               string  `json:"sender"`
}

type setcodeTxOut struct {
	OpType               string    `json:"_op_type"`
	OpRaw                string    `json:"_op_raw"`
	ChainID              string    `json:"chainId"`
	Nonce                string    `json:"nonce"`
	To                   *string   `json:"to"`
	Gas                  string    `json:"gas"`
	MaxFeePerGas         string    `json:"maxFeePerGas"`
	MaxPriorityFeePerGas string    `json:"maxPriorityFeePerGas"`
	Value                string    `json:"value"`
	Data                 string    `json:"data"`
	OpAuthorizationList  []authOut `json:"_op_authorization_list"`
	Sender               string    `json:"sender"`
}

type outputLog struct {
	Address string   `json:"address"`
	Topics  []string `json:"topics"`
	Data    string   `json:"data"`
}

type expectedReceipt struct {
	Type              string      `json:"type"`
	Status            string      `json:"status"`
	GasUsed           string      `json:"gasUsed"`
	CumulativeGasUsed string      `json:"cumulativeGasUsed"`
	LogsCount         int         `json:"logsCount"`
	Logs              []outputLog `json:"logs,omitempty"`
	Output            string      `json:"output"`

	OpDepositNonce          *string `json:"_op_deposit_nonce,omitempty"`
	OpDepositReceiptVersion *string `json:"_op_deposit_receipt_version,omitempty"`
	OpL1Fee                 *string `json:"_op_l1_fee,omitempty"`
	OpOperatorFee           *string `json:"_op_operator_fee,omitempty"`
	OpDaFootprint           *string `json:"_op_da_footprint,omitempty"`
	OpL1GasPrice            *string `json:"_op_l1_gas_price,omitempty"`
	OpL1BlobBaseFee         *string `json:"_op_l1_blob_base_fee,omitempty"`
	OpL1GasUsed             *string `json:"_op_l1_gas_used,omitempty"`
	OpL1BaseFeeScalar       *string `json:"_op_l1_base_fee_scalar,omitempty"`
	OpL1BlobBaseFeeScalar   *string `json:"_op_l1_blob_base_fee_scalar,omitempty"`
	OpL1FeeScalar           *string `json:"_op_l1_fee_scalar,omitempty"`
	OpOperatorFeeScalar     *string `json:"_op_operator_fee_scalar,omitempty"`
	OpOperatorFeeConstant   *string `json:"_op_operator_fee_constant,omitempty"`
	OpDaFootprintGasScalar  *string `json:"_op_da_footprint_gas_scalar,omitempty"`
}

type expectedHeader struct {
	GasUsed         string  `json:"gasUsed"`
	ReceiptsRoot    string  `json:"receiptsRoot"`
	LogsBloom       string  `json:"logsBloom"`
	WithdrawalsRoot *string `json:"withdrawalsRoot,omitempty"`
	RequestsHash    *string `json:"requestsHash,omitempty"`
	BlobGasUsed     *string `json:"blobGasUsed,omitempty"`
	StateRoot       string  `json:"stateRoot"`
}

type opExpected struct {
	Header   expectedHeader    `json:"header"`
	Receipts []expectedReceipt `json:"receipts"`
}

// postAccount mirrors types.GenesisAlloc marshaling (the generator postState
// shape): balance always, nonce/code/storage omitted when zero/empty.
type postAccount struct {
	Balance string            `json:"balance"`
	Nonce   string            `json:"nonce,omitempty"`
	Code    string            `json:"code,omitempty"`
	Storage map[string]string `json:"storage,omitempty"`
}

// preAccount mirrors the generator's outputAccount: balance/nonce/code always,
// storage only when non-empty (the replayer's TestState loader hard-requires
// all three scalar fields).
type preAccount struct {
	Balance string            `json:"balance"`
	Nonce   string            `json:"nonce"`
	Code    string            `json:"code"`
	Storage map[string]string `json:"storage,omitempty"`
}

type blockOutput struct {
	Info  caseInfo              `json:"_info"`
	Env   outputEnv             `json:"env"`
	Pre   map[string]preAccount `json:"pre,omitempty"`
	Block struct {
		Transactions []json.RawMessage `json:"transactions"`
	} `json:"block"`
	PostState  map[string]postAccount `json:"postState,omitempty"`
	OpExpected opExpected             `json:"_op_expected"`
}

type chainDoc struct {
	Blocks        []blockOutput `json:"blocks"`
	SampledBlocks []int         `json:"sampledBlocks,omitempty"`
}

// ----------------------------------------------------------------------------
// per-block fetch result
// ----------------------------------------------------------------------------

type blockFetch struct {
	num      uint64
	blk      *rpcBlock
	receipts []*rpcReceipt
	raws     map[int]string // tx index → raw envelope (non-deposit only)
	outputs  []string       // per-tx return data (callTracer)
	err      error
}

// ----------------------------------------------------------------------------
// operator fee (rollup_cost.go formulas; only needed when params non-zero)
// ----------------------------------------------------------------------------

func operatorFee(jovian bool, gasUsed, scalar uint64, constant uint64) *big.Int {
	if jovian {
		// gasUsed * scalar * 100 + constant (rollup_cost.go Jovian arm)
		f := new(big.Int).Mul(new(big.Int).SetUint64(gasUsed), new(big.Int).SetUint64(scalar))
		f.Mul(f, big.NewInt(100))
		f.Add(f, new(big.Int).SetUint64(constant))
		return f
	}
	// Isthmus arm: gasUsed * scalar / 1e6 + constant
	f := new(big.Int).Mul(new(big.Int).SetUint64(gasUsed), new(big.Int).SetUint64(scalar))
	f.Div(f, big.NewInt(1_000_000))
	f.Add(f, new(big.Int).SetUint64(constant))
	return f
}

// ----------------------------------------------------------------------------
// main
// ----------------------------------------------------------------------------

func main() {
	rpcURL := flag.String("rpc", "http://127.0.0.1:9545", "L2 geth HTTP RPC (opdevnet: 9545)")
	rollupPath := flag.String("rollup", "/tmp/opdevnet/artifacts/rollup.json", "op-deployer inspect rollup output")
	outDir := flag.String("out-dir", "", "directory for the exported <stem>.json (required)")
	poststate := flag.String("poststate", "boundary", "postState granularity: boundary|full")
	workers := flag.Int("workers", 8, "parallel RPC block-fetch workers")
	gethCommit := flag.String("op-geth-commit", "unknown", "op-geth pin recorded into _op_test_vectors.generator_commit")
	dumpWorkers := flag.Int("dump-workers", 4, "parallel state-dump workers")
	genesisPath := flag.String("genesis", "/tmp/opdevnet/artifacts/genesis.json",
		"geth genesis file whose alloc resolves preimage-less dump keys and backfills storage slots")
	rpcTimeout := flag.Duration("rpc-timeout", 300*time.Second,
		"per-request RPC timeout (a hung node previously blocked the export forever)")
	force := flag.Bool("force", false,
		"overwrite an existing output file (default: refuse — silent overwrite was a footgun)")
	fromBlock := flag.Uint64("from-block", 0,
		"first block to export, inclusive (0 = first block after genesis; block 0 is the genesis pre and is never exported)")
	toBlock := flag.Uint64("to-block", 0,
		"last block to export, inclusive (0 = chain head at export start; a value beyond head is a hard error, not clamped)")
	flag.Parse()
	if err := run(*rpcURL, *rollupPath, *outDir, *poststate, *workers, *dumpWorkers, *gethCommit, *genesisPath, *rpcTimeout, *force, *fromBlock, *toBlock); err != nil {
		fmt.Fprintf(os.Stderr, "[chainexport][ERROR] export: %v\n", err)
		os.Exit(1)
	}
}

// progressEvery returns the block interval for EXPORT progress lines:
// every 5% of the total, clamped to [1, 250].
func progressEvery(total uint64) uint64 {
	e := total / 20
	if e < 1 {
		e = 1
	}
	if e > 250 {
		e = 250
	}
	return e
}

// maxOutputBytes is the hard serialization ceiling for one exported vector
// file: 95 MiB. The devnet snapshot is a git-tracked corpus input and GitHub
// rejects any pushed blob >= 100MiB (the 315MiB full-mode snapshot in dc9b945
// blocked the branch push), so an export above this ceiling is not registrable.
// The full-postState path auto-downgrades to boundary sampling at write time
// (run()'s size guard); a boundary export over the ceiling can only warn.
const maxOutputBytes = 95 << 20

func run(rpcURL, rollupPath, outDir, poststate string, workers, dumpWorkers int, gethCommit, genesisPath string, rpcTimeout time.Duration, force bool, fromBlock, toBlock uint64) error {
	start := time.Now()
	// --from-block/--to-block arg sanity (no RPC needed): from > to is a
	// caller bug, report it before touching the chain
	if fromBlock != 0 && toBlock != 0 && fromBlock > toBlock {
		return fmt.Errorf("--from-block %d > --to-block %d (range is inclusive on both ends)", fromBlock, toBlock)
	}
	if outDir == "" {
		return fmt.Errorf("--out-dir is required")
	}
	full := false
	switch poststate {
	case "boundary":
	case "full":
		full = true
	default:
		return fmt.Errorf("--poststate must be boundary or full, got %q", poststate)
	}
	rollup, err := loadRollup(rollupPath)
	if err != nil {
		return err
	}
	forks := rollup.activeForks()
	forkNames := []string{"regolith"}
	for _, f := range forks {
		forkNames = append(forkNames, f.name)
	}
	client := &rpcClient{url: rpcURL, http: &http.Client{Timeout: rpcTimeout}, label: rpcURL}

	var headHex string
	if err := client.call(&headHex, "eth_blockNumber"); err != nil {
		return err
	}
	head, err := parseQty(headHex)
	if err != nil {
		return err
	}
	n := head.Uint64() // full chain = blocks 1..n (block 0 is the chain's genesis pre)
	if n < 1 {
		return fmt.Errorf("chain has no blocks beyond genesis")
	}
	// ---- block range resolution (--from-block/--to-block) ----------------------
	// Defaults: from 0 → 1 (block 0 is the genesis pre, never an exported
	// block), to 0 → head at export start. The from > to arg error is already
	// rejected above (fail-fast, no RPC); to > head is rejected here — no
	// clamping by design: silently truncating would export a different range
	// than the caller asked for.
	from, to := fromBlock, toBlock
	if from == 0 {
		from = 1
	}
	if to == 0 {
		to = n
	}
	if to > n {
		return fmt.Errorf("--to-block %d > chain head %d (no clamping by design: wait for the chain to grow or lower --to-block)", to, n)
	}
	ranged := from != 1 || to != n
	count := to - from + 1
	if ranged {
		fmt.Fprintf(os.Stderr, "chainexport: range export blocks %d-%d of head %d (postState sampling and dumps act on the range only)\n", from, to, n)
	}
	fmt.Fprintf(os.Stderr, "chainexport: l2_time=%d block_time=%ds forks=%v head=%d\n",
		rollup.Genesis.L2Time, rollup.BlockTime, forkNames, n)

	// ---- pass 1: fetch all blocks + receipts + raws + traces ----------------
	// Blocks before `from` are header-scanned only: fork activation detection
	// needs chain-authoritative timestamps for the whole prefix 1..to, but the
	// receipts/traces/raws of pre-range blocks are never exported.
	fetches := make([]*blockFetch, to)
	if from > 1 {
		fmt.Fprintf(os.Stderr, "chainexport: scanning headers 1..%d for fork activation detection (range export)\n", from-1)
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for i := uint64(1); i <= to; i++ {
		wg.Add(1)
		go func(num uint64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var bf *blockFetch
			var err error
			if num < from {
				bf, err = fetchHeader(client, num)
			} else {
				bf, err = fetchBlock(client, num)
			}
			mu.Lock()
			defer mu.Unlock()
			fetches[num-1] = bf
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("block %d: %w", num, err)
			}
		}(i)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}

	// chain linkage sanity (fetched data is what it claims to be) — headers
	// (light or full) carry hash/parentHash, so this covers the whole 1..to
	// prefix including the header-scanned pre-range blocks
	for i := uint64(2); i <= to; i++ {
		if fetches[i-1].blk.ParentHash != fetches[i-2].blk.Hash {
			return fmt.Errorf("block %d parentHash %s != block %d hash %s",
				i, fetches[i-1].blk.ParentHash, i-1, fetches[i-2].blk.Hash)
		}
	}

	// ---- activation blocks (actual chain, 1-based) --------------------------
	type activation struct {
		name  string
		ts    uint64
		block uint64 // 1-based
	}
	var activations []activation
	for _, f := range forks {
		// first block with ts >= boundary (headers are sorted by ts)
		blk := sortSearchBlock(fetches, f.ts)
		bts, err := parseQty(fetches[blk-1].blk.Timestamp)
		if err != nil {
			return err
		}
		if bts.Uint64() < f.ts {
			// boundary beyond block `to`: the fork never labels a block of
			// this export (ranged export, or the chain hasn't reached the
			// boundary yet) — exclude it from the activation spec/labels
			fmt.Fprintf(os.Stderr, "chainexport: %s boundary %d not reached by block %d (ts %d) — not active in export range\n",
				f.name, f.ts, blk, bts.Uint64())
			continue
		}
		activations = append(activations, activation{f.name, f.ts, blk})
		want := (f.ts - rollup.Genesis.L2Time + rollup.BlockTime - 1) / rollup.BlockTime
		if blk != want {
			fmt.Fprintf(os.Stderr, "chainexport: WARNING: %s activation on chain at block %d, formula ceil((boundary-l2_time)/block_time)=%d (chain is authoritative)\n", f.name, blk, want)
		}
	}
	for i, a := range activations {
		ts, err := parseQty(fetches[a.block-1].blk.Timestamp)
		if err != nil {
			return err
		}
		if i > 0 {
			prev := activations[i-1]
			pts, _ := parseQty(fetches[prev.block-1].blk.Timestamp)
			if pts.Uint64() >= a.ts {
				return fmt.Errorf("fork %s boundary %d not after prior activation ts %d", a.name, a.ts, pts)
			}
		}
		if ts.Uint64() < a.ts {
			return fmt.Errorf("block %d ts %d < %s boundary %d (activation search bug)", a.block, ts, a.name, a.ts)
		}
	}

	// per-block hardfork (segment = last activation with act.block <= num).
	// Delta is a devnet-local rollup-table fork with NO EL-level behavior at
	// this pin (geth chainconfig carries no delta time; attributes.go has no
	// Delta injection branch — Task 1 定案 2/3), and the vector contract admits
	// exactly regolith|canyon|ecotone|fjord|granite|holocene|isthmus|jovian
	// (OpT8nReplayTest rejects anything else). Delta-segment blocks are
	// therefore labeled by their actual EL rules: canyon.
	sawDelta := false
	hardforkOf := func(num uint64) string {
		hf := "regolith"
		for _, a := range activations {
			if a.block <= num {
				hf = a.name
			}
		}
		if hf == "delta" {
			sawDelta = true
			hf = "canyon"
		}
		return hf
	}
	defer func() {
		if sawDelta {
			fmt.Fprintf(os.Stderr, "chainexport: NOTE: delta-segment blocks labeled hardfork=canyon (EL-equivalent at pin e8800cffe; vector contract has no delta label)\n")
		}
	}()

	// ---- stem ----------------------------------------------------------------
	parts := []string{"0:regolith"}
	for _, a := range activations {
		parts = append(parts, fmt.Sprintf("%d:%s", a.block, a.name))
	}
	specDigest := sha256.Sum256([]byte(strings.Join(parts, ",")))
	stemBase := fmt.Sprintf("%d", n)
	if ranged {
		stemBase = fmt.Sprintf("%d-%d", from, to)
	}
	stem := fmt.Sprintf("devnet_%s_%s", stemBase, hex.EncodeToString(specDigest[:])[:8])

	// ---- per-block assembly ---------------------------------------------------
	doc := &chainDoc{Blocks: make([]blockOutput, count)}
	progEvery := progressEvery(count)
	var txCount uint64
	forkCounts := make(map[string]uint64)
	for i := uint64(0); i < count; i++ {
		bf := fetches[from-1+i] // doc index i ↔ block from+i
		num := bf.num
		hf := hardforkOf(num)
		blk, err := assembleBlock(bf, hf, forkNames, hf == "jovian")
		if err != nil {
			return fmt.Errorf("block %d: %w", num, err)
		}
		doc.Blocks[i] = *blk
		txCount += uint64(len(bf.blk.Transactions))
		forkCounts[hf]++
		if (i+1)%progEvery == 0 || i+1 == count {
			fmt.Fprintf(os.Stderr, "chainexport: EXPORT %d/%d blocks (receipts ok, state pages 0)\n", i+1, count)
		}
	}
	// description strings need the range/count; fill here (assembleBlock wrote a
	// placeholder). Full exports keep the historical wording byte-for-byte.
	for i := uint64(0); i < count; i++ {
		if ranged {
			doc.Blocks[i].Info.Description = fmt.Sprintf("devnet chain blocks %d-%d (%s), block %d/%d",
				from, to, strings.Join(forkNames, "->"), from+i, to)
		} else {
			doc.Blocks[i].Info.Description = fmt.Sprintf("devnet chain of %d blocks (%s), block %d/%d",
				n, strings.Join(forkNames, "->"), i+1, n)
		}
	}

	// ---- postState sampling ----------------------------------------------------
	// Indices are doc-relative (0-based into the exported range); activation ±1
	// indices outside the range are dropped by add()'s bounds guard.
	// boundarySamples() is a closure so the size guard's full→boundary downgrade
	// (write section) recomputes the exact same set without re-fetching dumps.
	boundarySamples := func() []int {
		inSample := make(map[int]bool)
		add := func(i int) {
			if i >= 0 && int64(i) < int64(count) {
				inSample[i] = true
			}
		}
		add(0)
		add(int(count) - 1)
		for i := 0; i < int(count); i += 100 {
			add(i)
		}
		for _, a := range activations {
			idx := int(a.block) - int(from) // 0-based doc index of the activation block
			add(idx - 1)
			add(idx)
			add(idx + 1)
		}
		out := make([]int, 0, len(inSample))
		for i := range inSample {
			out = append(out, i)
		}
		sort.Ints(out)
		return out
	}
	var sampledIdxs []int
	if full {
		for i := 0; i < int(count); i++ {
			sampledIdxs = append(sampledIdxs, i)
		}
	} else {
		sampledIdxs = boundarySamples()
		doc.SampledBlocks = sampledIdxs
	}

	// ---- pre: state dump at from-1 (genesis alloc when from=1) ----------------
	join, err := loadStateJoin(genesisPath)
	if err != nil {
		return err
	}
	if ranged {
		fmt.Fprintf(os.Stderr, "chainexport: dumping pre (state at block %d, the first exported block's parent)...\n", from-1)
	} else {
		fmt.Fprintf(os.Stderr, "chainexport: dumping pre (genesis state)...\n")
	}
	preDump, err := dumpState(client, from-1, join)
	if err != nil {
		return fmt.Errorf("state dump at block %d (pre): %w (devnet geth must run --state.scheme hash --gcmode archive)", from-1, err)
	}
	// Genesis self-check (full exports only): the block-0 dump (all accounts,
	// preimage-less ones resolved via this very alloc) must reproduce the
	// alloc's non-empty account set field-for-field. A mismatch means
	// --genesis is not the genesis the running chain was inited with — refuse
	// to emit a vector whose `pre` silently misrepresents reality. Ranged
	// exports dump a mid-chain state (block from-1) where execution-touched
	// accounts legitimately differ from the alloc, so the account-set check
	// does not apply; the preimage-resolution join above still does.
	if from == 1 {
		if len(preDump) != len(join.alloc) {
			return fmt.Errorf("genesis self-check: dump accounts %d != alloc accounts %d (--genesis mismatch?)", len(preDump), len(join.alloc))
		}
		var missing, extra []string
		for addr := range join.alloc {
			if _, ok := preDump[addr]; !ok {
				missing = append(missing, addr)
			}
		}
		for addr := range preDump {
			if _, ok := join.alloc[addr]; !ok {
				extra = append(extra, addr)
			}
		}
		if len(missing) > 0 || len(extra) > 0 {
			return fmt.Errorf("genesis self-check: alloc accounts missing from dump %v; dump keys not in alloc %v (--genesis mismatch?)", missing, extra)
		}
		for addr, want := range join.alloc {
			got, ok := preDump[addr]
			if !ok {
				return fmt.Errorf("genesis self-check: alloc account %s missing from dump", addr)
			}
			if got.Balance != want.Balance || got.Nonce != want.Nonce || got.Code != want.Code {
				return fmt.Errorf("genesis self-check: account %s fields differ from alloc (bal %s/%s nonce %s/%s code %d/%d bytes)",
					addr, got.Balance, want.Balance, want.Nonce, got.Nonce, len(got.Code), len(want.Code))
			}
			for slot, wantVal := range want.Storage {
				gotVal, ok := got.Storage[slot]
				if !ok {
					return fmt.Errorf("genesis self-check: account %s slot %s missing from dump join", addr, slot)
				}
				gn, _ := parseQty(gotVal)
				wn, _ := parseQty(wantVal)
				if gn.Cmp(wn) != 0 {
					return fmt.Errorf("genesis self-check: account %s slot %s value %s != alloc %s", addr, slot, gotVal, wantVal)
				}
			}
		}
		fmt.Fprintf(os.Stderr, "chainexport: genesis self-check OK (%d accounts, storage join verified)\n", len(preDump))
	}
	doc.Blocks[0].Pre = preFromDump(preDump)

	// ---- postState dumps -------------------------------------------------------
	fmt.Fprintf(os.Stderr, "chainexport: dumping %d/%d postStates (%s)...\n", len(sampledIdxs), count, poststate)
	dsem := make(chan struct{}, dumpWorkers)
	var dwg sync.WaitGroup
	var dmu sync.Mutex
	var dumpErr error
	var dumped uint64 // atomic — EXPORT 进度行的 state pages 计数
	for _, idx := range sampledIdxs {
		dwg.Add(1)
		go func(blockNum int) {
			defer dwg.Done()
			dsem <- struct{}{}
			defer func() { <-dsem }()
			st, err := dumpState(client, uint64(int(from)+blockNum), join) // state AFTER 0-based doc index blockNum = state at block from+blockNum
			dmu.Lock()
			defer dmu.Unlock()
			if err != nil && dumpErr == nil {
				dumpErr = fmt.Errorf("postState dump at block %d: %w", int(from)+blockNum, err)
				return
			}
			doc.Blocks[blockNum].PostState = st
			done := atomic.AddUint64(&dumped, 1)
			if done%progEvery == 0 || done == uint64(len(sampledIdxs)) {
				fmt.Fprintf(os.Stderr, "chainexport: EXPORT %d/%d blocks (receipts ok, state pages %d)\n", count, count, done)
			}
		}(idx)
	}
	dwg.Wait()
	if dumpErr != nil {
		return dumpErr
	}

	// ---- write ------------------------------------------------------------------
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	meta, err := json.Marshal(struct {
		Version         string `json:"version"`
		Generator       string `json:"generator"`
		GeneratorCommit string `json:"generator_commit"`
	}{fmt.Sprintf("%d-block", count), "chainexport", gethCommit})
	if err != nil {
		return err
	}
	docBytes, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	out := map[string]json.RawMessage{
		"_op_test_vectors": meta,
		stem:               docBytes,
	}
	outBytes, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	outBytes = append(outBytes, '\n')
	// ---- size guard (P5): the registered artifact must stay pushable ----------
	// A full-postState serialization over maxOutputBytes is auto-downgraded to
	// boundary sampling (the dumps are already in memory: drop the non-sampled
	// postStates, write sampledBlocks, re-serialize) — no re-fetch, same stem
	// (the digest covers only the activation spec, never the poststate mode).
	if len(outBytes) > maxOutputBytes && full {
		fmt.Fprintf(os.Stderr, "chainexport: SIZE GUARD: full-postState output %d bytes > %d MiB ceiling — full 超限，已自动降级为 boundary 采样并重新序列化\n",
			len(outBytes), maxOutputBytes>>20)
		full = false
		poststate = "boundary"
		sampledIdxs = boundarySamples()
		doc.SampledBlocks = sampledIdxs
		sampled := make(map[int]bool, len(sampledIdxs))
		for _, i := range sampledIdxs {
			sampled[i] = true
		}
		for i := range doc.Blocks {
			if !sampled[i] {
				doc.Blocks[i].PostState = nil
			}
		}
		out[stem], err = json.Marshal(doc)
		if err != nil {
			return err
		}
		outBytes, err = json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		outBytes = append(outBytes, '\n')
		fmt.Println("DOWNGRADED full->boundary")
	}
	if len(outBytes) > maxOutputBytes {
		fmt.Fprintf(os.Stderr, "chainexport: WARNING: %s serializes to %d bytes, over the %d MiB ceiling even with boundary sampling — do NOT register/push this file as-is\n",
			stem+".json", len(outBytes), maxOutputBytes>>20)
	}
	file := outDir + "/" + stem + ".json"
	// Refuse to silently clobber a previous export (robustness hardening): the
	// stem embeds the head height + activation spec, so an existing file is
	// either a previous export of the same chain or a colliding run.
	if !force {
		if _, err := os.Stat(file); err == nil {
			return fmt.Errorf("output %s already exists (pass --force to overwrite)", file)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.WriteFile(file, outBytes, 0o644); err != nil {
		return err
	}
	sum := sha256.Sum256(outBytes)
	fmt.Printf("DEVNET-STEM %s\n", stem)
	fmt.Printf("WROTE %s\n", file)
	fmt.Printf("SHA256 %x\n", sum)
	// 结束摘要（P4 可观测性；stderr，stdout 的 STEM/WROTE/SHA256 机读行不受影响）
	var segParts []string
	for _, f := range forkNames {
		segParts = append(segParts, fmt.Sprintf("%s=%d", f, forkCounts[f]))
	}
	fmt.Fprintf(os.Stderr, "chainexport: SUMMARY blocks=%d txs=%d postState blocks=%d/%d elapsed=%s\n",
		count, txCount, len(sampledIdxs), count, time.Since(start).Round(time.Second))
	fmt.Fprintf(os.Stderr, "chainexport: fork segment blocks: %s\n", strings.Join(segParts, " "))
	return nil
}

// sortSearchBlock returns the 1-based number of the first block whose
// timestamp >= ts (fetches are 1..n, timestamps non-decreasing).
func sortSearchBlock(fetches []*blockFetch, ts uint64) uint64 {
	lo, hi := uint64(1), uint64(len(fetches))
	for lo < hi {
		mid := (lo + hi) / 2
		t, _ := parseQty(fetches[mid-1].blk.Timestamp)
		if t.Uint64() >= ts {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

// fetchHeader light-fetches a block header (no tx bodies/receipts/traces).
// Used for blocks before --from-block: fork activation detection needs
// chain-authoritative timestamps for the whole 1..to prefix, but nothing
// before the range is ever assembled or dumped.
func fetchHeader(client *rpcClient, num uint64) (*blockFetch, error) {
	bf := &blockFetch{num: num, raws: map[int]string{}}
	if err := client.call(&bf.blk, "eth_getBlockByNumber", hexQtyU64(num), false); err != nil {
		return nil, err
	}
	if bf.blk == nil {
		return nil, fmt.Errorf("block not found")
	}
	return bf, nil
}

func fetchBlock(client *rpcClient, num uint64) (*blockFetch, error) {
	bf := &blockFetch{num: num, raws: map[int]string{}}
	if err := client.call(&bf.blk, "eth_getBlockByNumber", hexQtyU64(num), true); err != nil {
		return nil, err
	}
	if bf.blk == nil {
		return nil, fmt.Errorf("block not found")
	}
	if err := client.call(&bf.receipts, "eth_getBlockReceipts", hexQtyU64(num)); err != nil {
		return nil, fmt.Errorf("receipts: %w", err)
	}
	if len(bf.receipts) != len(bf.blk.Transactions) {
		return nil, fmt.Errorf("receipts/txs count mismatch: %d vs %d", len(bf.receipts), len(bf.blk.Transactions))
	}
	// per-tx return data via callTracer (the RPC receipt shape carries no output)
	var traces []struct {
		TxHash string `json:"txHash"`
		Result struct {
			Output string `json:"output"`
		} `json:"result"`
	}
	if err := client.call(&traces, "debug_traceBlockByNumber", hexQtyU64(num), map[string]string{"tracer": "callTracer"}); err != nil {
		return nil, fmt.Errorf("trace: %w", err)
	}
	if len(traces) != len(bf.blk.Transactions) {
		return nil, fmt.Errorf("trace/txs count mismatch: %d vs %d", len(traces), len(bf.blk.Transactions))
	}
	bf.outputs = make([]string, len(traces))
	for i := range traces {
		bf.outputs[i] = traces[i].Result.Output
		if bf.outputs[i] == "" {
			bf.outputs[i] = "0x"
		}
	}
	// raw envelopes for every non-deposit tx
	for i, tx := range bf.blk.Transactions {
		if tx.Type == "0x7e" {
			continue
		}
		var raw string
		if err := client.call(&raw, "eth_getRawTransactionByBlockNumberAndIndex", hexQtyU64(num), hexQtyU64(uint64(i))); err != nil {
			return nil, fmt.Errorf("raw tx %d: %w", i, err)
		}
		if raw == "" || raw == "0x" {
			return nil, fmt.Errorf("raw tx %d (%s): empty", i, tx.Hash)
		}
		bf.raws[i] = raw
	}
	return bf, nil
}

// assembleBlock maps one fetched block into the vector shape. The emission
// rules mirror the generator's buildExpectedReceipts/assembleOutput.
func assembleBlock(bf *blockFetch, hardfork string, forkNames []string, isJovian bool) (*blockOutput, error) {
	blk := bf.blk
	num, err := parseQty(blk.Number)
	if err != nil {
		return nil, err
	}
	ts, err := parseQty(blk.Timestamp)
	if err != nil {
		return nil, err
	}
	gasLimit, err := canonQty(blk.GasLimit)
	if err != nil {
		return nil, fmt.Errorf("gasLimit: %w", err)
	}
	gasUsed, err := canonQty(blk.GasUsed)
	if err != nil {
		return nil, fmt.Errorf("gasUsed: %w", err)
	}
	coinbase, err := canonAddr(blk.Miner)
	if err != nil {
		return nil, fmt.Errorf("miner: %w", err)
	}
	parentHash, err := canonHash(blk.ParentHash)
	if err != nil {
		return nil, err
	}
	mixHash, err := canonHash(blk.MixHash)
	if err != nil {
		return nil, err
	}
	stateRoot, err := canonHash(blk.StateRoot)
	if err != nil {
		return nil, err
	}
	receiptsRoot, err := canonHash(blk.ReceiptsRoot)
	if err != nil {
		return nil, err
	}
	bloom := strings.ToLower(blk.LogsBloom)
	if !strings.HasPrefix(bloom, "0x") || len(bloom) != 2+512 {
		return nil, fmt.Errorf("logsBloom must be 0x + 512 hex chars, got %d chars", len(bloom))
	}
	if blk.BaseFeePerGas == nil {
		return nil, fmt.Errorf("pre-London block (no baseFeePerGas) not expressible in the v3-block env schema")
	}
	baseFee, err := canonQty(*blk.BaseFeePerGas)
	if err != nil {
		return nil, fmt.Errorf("baseFee: %w", err)
	}
	// pre-Cancun headers carry no beacon root: emit the zero hash so the env
	// schema stays complete (generator: parentBeaconBlockRootOrZero).
	beaconRoot := "0x0000000000000000000000000000000000000000000000000000000000000000"
	if blk.ParentBeaconBlockRoot != nil {
		if beaconRoot, err = canonHash(*blk.ParentBeaconBlockRoot); err != nil {
			return nil, fmt.Errorf("parentBeaconBlockRoot: %w", err)
		}
	}

	out := &blockOutput{
		Info: caseInfo{Hardfork: hardfork},
		Env: outputEnv{
			CurrentCoinbase:       coinbase,
			CurrentNumber:         hexQtyBig(num),
			CurrentTimestamp:      hexQtyBig(ts),
			CurrentGasLimit:       gasLimit,
			CurrentBaseFee:        baseFee,
			CurrentRandom:         mixHash,
			ParentBeaconBlockRoot: beaconRoot,
			ParentHash:            parentHash,
		},
	}
	// header expectations: presence mirrors the RPC header (nil → absent),
	// which in turn mirrors op-geth's fork-gated header fields.
	hdr := expectedHeader{
		GasUsed:      gasUsed,
		ReceiptsRoot: receiptsRoot,
		LogsBloom:    bloom,
		StateRoot:    stateRoot,
	}
	if blk.WithdrawalsRoot != nil {
		s, err := canonHash(*blk.WithdrawalsRoot)
		if err != nil {
			return nil, fmt.Errorf("withdrawalsRoot: %w", err)
		}
		hdr.WithdrawalsRoot = &s
	}
	if blk.RequestsHash != nil {
		s, err := canonHash(*blk.RequestsHash)
		if err != nil {
			return nil, fmt.Errorf("requestsHash: %w", err)
		}
		hdr.RequestsHash = &s
	}
	if blk.BlobGasUsed != nil {
		s, err := canonQty(*blk.BlobGasUsed)
		if err != nil {
			return nil, fmt.Errorf("header blobGasUsed: %w", err)
		}
		hdr.BlobGasUsed = &s
	}
	out.OpExpected.Header = hdr

	// transactions
	txs := make([]json.RawMessage, 0, len(blk.Transactions))
	for i, tx := range blk.Transactions {
		obj, err := txToObject(tx, bf, i)
		if err != nil {
			return nil, fmt.Errorf("tx %d (%s): %w", i, tx.Hash, err)
		}
		enc, err := json.Marshal(obj)
		if err != nil {
			return nil, err
		}
		txs = append(txs, enc)
	}
	out.Block.Transactions = txs

	// receipts
	expReceipts := make([]expectedReceipt, len(blk.Transactions))
	for i, r := range bf.receipts {
		tx := blk.Transactions[i]
		er, err := receiptToExpected(r, tx, bf.outputs[i], isJovian)
		if err != nil {
			return nil, fmt.Errorf("receipt %d (%s): %w", i, tx.Hash, err)
		}
		expReceipts[i] = er
	}
	out.OpExpected.Receipts = expReceipts
	return out, nil
}

// txToObject maps one RPC tx object into the vector tx shape (generator arms).
func txToObject(tx *rpcTx, bf *blockFetch, idx int) (interface{}, error) {
	sender, err := canonAddr(tx.From)
	if err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}
	var to *string
	if tx.To != nil {
		a, err := canonAddr(*tx.To)
		if err != nil {
			return nil, fmt.Errorf("to: %w", err)
		}
		to = &a
	}
	nonce, err := canonQty(tx.Nonce)
	if err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	gas, err := canonQty(tx.Gas)
	if err != nil {
		return nil, fmt.Errorf("gas: %w", err)
	}
	value, err := canonQty(tx.Value)
	if err != nil {
		return nil, fmt.Errorf("value: %w", err)
	}
	switch tx.Type {
	case "0x7e": // deposit
		if tx.SourceHash == nil {
			return nil, fmt.Errorf("deposit tx missing sourceHash")
		}
		sh, err := canonHash(*tx.SourceHash)
		if err != nil {
			return nil, fmt.Errorf("sourceHash: %w", err)
		}
		isSystem := false
		if tx.IsSystemTx != nil {
			isSystem = *tx.IsSystemTx
		}
		d := depOut{
			From:       sender,
			To:         to,
			Gas:        gas,
			IsSystemTx: isSystem,
			SourceHash: sh,
		}
		if tx.Mint != nil {
			m, err := canonQty(*tx.Mint)
			if err != nil {
				return nil, fmt.Errorf("mint: %w", err)
			}
			d.Mint = &m
		}
		// value: the generator's deposit arms never emitted it (always 0),
		// but real L1-portal deposits carry value == mint and the FISCO
		// re-encode needs it for exact replay. Emit only when non-zero.
		if v, _ := parseQty(tx.Value); v.Sign() != 0 {
			vv := hexQtyBig(v)
			d.Value = &vv
		}
		return depositTxOut{
			OpType:    "deposit",
			OpDeposit: d,
			Data:      strings.ToLower(tx.Input),
		}, nil
	case "0x0": // legacy (EIP-155 protected)
		if tx.ChainID == nil {
			return nil, fmt.Errorf("legacy tx has no chainId (unprotected legacy is not expressible)")
		}
		chainID, err := canonQty(*tx.ChainID)
		if err != nil {
			return nil, fmt.Errorf("chainId: %w", err)
		}
		if tx.GasPrice == nil {
			return nil, fmt.Errorf("legacy tx missing gasPrice")
		}
		gp, err := canonQty(*tx.GasPrice)
		if err != nil {
			return nil, err
		}
		return legacyTxOut{
			OpType:   "legacy",
			OpRaw:    bf.raws[idx],
			ChainID:  chainID,
			Nonce:    nonce,
			To:       to,
			Gas:      gas,
			GasPrice: gp,
			Value:    value,
			Data:     strings.ToLower(tx.Input),
			Sender:   sender,
		}, nil
	case "0x1": // access list (EIP-2930)
		if tx.ChainID == nil {
			return nil, fmt.Errorf("accesslist tx missing chainId")
		}
		chainID, err := canonQty(*tx.ChainID)
		if err != nil {
			return nil, err
		}
		if tx.GasPrice == nil {
			return nil, fmt.Errorf("accesslist tx missing gasPrice")
		}
		gp, err := canonQty(*tx.GasPrice)
		if err != nil {
			return nil, err
		}
		al, err := accessListOut(tx.AccessList)
		if err != nil {
			return nil, err
		}
		return accessListTxOut{
			OpType:     "accesslist",
			OpRaw:      bf.raws[idx],
			ChainID:    chainID,
			Nonce:      nonce,
			To:         to,
			Gas:        gas,
			GasPrice:   gp,
			Value:      value,
			Data:       strings.ToLower(tx.Input),
			AccessList: al,
			Sender:     sender,
		}, nil
	case "0x2": // eip1559
		if tx.ChainID == nil {
			return nil, fmt.Errorf("eip1559 tx missing chainId")
		}
		chainID, err := canonQty(*tx.ChainID)
		if err != nil {
			return nil, err
		}
		if tx.MaxFeePerGas == nil || tx.MaxPriorityFeePerGas == nil {
			return nil, fmt.Errorf("eip1559 tx missing maxFeePerGas/maxPriorityFeePerGas")
		}
		mf, err := canonQty(*tx.MaxFeePerGas)
		if err != nil {
			return nil, err
		}
		mp, err := canonQty(*tx.MaxPriorityFeePerGas)
		if err != nil {
			return nil, err
		}
		return eip1559TxOut{
			OpType:               "eip1559",
			OpRaw:                bf.raws[idx],
			ChainID:              chainID,
			Nonce:                nonce,
			To:                   to,
			Gas:                  gas,
			MaxFeePerGas:         mf,
			MaxPriorityFeePerGas: mp,
			Value:                value,
			Data:                 strings.ToLower(tx.Input),
			Sender:               sender,
		}, nil
	case "0x4": // setcode (7702)
		if tx.ChainID == nil {
			return nil, fmt.Errorf("setcode tx missing chainId")
		}
		chainID, err := canonQty(*tx.ChainID)
		if err != nil {
			return nil, err
		}
		if tx.MaxFeePerGas == nil || tx.MaxPriorityFeePerGas == nil {
			return nil, fmt.Errorf("setcode tx missing maxFeePerGas/maxPriorityFeePerGas")
		}
		mf, err := canonQty(*tx.MaxFeePerGas)
		if err != nil {
			return nil, err
		}
		mp, err := canonQty(*tx.MaxPriorityFeePerGas)
		if err != nil {
			return nil, err
		}
		auths := make([]authOut, 0, len(tx.AuthorizationList))
		for _, a := range tx.AuthorizationList {
			if a.ChainID == nil {
				return nil, fmt.Errorf("authorization tuple missing chainId")
			}
			cid, err := canonQty(*a.ChainID)
			if err != nil {
				return nil, err
			}
			an, err := canonQty(a.Nonce)
			if err != nil {
				return nil, err
			}
			yp := a.YParity
			if yp == nil {
				yp = a.V
			}
			if yp == nil {
				return nil, fmt.Errorf("authorization tuple missing yParity/v")
			}
			y, err := canonQty(*yp)
			if err != nil {
				return nil, err
			}
			r, err := canonQty(a.R)
			if err != nil {
				return nil, err
			}
			s, err := canonQty(a.S)
			if err != nil {
				return nil, err
			}
			addr, err := canonAddr(a.Address)
			if err != nil {
				return nil, err
			}
			auths = append(auths, authOut{
				ChainID: cid, Address: addr, Nonce: an, YParity: y, R: r, S: s,
			})
		}
		return setcodeTxOut{
			OpType:               "setcode",
			OpRaw:                bf.raws[idx],
			ChainID:              chainID,
			Nonce:                nonce,
			To:                   to,
			Gas:                  gas,
			MaxFeePerGas:         mf,
			MaxPriorityFeePerGas: mp,
			Value:                value,
			Data:                 strings.ToLower(tx.Input),
			OpAuthorizationList:  auths,
			Sender:               sender,
		}, nil
	case "0x3":
		return nil, fmt.Errorf("blob tx cannot appear in an OP-devnet block (type-3 is deposit-chain rejected); real chain data says otherwise — refusing to export")
	default:
		return nil, fmt.Errorf("unsupported tx type %q", tx.Type)
	}
}

func accessListOut(in []rpcAccessTuple) ([]accessTupleOut, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]accessTupleOut, 0, len(in))
	for _, e := range in {
		a, err := canonAddr(e.Address)
		if err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(e.StorageKeys))
		for _, k := range e.StorageKeys {
			h, err := canonHash(k)
			if err != nil {
				return nil, err
			}
			keys = append(keys, h)
		}
		out = append(out, accessTupleOut{Address: a, StorageKeys: keys})
	}
	return out, nil
}

// receiptToExpected maps one RPC receipt into the vector receipt shape. Rules
// mirror buildExpectedReceipts: presence mirrors op-geth's deriveOPStackFields
// (nil → absent); the operator aggregate fee is emitted only when the params
// are non-zero; _op_l1_fee_scalar only when the RPC decimal scalar parses to an
// exact integer.
func receiptToExpected(r *rpcReceipt, tx *rpcTx, output string, isJovian bool) (expectedReceipt, error) {
	er := expectedReceipt{}
	typ, err := canonQty(r.Type)
	if err != nil {
		return er, fmt.Errorf("type: %w", err)
	}
	er.Type = typ
	if r.Status == nil {
		return er, fmt.Errorf("receipt carries no status field (pre-byzantium root receipts are out of contract)")
	}
	st, err := canonQty(*r.Status)
	if err != nil {
		return er, fmt.Errorf("status: %w", err)
	}
	er.Status = st
	gu, err := canonQty(r.GasUsed)
	if err != nil {
		return er, fmt.Errorf("gasUsed: %w", err)
	}
	er.GasUsed = gu
	cgu, err := canonQty(r.CumulativeGasUsed)
	if err != nil {
		return er, fmt.Errorf("cumulativeGasUsed: %w", err)
	}
	er.CumulativeGasUsed = cgu
	er.LogsCount = len(r.Logs)
	if len(r.Logs) > 0 {
		logs := make([]outputLog, 0, len(r.Logs))
		for _, l := range r.Logs {
			a, err := canonAddr(l.Address)
			if err != nil {
				return er, fmt.Errorf("log address: %w", err)
			}
			topics := make([]string, 0, len(l.Topics))
			for _, t := range l.Topics {
				h, err := canonHash(t)
				if err != nil {
					return er, fmt.Errorf("log topic: %w", err)
				}
				topics = append(topics, h)
			}
			logs = append(logs, outputLog{Address: a, Topics: topics, Data: strings.ToLower(l.Data)})
		}
		er.Logs = logs
	}
	if output == "" {
		output = "0x"
	}
	er.Output = strings.ToLower(output)

	isDeposit := tx.Type == "0x7e"
	if r.DepositNonce != nil {
		s, err := canonQty(*r.DepositNonce)
		if err != nil {
			return er, fmt.Errorf("depositNonce: %w", err)
		}
		er.OpDepositNonce = &s
	}
	if r.DepositReceiptVersion != nil {
		s, err := canonQty(*r.DepositReceiptVersion)
		if err != nil {
			return er, fmt.Errorf("depositReceiptVersion: %w", err)
		}
		er.OpDepositReceiptVersion = &s
	}
	if !isDeposit {
		if r.L1Fee == nil {
			return er, fmt.Errorf("non-deposit tx missing l1Fee")
		}
		f, err := canonQty(*r.L1Fee)
		if err != nil {
			return er, fmt.Errorf("l1Fee: %w", err)
		}
		er.OpL1Fee = &f
		put := func(dst **string, key, src string, canon func(string) (string, error)) error {
			v, err := canon(src)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*dst = &v
			return nil
		}
		if r.L1GasPrice != nil {
			if err := put(&er.OpL1GasPrice, "l1GasPrice", *r.L1GasPrice, canonQty); err != nil {
				return er, err
			}
		}
		if r.L1BlobBaseFee != nil {
			if err := put(&er.OpL1BlobBaseFee, "l1BlobBaseFee", *r.L1BlobBaseFee, canonQty); err != nil {
				return er, err
			}
		}
		if r.L1GasUsed != nil {
			if err := put(&er.OpL1GasUsed, "l1GasUsed", *r.L1GasUsed, canonQty); err != nil {
				return er, err
			}
		}
		if r.L1BaseFeeScalar != nil {
			if err := put(&er.OpL1BaseFeeScalar, "l1BaseFeeScalar", *r.L1BaseFeeScalar, canonQty); err != nil {
				return er, err
			}
		}
		if r.L1BlobBaseFeeScalar != nil {
			if err := put(&er.OpL1BlobBaseFeeScalar, "l1BlobBaseFeeScalar", *r.L1BlobBaseFeeScalar, canonQty); err != nil {
				return er, err
			}
		}
		// Bedrock-era only (op-geth leaves FeeScalar nil from Ecotone on). The
		// RPC decimal string is the SCALED value (raw scalar / 1e6); emit the
		// hex of that integer only when exact (generator: big.Exact).
		if r.L1FeeScalar != nil {
			f, _, err := big.ParseFloat(*r.L1FeeScalar, 10, 512, big.ToNearestEven)
			if err != nil {
				return er, fmt.Errorf("l1FeeScalar %q: %w", *r.L1FeeScalar, err)
			}
			scaled, acc := f.Int(nil)
			if acc == big.Exact {
				s := hexQtyBig(scaled)
				er.OpL1FeeScalar = &s
			}
		}
		if r.OperatorFeeScalar != nil {
			if err := put(&er.OpOperatorFeeScalar, "operatorFeeScalar", *r.OperatorFeeScalar, canonQty); err != nil {
				return er, err
			}
		}
		if r.OperatorFeeConstant != nil {
			if err := put(&er.OpOperatorFeeConstant, "operatorFeeConstant", *r.OperatorFeeConstant, canonQty); err != nil {
				return er, err
			}
		}
		if r.DAFootprintGasScalar != nil {
			if err := put(&er.OpDaFootprintGasScalar, "daFootprintGasScalar", *r.DAFootprintGasScalar, canonQty); err != nil {
				return er, err
			}
		}
		// operator aggregate fee: only when params non-zero (mirrors
		// deriveOPStackFields). Formula per rollup_cost.go (iron rule 7).
		if r.OperatorFeeScalar != nil || r.OperatorFeeConstant != nil {
			var scalar, constant uint64
			if r.OperatorFeeScalar != nil {
				v, err := parseQty(*r.OperatorFeeScalar)
				if err != nil {
					return er, err
				}
				scalar = v.Uint64()
			}
			if r.OperatorFeeConstant != nil {
				v, err := parseQty(*r.OperatorFeeConstant)
				if err != nil {
					return er, err
				}
				constant = v.Uint64()
			}
			gasUsed, _ := parseQty(r.GasUsed)
			fee := operatorFee(isJovian, gasUsed.Uint64(), scalar, constant)
			sf := hexQtyBig(fee)
			er.OpOperatorFee = &sf
		}
		if isJovian {
			if r.BlobGasUsed == nil {
				return er, fmt.Errorf("jovian non-deposit receipt missing blobGasUsed (DA footprint)")
			}
			d, err := canonQty(*r.BlobGasUsed)
			if err != nil {
				return er, fmt.Errorf("blobGasUsed: %w", err)
			}
			er.OpDaFootprint = &d
		}
	}
	return er, nil
}

// ----------------------------------------------------------------------------
// state dumps (debug_accountRange, paginated ≤256)
// ----------------------------------------------------------------------------

type dumpAccount struct {
	Balance string            `json:"balance"` // DECIMAL string
	Nonce   json.Number       `json:"nonce"`   // number
	Code    string            `json:"code"`    // "0x..." (may be absent)
	Storage map[string]string `json:"storage"` // "0x<slot>" → hex WITHOUT 0x prefix
	// AddressHash (dump.go sets it for EVERY account, addressed or not). Used
	// as the pagination seek position: resuming from the dump's own `next`
	// cursor loses the account AT the cursor (tr.Iterator.Next() advances past
	// the seek node), so we re-seek to the last EMITTED account instead —
	// skipping it on resume is correct because it was already emitted.
	Key string `json:"key"`
}

type dumpResult struct {
	Root     string                 `json:"root"`
	Accounts map[string]dumpAccount `json:"accounts"`
	// Next: raw []byte on the Go side → JSON **base64** (geth quirk); the
	// `start` RPC parameter wants "0x"-hex, so decode before continuing.
	Next *string `json:"next"`
}

// stateJoin repairs the two hash-scheme consequences of a preimage-less dump
// (see the header comment "accountRange completeness"):
//   - addrByTrieKey resolves "pre(0x<addrHash>)" dump keys back to addresses
//     via keccak256(addr)==addrHash over the --genesis alloc (a preimage-less
//     account was never execution-written, hence must be a genesis account);
//   - allocStorage carries the alloc's raw storage slots so the merge in
//     dumpState can backfill slots the dump drops (no storage-key preimage).
type stateJoin struct {
	addrByTrieKey map[common.Hash]string
	alloc         map[string]postAccount // canonical lowercase addr → alloc account
}

// genesisFile mirrors the geth genesis JSON subset we consume.
type genesisFile struct {
	Alloc map[string]struct {
		Balance string            `json:"balance"`
		Nonce   string            `json:"nonce"`
		Code    string            `json:"code"`
		Storage map[string]string `json:"storage"`
	} `json:"alloc"`
}

// loadStateJoin parses the geth genesis file and builds the trie-key reverse
// map. Alloc keys may carry "0x" or not (geth tolerates both); storage values
// are right-padded quantities in the wild, normalized to minimal hex here.
func loadStateJoin(path string) (*stateJoin, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("genesis alloc: %w", err)
	}
	var g genesisFile
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, fmt.Errorf("genesis alloc: %w", err)
	}
	join := &stateJoin{
		addrByTrieKey: make(map[common.Hash]string, len(g.Alloc)),
		alloc:         make(map[string]postAccount, len(g.Alloc)),
	}
	for key, acc := range g.Alloc {
		ks := strings.ToLower(strings.TrimPrefix(key, "0x"))
		if len(ks) != 40 {
			return nil, fmt.Errorf("genesis alloc: bad address %q", key)
		}
		addrRaw, err := hex.DecodeString(ks)
		if err != nil {
			return nil, fmt.Errorf("genesis alloc: bad address %q: %w", key, err)
		}
		var addrBytes common.Address
		copy(addrBytes[:], addrRaw)
		// Vector pre/postState key shape (ladder vectors, generator GenesisAlloc
		// marshaling): "0x" + lowercase hex. canonAddr produces the same shape.
		join.addrByTrieKey[crypto.Keccak256Hash(addrBytes[:])] = "0x" + ks

		pa := postAccount{}
		bal, ok := new(big.Int).SetString(strings.TrimPrefix(acc.Balance, "0x"), 16)
		if !ok {
			return nil, fmt.Errorf("genesis alloc %s: balance %q not hex", key, acc.Balance)
		}
		pa.Balance = hexQtyBig(bal)
		if acc.Nonce != "" {
			nn, err := parseQty(acc.Nonce)
			if err != nil {
				return nil, fmt.Errorf("genesis alloc %s: nonce: %w", key, err)
			}
			if nn.Sign() > 0 {
				pa.Nonce = hexQtyBig(nn)
			}
		}
		if c := strings.ToLower(acc.Code); c != "" && c != "0x" {
			pa.Code = c
		}
		if len(acc.Storage) > 0 {
			st := make(map[string]string, len(acc.Storage))
			for slot, val := range acc.Storage {
				sh, err := canonHash(slot)
				if err != nil {
					return nil, fmt.Errorf("genesis alloc %s: slot %q: %w", key, slot, err)
				}
				v, err := canonSlotValue(val)
				if err != nil {
					return nil, fmt.Errorf("genesis alloc %s: slot %q value: %w", key, slot, err)
				}
				st[sh] = v
			}
			pa.Storage = st
		}
		join.alloc["0x"+ks] = pa
	}
	return join, nil
}

// dumpState returns the full account set at the state of block `num`
// (0 = genesis state, the chain replay's `pre`). Preimage-less accounts arrive
// keyed "pre(0x<addrHash>)" and are resolved through the genesis-alloc join;
// storage is genesis-alloc slots overlaid with the dump's (execution-recorded,
// raw-keyed) slots — dump wins. Unresolvable keys are a hard error.
func dumpState(client *rpcClient, num uint64, join *stateJoin) (map[string]postAccount, error) {
	out := map[string]postAccount{}
	start := "0x"
	lastKey, lastAddr := "", "" // page's max AddressHash + its resolved address
	dropPrev := ""              // the address expected to be re-emitted at the next page head
	for page := 0; ; page++ {
		var res dumpResult
		// nocode=false, nostorage=false, incompletes=true (→
		// DumpConfig.OnlyWithAddresses=false): include preimage-less accounts.
		if err := client.call(&res, "debug_accountRange", hexQtyU64(num), start, 256, false, false, true); err != nil {
			return nil, err
		}
		for key, acc := range res.Accounts {
			var addr string
			if strings.HasPrefix(key, "pre(") && strings.HasSuffix(key, ")") {
				h, err := canonHash(key[len("pre(") : len(key)-1])
				if err != nil {
					return nil, fmt.Errorf("dump preimage-less key %q: %w", key, err)
				}
				a, ok := join.addrByTrieKey[common.HexToHash(h)]
				if !ok {
					return nil, fmt.Errorf("dump: unresolvable preimage-less key %s "+
						"(not a --genesis alloc address; a non-genesis account lost its preimage?)", key)
				}
				addr = a
			} else {
				a, err := canonAddr(key) // dump keys are EIP-55 checksummed
				if err != nil {
					return nil, fmt.Errorf("dump key %q: %w", key, err)
				}
				addr = a
			}
			// Track the page's max AddressHash account TOGETHER with its resolved
			// address: dropPrev below needs the ADDRESS, and execution-created
			// accounts (bcos-testing funding EOA / deployed contracts) have no
			// genesis-alloc preimage — resolving them through addrByTrieKey
			// silently yields "" and the re-emitted boundary account then
			// duplicates across pages (hard error at the dup check).
			if acc.Key > lastKey {
				lastKey, lastAddr = acc.Key, addr
			}
			if addr == dropPrev {
				dropPrev = "" // expected boundary re-emission; drop once
				continue
			}
			if _, dup := out[addr]; dup {
				return nil, fmt.Errorf("dump: duplicate account %s across pages", addr)
			}
			pa := postAccount{}
			bal, ok := new(big.Int).SetString(acc.Balance, 10)
			if !ok {
				return nil, fmt.Errorf("dump %s: balance %q not decimal", addr, acc.Balance)
			}
			pa.Balance = hexQtyBig(bal)
			nn, err := strconv.ParseUint(acc.Nonce.String(), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("dump %s: nonce %q: %w", addr, acc.Nonce.String(), err)
			}
			if nn > 0 {
				pa.Nonce = hexQtyU64(nn)
			}
			if c := strings.ToLower(acc.Code); c != "" && c != "0x" {
				pa.Code = c
			}
			if len(acc.Storage) > 0 {
				st := make(map[string]string, len(acc.Storage))
				for slot, val := range acc.Storage {
					sh, err := canonHash(slot)
					if err != nil {
						return nil, fmt.Errorf("dump %s: storage slot %q: %w", addr, slot, err)
					}
					// dump values are hex WITHOUT the 0x prefix (common.Bytes2Hex,
					// already 64 digits); normalize through canonSlotValue anyway
					// so both join sources share one fixed-width shape.
					v, err := canonSlotValue(val)
					if err != nil {
						return nil, fmt.Errorf("dump %s: storage slot %s value: %w", addr, sh, err)
					}
					st[sh] = v
				}
				pa.Storage = st
			}
			// Storage completeness join: backfill the alloc's raw-keyed slots
			// (dump drops slots without storage-key preimages, and preimage-less
			// accounts dump storage against the zero address). The dump's own
			// slots (execution writes carry recorded preimages) win.
			if ga, inAlloc := join.alloc[addr]; inAlloc && len(ga.Storage) > 0 {
				st := make(map[string]string, len(ga.Storage)+len(pa.Storage))
				for slot, val := range ga.Storage {
					st[slot] = val
				}
				for slot, val := range pa.Storage {
					st[slot] = val
				}
				pa.Storage = st
			}
			out[addr] = pa
		}
		if res.Next == nil || *res.Next == "" || len(res.Accounts) == 0 {
			break
		}
		// Pagination resume: seek to the last EMITTED account's trie key (the
		// max AddressHash on this page — Accounts is an unordered JSON map),
		// carrying its already-resolved address as the expected page-head
		// re-emission. Empirics (this pin): a nodeIterator seek positions
		// BEFORE the seek key, so the next page RE-EMITS that key; drop it
		// (dropPrev) instead of erroring. Resuming from res.Next instead loses
		// the account AT the cursor (tr.Iterator.Next() advances past the seek
		// node).
		if lastKey == "" {
			return nil, fmt.Errorf("dump: page %d has no address hashes for pagination", page)
		}
		lh, err := canonHash(lastKey)
		if err != nil {
			return nil, fmt.Errorf("dump: page %d bad last address hash %q: %w", page, lastKey, err)
		}
		start = lh
		dropPrev = lastAddr
		if page > 1_000_000 {
			return nil, fmt.Errorf("dump: pagination did not terminate")
		}
	}
	return out, nil
}

// preFromDump converts a state dump into the `pre` shape (the replayer's
// TestState loader hard-requires balance/nonce/code on every account).
func preFromDump(dump map[string]postAccount) map[string]preAccount {
	pre := make(map[string]preAccount, len(dump))
	for addr, pa := range dump {
		nonce := pa.Nonce
		if nonce == "" {
			nonce = "0x0"
		}
		code := pa.Code
		if code == "" {
			code = "0x"
		}
		pre[addr] = preAccount{Balance: pa.Balance, Nonce: nonce, Code: code, Storage: pa.Storage}
	}
	return pre
}
