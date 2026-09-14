package main

// P2-C fork-sensitivity probes (design ladder differential, stage C):
// SELFDESTRUCT in both shapes, the EIP-150 63/64 gas boundary, and the
// fork-gated opcodes (PUSH0 / MCOPY / BLOBHASH). All injected ONCE PER FORK
// SEGMENT on the segment probe blocks (ladderCreateCallProbeBlocks), appended
// after the P2-B tx-type probes.
//
//   - Cross-tx SELFDESTRUCT (fork-sensitive, THE differential point): a
//     genesis predeploy per fork segment (ladderSelfdestructProbeAddr(seq),
//     funded 1 ETH, code = PUSH20 beneficiary; SELFDESTRUCT). Executing the
//     same 22 bytes on both sides of the Ecotone (= Cancun/EIP-6780) boundary
//     yields DIFFERENT state: pre-Ecotone (regolith/canyon segments) the
//     account is DELETED from the trie (postState emits the explicit all-zero
//     row {"balance":"0x0"} via the candidate set); Ecotone and later the
//     account is KEPT (only the balance moves to the beneficiary) and stays in
//     postState as {"balance":"0x0","code":...,"nonce":"0x1"}. The golden
//     naturally captures whichever side op-geth takes; the replayer must
//     reproduce it (bcos-evm Host::selfdestruct gates on EVMC_CANCUN &&
//     !just_created, so both sides agree).
//   - Same-tx create+SELFDESTRUCT (fork-invariant deletion): a CREATE whose
//     init code is SELFDESTRUCT-to-beneficiary. The created account is
//     destroyed in the SAME transaction, which deletes it on every fork
//     (pre-6780: unconditional; 6780: same-tx exemption). The created address
//     is declared a candidate for the creating block only, so postState shows
//     the explicit all-zero shape; the create receipt's output is empty on
//     both sides (no runtime returned), i.e. no FINDING-create-output row.
//   - EIP-150 63/64 boundary: a genesis predeploy caller that forwards ALL its
//     gas (GAS) via CALL to the P2-A INVALID predeploy (0xfe burns everything
//     it is given). The 63/64 rule retains 1/64 in the caller frame, so the
//     receipt is status 1 with an exactly-pinnable gasUsed -- a precise
//     end-to-end gas accounting probe.
//   - Fork-sensitive opcodes: a genesis predeploy with three selector arms --
//     PUSH0 (Shanghai = Canyon), MCOPY and BLOBHASH (Cancun = Ecotone). The
//     dispatch is London-safe (the P2-A constraint), the gated opcode appears
//     only inside its arm, so calling an arm BEFORE its fork activates is
//     INVALID (status 0, gasUsed == gasLimit) and AFTER is success (status 1):
//     two hard activation-boundary flips captured by the same golden.
//
// All new accounts are genesis predeploys (Nonce 1 + code keeps each non-empty
// under EIP-158) and join the every-block candidate set, so their code
// survives the per-block pre chain between probes (same rationale as
// ladderContractProbeAddrs).

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

var (
	// ladderGasBoundaryCallerAddr: CALL(GAS, ladderInvalidProbeAddr) x63/64.
	ladderGasBoundaryCallerAddr = common.HexToAddress("0xc0de00000000000000000000000000000000000e")
	// ladderOpcodeProbeAddr: PUSH0 / MCOPY / BLOBHASH selector arms.
	ladderOpcodeProbeAddr = common.HexToAddress("0xc0de00000000000000000000000000000000000f")
)

// ladderSelfdestructProbeCount: one cross-tx SELFDESTRUCT probe per fork
// segment (the registered 8-fork ladder uses seq 0..7). A segment's probe is
// called exactly once (in its own segment), so pre-Ecotone deletions never
// starve a later segment of a live target.
const ladderSelfdestructProbeCount = 8

// ladderSelfdestructProbeAddr: seq 0..7 -> 0xc0de...0010..0017 (the same
// synthetic 0xc0de namespace; 0001-000d are taken).
func ladderSelfdestructProbeAddr(seq int) common.Address {
	if seq < 0 || seq >= ladderSelfdestructProbeCount {
		panic(fmt.Sprintf("ladderSelfdestructProbeAddr: seq %d out of range", seq))
	}
	return common.HexToAddress(fmt.Sprintf("0xc0de00000000000000000000000000000000001%x", seq))
}

