// This code is adapted from the following repository:
// Repository: https://github.com/stackup-wallet/erc-4337-execution-clients
// Original file: tracers/bundler_collector_next.go.template

package native

import (
	"encoding/json"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/eth/tracers/internal"
	"github.com/holiman/uint256"
)

func init() {
	tracers.DefaultDirectory.Register("bundlerCollectorTracer", newBundlerCollector, false)
}

type partialStack = []*uint256.Int

type contractSizeVal struct {
	ContractSize int    `json:"contractSize"`
	Opcode       string `json:"opcode"`
}

type access struct {
	Reads  map[string]string `json:"reads"`
	Writes map[string]uint64 `json:"writes"`
}

type entryPointCall struct {
	TopLevelMethodSig     hexutil.Bytes                       `json:"topLevelMethodSig"`
	TopLevelTargetAddress common.Address                      `json:"topLevelTargetAddress"`
	Access                map[common.Address]*access          `json:"access"`
	Opcodes               map[string]uint64                   `json:"opcodes"`
	ExtCodeAccessInfo     map[common.Address]string           `json:"extCodeAccessInfo"`
	ContractSize          map[common.Address]*contractSizeVal `json:"contractSize"`
	OOG                   bool                                `json:"oog"`
}

type callsItem struct {
	// Common
	Type string `json:"type"`

	// Enter info
	From   common.Address `json:"from"`
	To     common.Address `json:"to"`
	Method hexutil.Bytes  `json:"method"`
	Value  *hexutil.Big   `json:"value"`
	Gas    uint64         `json:"gas"`

	// Exit info
	GasUsed uint64        `json:"gasUsed"`
	Data    hexutil.Bytes `json:"data"`
}

type logsItem struct {
	Data  hexutil.Bytes   `json:"data"`
	Topic []hexutil.Bytes `json:"topic"`
}

type lastThreeOpCodesItem struct {
	Opcode    string
	StackTop3 partialStack
}

type bundlerCollectorResults struct {
	CallsFromEntryPoint []*entryPointCall `json:"callsFromEntryPoint"`
	Keccak              []hexutil.Bytes   `json:"keccak"`
	Logs                []*logsItem       `json:"logs"`
	Calls               []*callsItem      `json:"calls"`
}

type bundlerCollector struct {
	vm *tracing.VMContext

	CallsFromEntryPoint []*entryPointCall
	CurrentLevel        *entryPointCall
	Keccak              []hexutil.Bytes
	Calls               []*callsItem
	Logs                []*logsItem
	lastOp              string
	lastThreeOpCodes    []*lastThreeOpCodesItem
	allowedOpcodeRegex  *regexp.Regexp
	stopCollectingTopic string
	stopCollecting      bool
}

func newBundlerCollector(ctx *tracers.Context, cfg json.RawMessage) (*tracers.Tracer, error) {
	rgx, err := regexp.Compile(
		`^(DUP\d+|PUSH\d+|SWAP\d+|POP|ADD|SUB|MUL|DIV|EQ|LTE?|S?GTE?|SLT|SH[LR]|AND|OR|NOT|ISZERO)$`,
	)
	if err != nil {
		return nil, err
	}
	// event sent after all validations are done: keccak("BeforeExecution()")
	stopCollectingTopic := "0xbb47ee3e183a558b1a2ff0874b079f3fc5478b7454eacf2bfc5af2ff5878f972"

	t := &bundlerCollector{
		CallsFromEntryPoint: []*entryPointCall{},
		CurrentLevel:        nil,
		Keccak:              []hexutil.Bytes{},
		Calls:               []*callsItem{},
		Logs:                []*logsItem{},
		lastOp:              "",
		lastThreeOpCodes:    []*lastThreeOpCodesItem{},
		allowedOpcodeRegex:  rgx,
		stopCollectingTopic: stopCollectingTopic,
		stopCollecting:      false,
	}

	return &tracers.Tracer{
		Hooks: &tracing.Hooks{
			OnTxStart: t.OnTxStart,
			OnEnter:   t.OnEnter,
			OnExit:    t.OnExit,
			OnOpcode:  t.OnOpcode,
		},
		GetResult: t.GetResult,
	}, nil
}

func (b *bundlerCollector) isEXTorCALL(opcode string) bool {
	return strings.HasPrefix(opcode, "EXT") ||
		opcode == "CALL" ||
		opcode == "CALLCODE" ||
		opcode == "DELEGATECALL" ||
		opcode == "STATICCALL"
}

// not using 'isPrecompiled' to only allow the ones defined by the ERC-4337 as stateless precompiles
// [OP-062]
func (b *bundlerCollector) isAllowedPrecompile(addr common.Address) bool {
	addrInt := addr.Big()
	return addrInt.Cmp(big.NewInt(0)) == 1 && addrInt.Cmp(big.NewInt(10)) == -1
}

func (b *bundlerCollector) incrementCount(m map[string]uint64, k string) {
	if _, ok := m[k]; !ok {
		m[k] = 0
	}
	m[k]++
}

func (b *bundlerCollector) OnTxStart(
	vm *tracing.VMContext,
	tx *types.Transaction,
	from common.Address,
) {
	b.vm = vm
}

func (b *bundlerCollector) GetResult() (json.RawMessage, error) {
	bcr := bundlerCollectorResults{
		CallsFromEntryPoint: b.CallsFromEntryPoint,
		Keccak:              b.Keccak,
		Logs:                b.Logs,
		Calls:               b.Calls,
	}

	r, err := json.Marshal(bcr)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (b *bundlerCollector) OnEnter(
	depth int,
	typ byte,
	from common.Address,
	to common.Address,
	input []byte,
	gas uint64,
	value *big.Int,
) {
	if b.stopCollecting {
		return
	}

	op := vm.OpCode(typ)
	method := []byte{}
	if len(input) >= 4 {
		method = append(method, input[:4]...)
	}
	b.Calls = append(b.Calls, &callsItem{
		Type:   op.String(),
		From:   from,
		To:     to,
		Method: method,
		Gas:    gas,
		Value:  (*hexutil.Big)(value),
	})
}

func (b *bundlerCollector) OnExit(depth int, output []byte, gasUsed uint64, err error, reverted bool) {
	if b.stopCollecting {
		return
	}

	rt := "RETURN"
	if err != nil {
		rt = "REVERT"
	}
	b.Calls = append(b.Calls, &callsItem{
		Type:    rt,
		GasUsed: gasUsed,
		Data:    output,
	})
}

func (b *bundlerCollector) OnOpcode(
	pc uint64,
	op byte,
	gas, cost uint64,
	scope tracing.OpContext,
	rData []byte,
	depth int,
	err error,
) {
	if b.stopCollecting {
		return
	}
	opcode := vm.OpCode(op).String()

	stackSize := len(scope.StackData())
	stackLast := stackSize - 1
	stackTop3 := partialStack{}
	for i := 0; i < 3 && i < stackSize; i++ {
		stackTop3 = append(stackTop3, scope.StackData()[stackLast-i].Clone())
	}
	b.lastThreeOpCodes = append(b.lastThreeOpCodes, &lastThreeOpCodesItem{
		Opcode:    opcode,
		StackTop3: stackTop3,
	})
	if len(b.lastThreeOpCodes) > 3 {
		b.lastThreeOpCodes = b.lastThreeOpCodes[1:]
	}

	if gas < cost || (opcode == "SSTORE" && gas < 2300) {
		b.CurrentLevel.OOG = true
	}

	if opcode == "REVERT" || opcode == "RETURN" {
		// exit() is not called on top-level return/revert, so we reconstruct it from opcode
		if depth == 1 {
			ofs := scope.StackData()[stackLast].ToBig().Int64()
			len := scope.StackData()[stackLast-1].ToBig().Int64()
			data := make([]byte, len)
			m, _ := internal.GetMemoryCopyPadded(scope.MemoryData(), ofs, len)
			copy(data, m)
			b.Calls = append(b.Calls, &callsItem{
				Type:    opcode,
				GasUsed: 0,
				Data:    data,
			})
		}
		// NOTE: flushing all history after RETURN
		b.lastThreeOpCodes = []*lastThreeOpCodesItem{}
	}

	if depth == 1 {
		if opcode == "CALL" || opcode == "STATICCALL" {
			addr := common.HexToAddress(scope.StackData()[stackLast-1].Hex())
			ofs := scope.StackData()[stackLast-3].ToBig().Int64()
			sig := make([]byte, 4)
			m, _ := internal.GetMemoryCopyPadded(scope.MemoryData(), ofs, ofs+4)
			copy(sig, m)

			b.CurrentLevel = &entryPointCall{
				TopLevelMethodSig:     sig,
				TopLevelTargetAddress: addr,
				Access:                map[common.Address]*access{},
				Opcodes:               map[string]uint64{},
				ExtCodeAccessInfo:     map[common.Address]string{},
				ContractSize:          map[common.Address]*contractSizeVal{},
				OOG:                   false,
			}
			b.CallsFromEntryPoint = append(b.CallsFromEntryPoint, b.CurrentLevel)
		} else if opcode == "LOG1" && scope.StackData()[stackLast-2].Hex() == b.stopCollectingTopic {
			b.stopCollecting = true
		}
		b.lastOp = ""
		return
	}

	var lastOpInfo *lastThreeOpCodesItem
	if len(b.lastThreeOpCodes) >= 2 {
		lastOpInfo = b.lastThreeOpCodes[len(b.lastThreeOpCodes)-2]
	}
	// store all addresses touched by EXTCODE* opcodes
	if lastOpInfo != nil && strings.HasPrefix(lastOpInfo.Opcode, "EXT") {
		addr := common.HexToAddress(lastOpInfo.StackTop3[0].Hex())
		ops := []string{}
		for _, item := range b.lastThreeOpCodes {
			ops = append(ops, item.Opcode)
		}
		last3OpcodeStr := strings.Join(ops, ",")

		// only store the last EXTCODE* opcode per address - could even be a boolean for our current use-case
		// [OP-051]
		if !strings.Contains(last3OpcodeStr, ",EXTCODESIZE,ISZERO") {
			b.CurrentLevel.ExtCodeAccessInfo[addr] = opcode
		}
	}

	// [OP-041]
	if b.isEXTorCALL(opcode) {
		n := 0
		if !strings.HasPrefix(opcode, "EXT") {
			n = 1
		}
		addr := common.BytesToAddress(scope.StackData()[stackLast-n].Bytes())

		if _, ok := b.CurrentLevel.ContractSize[addr]; !ok && !b.isAllowedPrecompile(addr) {
			b.CurrentLevel.ContractSize[addr] = &contractSizeVal{
				ContractSize: len(b.vm.StateDB.GetCode(addr)),
				Opcode:       opcode,
			}
		}
	}

	// [OP-012]
	if b.lastOp == "GAS" && !strings.Contains(opcode, "CALL") {
		b.incrementCount(b.CurrentLevel.Opcodes, "GAS")
	}
	// ignore "unimportant" opcodes
	if opcode != "GAS" && !b.allowedOpcodeRegex.MatchString(opcode) {
		b.incrementCount(b.CurrentLevel.Opcodes, opcode)
	}
	b.lastOp = opcode

	if opcode == "SLOAD" || opcode == "SSTORE" {
		slot := common.BytesToHash(scope.StackData()[stackLast].Bytes())
		slotHex := slot.Hex()
		addr := scope.Address()
		if _, ok := b.CurrentLevel.Access[addr]; !ok {
			b.CurrentLevel.Access[addr] = &access{
				Reads:  map[string]string{},
				Writes: map[string]uint64{},
			}
		}
		access := *b.CurrentLevel.Access[addr]

		if opcode == "SLOAD" {
			// read slot values before this UserOp was created
			// (so saving it if it was written before the first read)
			_, rOk := access.Reads[slotHex]
			_, wOk := access.Writes[slotHex]
			if !rOk && !wOk {
				access.Reads[slotHex] = string(b.vm.StateDB.GetState(addr, slot).Hex())
			}
		} else {
			b.incrementCount(access.Writes, slotHex)
		}
	}

	if opcode == "KECCAK256" {
		// collect keccak on 64-byte blocks
		ofs := scope.StackData()[stackLast].ToBig().Int64()
		len := scope.StackData()[stackLast-1].ToBig().Int64()
		// currently, solidity uses only 2-word (6-byte) for a key. this might change..still, no need to
		// return too much
		if len > 20 && len < 512 {
			data := make([]byte, len)
			m, _ := internal.GetMemoryCopyPadded(scope.MemoryData(), ofs, len)
			copy(data, m)
			b.Keccak = append(b.Keccak, data)
		}
	} else if strings.HasPrefix(opcode, "LOG") {
		count, _ := strconv.Atoi(opcode[3:])
		ofs := scope.StackData()[stackLast].ToBig().Int64()
		len := scope.StackData()[stackLast-1].ToBig().Int64()
		topics := []hexutil.Bytes{}
		for i := 0; i < count; i++ {
			topics = append(topics, scope.StackData()[stackLast-(2+i)].Bytes())
		}
		data := make([]byte, len)
		m, _ := internal.GetMemoryCopyPadded(scope.MemoryData(), ofs, len)
		copy(data, m)
		b.Logs = append(b.Logs, &logsItem{
			Data:  data,
			Topic: topics,
		})
	}
}