// ladderForkSensitivityProbeAddrs returns every P2-C predeploy address (fresh
// slice each call: callers append it to per-block candidate sets).
func ladderForkSensitivityProbeAddrs() []common.Address {
	out := make([]common.Address, 0, 2+ladderSelfdestructProbeCount)
	out = append(out, ladderGasBoundaryCallerAddr, ladderOpcodeProbeAddr)
	for k := 0; k < ladderSelfdestructProbeCount; k++ {
		out = append(out, ladderSelfdestructProbeAddr(k))
	}
	return out
}

const (
	// 21000 intrinsic + CALL cold 2600 + SELFDESTRUCT 5000 + cold beneficiary
	// 2600: 100k is ample headroom and stays deterministic.
	ladderSelfdestructCallGas = 100_000
	// CREATE intrinsic 32000 + init (PUSH20+SELFDESTRUCT + cold beneficiary).
	ladderCreateDestructGas = 200_000
	// 0.1 ETH (1e17 wei) endowment: the same-tx destruction TRANSFERS
	// observable value to the beneficiary (recA) on every fork.
	ladderCreateDestructEndowment = 100_000_000_000_000_000
	// caller: 21000 intrinsic + dispatch + the 63/64-bounded CALL.
	ladderGasBoundaryProbeGas = 150_000
	// opcode arms: 21000 intrinsic + dispatch + the arm; INVALID burns the rest.
	ladderOpcodeProbeGas = 100_000
)

// ladderSelfdestructCode is PUSH20 beneficiary; SELFDESTRUCT (22 bytes). Used
// verbatim as the cross-tx probes' runtime AND as the same-tx init code.
func ladderSelfdestructCode(beneficiary common.Address) []byte {
	a := newLadderAsm()
	a.push(beneficiary.Bytes())
	a.raw(0xff)
	return a.assemble()
}

// ladderGasBoundaryCallerCode forwards ALL available gas (GAS) in one CALL to
// callee; the 63/64 rule keeps 1/64 of the caller frame so the POP+STOP after
// the (inevitably exhausted) callee still runs and the receipt is status 1.
func ladderGasBoundaryCallerCode(callee common.Address) []byte {
	a := newLadderAsm()
	a.raw(0x60, 0x00)      // outSize 0
	a.raw(0x60, 0x00)      // outOff 0
	a.raw(0x60, 0x00)      // inSize 0
	a.raw(0x60, 0x00)      // inOff 0
	a.raw(0x60, 0x00)      // value 0
	a.push(callee.Bytes()) // PUSH20 callee
	a.raw(0x5a)            // GAS
	a.raw(0xf1)            // CALL
	a.raw(0x50)            // POP (ignore the 0 result: the callee exhausted its 63/64)
	a.raw(0x00)            // STOP
	return a.assemble()
}

// ladderOpcodeProbeSelectors are the three keccak-derived entry points, in
// activation order: PUSH0 (Canyon), MCOPY (Ecotone), BLOBHASH (Ecotone).
var ladderOpcodeProbeSelectors = func() [3][]byte {
	sigs := [...]string{"ladderPush0()", "ladderMcopy()", "ladderBlobhash()"}
	var out [3][]byte
	for i, s := range sigs {
		out[i] = crypto.Keccak256([]byte(s))[:4]
	}
	return out
}()

// ladderOpcodeProbeCode is a selector-dispatched probe whose dispatch is
// London-safe (the regolith segment executes it BEFORE Canyon, where PUSH0 is
// undefined -- same constraint as the P2-A probes); each gated opcode appears
// only inside its own arm:
//
//	0: PUSH0; POP; STOP            -- INVALID before Canyon/Shanghai
//	1: MCOPY 32 bytes; STOP        -- INVALID before Ecotone/Cancun
//	2: BLOBHASH(0); POP; STOP      -- INVALID before Ecotone/Cancun
func ladderOpcodeProbeCode() []byte {
	a := newLadderAsm()
	// selector = uint32(CALLDATALOAD(0) >> 224)
	a.raw(0x60, 0x00, 0x35) // PUSH1 0; CALLDATALOAD
	a.raw(0x60, 0xe0, 0x1c) // PUSH1 0xe0; SHR
	arms := [...]struct {
		sel   []byte
		label string
	}{
		{ladderOpcodeProbeSelectors[0], "arm_push0"},
		{ladderOpcodeProbeSelectors[1], "arm_mcopy"},
		{ladderOpcodeProbeSelectors[2], "arm_blobhash"},
	}
	for _, arm := range arms {
		a.raw(0x80)             // DUP1
		a.pushSelector(arm.sel) // PUSH4 selector
		a.raw(0x14)             // EQ
		a.pushLabel(arm.label)  // PUSH2 dest
		a.raw(0x57)             // JUMPI
	}
	a.raw(0x00) // unknown selector (incl. empty calldata) -> STOP

	// arm_push0: PUSH0; POP; STOP.
	a.jumpLabel("arm_push0")
	a.raw(0x5f, 0x50, 0x00)

	// arm_mcopy: dst=32 <- src=0, len=32; STOP. Stack order (top first):
	// dstOffset, srcOffset, length -> push length, src, dst.
	a.jumpLabel("arm_mcopy")
	a.raw(0x60, 0x20) // PUSH1 32 (len)
	a.raw(0x60, 0x00) // PUSH1 0 (src)
	a.raw(0x60, 0x20) // PUSH1 32 (dst)
	a.raw(0x5e, 0x00) // MCOPY; STOP
	a.jumpLabel("arm_blobhash")
	a.raw(0x60, 0x00) // PUSH1 0 (index)
	a.raw(0x49)       // BLOBHASH
	a.raw(0x50, 0x00) // POP; STOP
	return a.assemble()
}

// createWithValueTx mirrors createTx but carries an explicit endowment (the
// same-tx create+SELFDESTRUCT probe transfers it to the beneficiary).
func createWithValueTx(key byte, nonce uint64, value *big.Int, gas uint64, data hexutil.Bytes) inputTx {
	k := privKey(key)
	return inputTx{
		OpType:               "eip1559",
		ChainID:              hd256(chainID),
		Nonce:                hd64(nonce),
		To:                   nil, // create marker
		Value:                hd256(value),
		Gas:                  hd64(gas),
		MaxFeePerGas:         hdu(2_000_000_000), // 2 gwei
		MaxPriorityFeePerGas: hdu(100_000_000),   // 0.1 gwei
		Data:                 data,
		SecretKey:            &k,
	}
}

// injectLadderForkSensitivityProbes appends the P2-C probes after the P2-B
// tx-type probes on each segment probe block. selfdestructSeq is the 0-based
// segment counter (which cross-tx SELFDESTRUCT probe this segment uses). The
// same-tx create+destruct's created address is declared a candidate for this
// block only (it never exists in the post-block trie -- emitPostState then
// emits the explicit all-zero row).
func injectLadderForkSensitivityProbes(add func(inputTx), nextNonce func() uint64,
	in *inputCase, selfdestructSeq int) {

	// 1) cross-tx SELFDESTRUCT: pre-Ecotone deletes the probe account,
	//    Ecotone+ only drains its 1 ETH to recA (fork-sensitive).
	add(transferTx(1, nextNonce(), ladderSelfdestructProbeAddr(selfdestructSeq),
		big.NewInt(0), ladderSelfdestructCallGas, nil))

	// 2) same-tx create + SELFDESTRUCT: deleted on EVERY fork.
	createNonce := nextNonce()
	destroyed := crypto.CreateAddress(addrOfKey(1), createNonce)
	add(createWithValueTx(1, createNonce, big.NewInt(ladderCreateDestructEndowment),
		ladderCreateDestructGas, ladderSelfdestructCode(recA)))
	in.ExtraCandidates = append(in.ExtraCandidates, destroyed)

	// 3) EIP-150 63/64 gas boundary: all-gas CALL into the INVALID predeploy.
	add(transferTx(1, nextNonce(), ladderGasBoundaryCallerAddr,
		big.NewInt(0), ladderGasBoundaryProbeGas, nil))

	// 4) fork-sensitive opcodes: arm status flips at Canyon (PUSH0) and at
	//    Ecotone (MCOPY, BLOBHASH).
	for _, sel := range ladderOpcodeProbeSelectors {
		add(transferTx(1, nextNonce(), ladderOpcodeProbeAddr,
			big.NewInt(0), ladderOpcodeProbeGas, sel))
	}
}
