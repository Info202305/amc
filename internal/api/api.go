// Copyright 2022 The AmazeChain Authors
// This file is part of the AmazeChain library.
//
// The AmazeChain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The AmazeChain library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the AmazeChain library. If not, see <http://www.gnu.org/licenses/>.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/amazechain/amc/accounts"
	"github.com/amazechain/amc/common"
	"github.com/amazechain/amc/common/block"
	"github.com/amazechain/amc/common/db"
	"github.com/amazechain/amc/common/transaction"
	"github.com/amazechain/amc/common/txs_pool"
	"github.com/amazechain/amc/common/types"
	"github.com/amazechain/amc/internal/api/filters"
	"github.com/amazechain/amc/internal/avm"
	"github.com/amazechain/amc/internal/avm/abi"
	mvm_common "github.com/amazechain/amc/internal/avm/common"
	"github.com/amazechain/amc/internal/avm/common/hexutil"
	"github.com/amazechain/amc/internal/avm/crypto"
	"github.com/amazechain/amc/internal/avm/params"
	mvm_types "github.com/amazechain/amc/internal/avm/types"
	"github.com/amazechain/amc/internal/avm/vm"
	"github.com/amazechain/amc/internal/consensus"
	"github.com/amazechain/amc/log"
	"github.com/amazechain/amc/modules/rawdb"
	"github.com/amazechain/amc/modules/rpc/jsonrpc"
	"github.com/amazechain/amc/modules/statedb"
	"github.com/libp2p/go-libp2p-core/peer"
)

const (
	baseFee       = 5000000
	rpcEVMTimeout = time.Duration(5 * time.Second)
	rpcGasCap     = 5000
)

// API compatible EthereumAPI provides an API to access related information.
type API struct {
	db         db.IDatabase
	pubsub     common.IPubSub
	p2pserver  common.INetwork
	peers      map[peer.ID]common.Peer
	bc         common.IBlockChain
	engine     consensus.Engine
	txspool    txs_pool.ITxsPool
	downloader common.IDownloader

	accountManager *accounts.Manager
}

// NewAPI creates a new protocol API.
func NewAPI(pubsub common.IPubSub, p2pserver common.INetwork, peers map[peer.ID]common.Peer, bc common.IBlockChain, db db.IDatabase, engine consensus.Engine, txspool txs_pool.ITxsPool, downloader common.IDownloader, accountManager *accounts.Manager) *API {
	return &API{
		db:             db,
		pubsub:         pubsub,
		p2pserver:      p2pserver,
		peers:          peers,
		bc:             bc,
		engine:         engine,
		txspool:        txspool,
		downloader:     downloader,
		accountManager: accountManager,
	}
}

func (api *API) Apis() []jsonrpc.API {
	nonceLock := new(AddrLocker)
	return []jsonrpc.API{
		{
			Namespace: "eth",
			Service:   NewBlockChainAPI(api),
		}, {
			Namespace: "eth",
			Service:   NewAmcAPI(api),
		}, {
			Namespace: "eth",
			Service:   NewTransactionAPI(api, nonceLock),
		}, {
			Namespace: "web3",
			Service:   &Web3API{api},
		}, {
			Namespace: "net",
			Service:   NewNetAPI(api, api.GetChainConfig().ChainID.Uint64()),
		}, {
			Namespace: "debug",
			Service:   NewDebugAPI(api),
		}, {
			Namespace: "txpool",
			Service:   NewTxsPoolAPI(api),
		}, {
			Namespace: "eth",
			Service:   filters.NewFilterAPI(api, 5*time.Minute),
		},
	}
}

func (n *API) TxsPool() txs_pool.ITxsPool     { return n.txspool }
func (n *API) Downloader() common.IDownloader { return n.downloader }
func (n *API) P2pServer() common.INetwork     { return n.p2pserver }
func (n *API) Peers() map[peer.ID]common.Peer { return n.peers }
func (n *API) Database() db.IDatabase         { return n.db }
func (n *API) Engine() consensus.Engine       { return n.engine }
func (n *API) BlockChain() common.IBlockChain { return n.bc }
func (n *API) GetEvm(ctx context.Context, msg mvm_types.Message, state common.IStateDB, header block.IHeader, vmConfig *vm.Config) (*vm.EVM, func() error, error) {
	vmError := func() error { return nil }

	txContext := avm.NewEVMTxContext(msg)
	context := avm.NewBlockContext(header, n.BlockChain(), nil)

	return vm.NewEVM(context, txContext, avm.NewDBStates(state), n.GetChainConfig(), *vmConfig), vmError, nil
}

func (n *API) State(blockNrOrHash jsonrpc.BlockNumberOrHash) common.IStateDB {
	// todo if header not found
	var blockHash types.Hash

	if blockNr, ok := blockNrOrHash.Number(); ok {
		//todo
		var header block.IHeader
		var err error
		if blockNr < jsonrpc.EarliestBlockNumber {
			header = n.BlockChain().CurrentBlock().Header()
		} else {
			header, err = n.BlockChain().GetHeaderByNumber(types.NewInt64(uint64(blockNr.Int64())))
			if err != nil {
				return nil
			}
		}

		blockHash = header.Hash()
	} else if hash, ok := blockNrOrHash.Hash(); ok {
		blockHash = mvm_types.ToAmcHash(hash)
	} else {
		blockHash = n.BlockChain().CurrentBlock().Header().Hash()
	}

	return n.BlockChain().StateAt(blockHash)
}

func (n *API) GetChainConfig() *params.ChainConfig {
	return params.AmazeChainConfig
}

// AmcAPI provides an API to access meta related information.
type AmcAPI struct {
	api *API
}

// NewAmcAPI creates a new Meta protocol API.
func NewAmcAPI(api *API) *AmcAPI {
	return &AmcAPI{api}
}

// GasPrice returns a suggestion for a gas price for legacy transactions.
func (s *AmcAPI) GasPrice(ctx context.Context) (*hexutil.Big, error) {
	LightClientGPO.Default = big.NewInt(params.GWei)
	oracle := NewOracle(s.api.BlockChain(), LightClientGPO)
	tipcap, err := oracle.SuggestTipCap(ctx, s.api.GetChainConfig())
	if err != nil {
		return nil, err
	}
	if head := s.api.BlockChain().CurrentBlock().Header(); head.BaseFee64() != types.NewInt64(0) {
		tipcap.Add(tipcap, head.BaseFee64().ToBig())
	}
	return (*hexutil.Big)(tipcap), nil
	//todo hardcode 13Gwei
	//tipcap := 13000000000
	//return (*hexutil.Big)(new(big.Int).SetUint64(uint64(tipcap))), nil
}

type feeHistoryResult struct {
	OldestBlock  *hexutil.Big     `json:"oldestBlock"`
	Reward       [][]*hexutil.Big `json:"reward,omitempty"`
	BaseFee      []*hexutil.Big   `json:"baseFeePerGas,omitempty"`
	GasUsedRatio []float64        `json:"gasUsedRatio"`
}

// TxPoolAPI offers and API for the transaction pool. It only operates on data that is non confidential.
type TxPoolAPI struct {
	api *API
}

// NewTxPoolAPI creates a new tx pool service that gives information about the transaction pool.
func NewTxPoolAPI(api *API) *TxPoolAPI {
	return &TxPoolAPI{api}
}

// Content returns the transactions contained within the transaction pool.
func (s *TxPoolAPI) Content() map[string]map[string]map[string]*RPCTransaction {
	content := map[string]map[string]map[string]*RPCTransaction{
		"pending": make(map[string]map[string]*RPCTransaction),
		"queued":  make(map[string]map[string]*RPCTransaction),
	}
	return content
}

// ContentFrom returns the transactions contained within the transaction pool.
func (s *TxPoolAPI) ContentFrom(addr types.Address) map[string]map[string]*RPCTransaction {
	content := make(map[string]map[string]*RPCTransaction, 2)

	return content
}

// Status returns the number of pending and queued transaction in the pool.
func (s *TxPoolAPI) Status() map[string]hexutil.Uint {
	_, pending, _, queue := s.api.TxsPool().Stats()
	return map[string]hexutil.Uint{
		"pending": hexutil.Uint(pending),
		"queued":  hexutil.Uint(queue),
	}
}

// Inspect retrieves the content of the transaction pool and flattens it into an
// easily inspectable list.
func (s *TxPoolAPI) Inspect() map[string]map[string]map[string]string {
	content := map[string]map[string]map[string]string{
		"pending": make(map[string]map[string]string),
		"queued":  make(map[string]map[string]string),
	}

	return content
}

// AccountAPI provides an API to access accounts managed by this node.
// It offers only methods that can retrieve accounts.
type AccountAPI struct {
}

// NewAccountAPI creates a new AccountAPI.
func NewAccountAPI() *AccountAPI {
	return &AccountAPI{}
}

// Accounts returns the collection of accounts this node manages.
func (s *AccountAPI) Accounts() []types.Address {
	//return s.am.Accounts()
	return nil
}

// BlockChainAPI provides an API to access Ethereum blockchain data.
type BlockChainAPI struct {
	api *API
}

// NewBlockChainAPI creates a new  blockchain API.
func NewBlockChainAPI(api *API) *BlockChainAPI {
	return &BlockChainAPI{api}
}

// ChainId get Chain ID
func (api *BlockChainAPI) ChainId() *hexutil.Big {
	return (*hexutil.Big)(api.api.GetChainConfig().ChainID)
}

// GetBalance get balance
func (s *BlockChainAPI) GetBalance(ctx context.Context, address mvm_common.Address, blockNrOrHash jsonrpc.BlockNumberOrHash) (*hexutil.Big, error) {
	state := s.api.State(blockNrOrHash)
	if state == nil {
		return nil, nil
	}
	balance := state.GetBalance(*mvm_types.ToAmcAddress(&address))
	return (*hexutil.Big)(balance.ToBig()), nil
}

func (s *BlockChainAPI) BlockNumber() hexutil.Uint64 {
	//jsonrpc.LatestBlockNumber
	header := s.api.BlockChain().CurrentBlock().Header() // latest header should always be available
	return hexutil.Uint64(header.Number64().Uint64())
}

// GetCode get code
func (s *BlockChainAPI) GetCode(ctx context.Context, address mvm_common.Address, blockNrOrHash jsonrpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	state := s.api.State(blockNrOrHash)
	if state == nil {
		return nil, nil
	}
	code := state.GetCode(*mvm_types.ToAmcAddress(&address))
	return code, nil
}

// GetStorageAt returns the storage from the state at the given address, key and
// block number. The rpc.LatestBlockNumber and rpc.PendingBlockNumber meta block
// numbers are also allowed.
func (s *BlockChainAPI) GetStorageAt(ctx context.Context, address mvm_common.Address, key string, blockNrOrHash jsonrpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	state := s.api.State(blockNrOrHash)
	if state == nil {
		return nil, nil
	}
	res := state.GetState(*mvm_types.ToAmcAddress(&address), mvm_types.ToAmcHash(mvm_common.HexToHash(key)))
	hash := mvm_types.FromAmcHash(res)
	return hash[:], state.Error()
}

// GetUncleCountByBlockHash returns number of uncles in the block for the given block hash
func (s *BlockChainAPI) GetUncleCountByBlockHash(ctx context.Context, blockHash mvm_common.Hash) *hexutil.Uint {
	if block, _ := s.api.BlockChain().GetBlockByHash(mvm_types.ToAmcHash(blockHash)); block != nil {
		//POA donot have Uncles
		n := hexutil.Uint(0)
		return &n
	}
	return nil
}

// GetUncleByBlockHashAndIndex returns the uncle block for the given block hash and index.
func (s *BlockChainAPI) GetUncleByBlockHashAndIndex(ctx context.Context, blockHash mvm_common.Hash, index hexutil.Uint) (map[string]interface{}, error) {
	b, err := s.api.BlockChain().GetBlockByHash(mvm_types.ToAmcHash(blockHash))
	if b != nil {
		//POA donot have Uncles
		var uncles []struct{}
		if index >= hexutil.Uint(len(uncles)) {
			return nil, nil
		}
		block := block.NewBlock(&block.Header{}, nil)
		return RPCMarshalBlock(block, false, false, s.api.Engine())
	}
	return nil, err
}

// Result structs for GetProof
type AccountResult struct {
	Address      types.Address   `json:"address"`
	AccountProof []string        `json:"accountProof"`
	Balance      *hexutil.Big    `json:"balance"`
	CodeHash     types.Hash      `json:"codeHash"`
	Nonce        hexutil.Uint64  `json:"nonce"`
	StorageHash  types.Hash      `json:"storageHash"`
	StorageProof []StorageResult `json:"storageProof"`
}

type StorageResult struct {
	Key   string       `json:"key"`
	Value *hexutil.Big `json:"value"`
	Proof []string     `json:"proof"`
}

// // OverrideAccount indicates the overriding fields of account during the execution
// // of a message call.
// // Note, state and stateDiff can't be specified at the same time. If state is
// // set, message execution will only use the data in the given state. Otherwise
// // if statDiff is set, all diff will be applied first and then execute the call
// // message.
type OverrideAccount struct {
	Nonce      *hexutil.Uint64                      `json:"nonce"`
	Code       *hexutil.Bytes                       `json:"code"`
	Balance    **hexutil.Big                        `json:"balance"`
	StatsPrint *map[mvm_common.Hash]mvm_common.Hash `json:"state"`
	StateDiff  *map[mvm_common.Hash]mvm_common.Hash `json:"stateDiff"`
}

// StateOverride is the collection of overridden accounts.
type StateOverride map[mvm_common.Address]OverrideAccount

// Apply overrides the fields of specified accounts into the given state.
func (diff *StateOverride) Apply(state *statedb.StateDB) error {
	if diff == nil {
		return nil
	}
	for addr, account := range *diff {
		// Override account nonce.
		if account.Nonce != nil {
			state.SetNonce(*mvm_types.ToAmcAddress(&addr), uint64(*account.Nonce))
		}
		// Override account(contract) code.
		if account.Code != nil {
			state.SetCode(*mvm_types.ToAmcAddress(&addr), *account.Code)
		}
		// Override account balance.
		if account.Balance != nil {
			balance, _ := types.FromBig((*big.Int)(*account.Balance))
			state.SetBalance(*mvm_types.ToAmcAddress(&addr), balance)
		}
		if account.StatsPrint != nil && account.StateDiff != nil {
			return fmt.Errorf("account %s has both 'state' and 'stateDiff'", addr.String())
		}
		if account.StatsPrint != nil {
			statesPrint := make(map[types.Hash]types.Hash)
			for k, v := range *account.StatsPrint {
				statesPrint[mvm_types.ToAmcHash(k)] = mvm_types.ToAmcHash(v)
			}
			state.SetStorage(*mvm_types.ToAmcAddress(&addr), statesPrint)
		}
		// Apply state diff into specified accounts.
		if account.StateDiff != nil {
			for key, value := range *account.StateDiff {
				state.SetState(*mvm_types.ToAmcAddress(&addr), mvm_types.ToAmcHash(key), mvm_types.ToAmcHash(value))
			}
		}
	}
	return nil
}

func DoCall(ctx context.Context, api *API, args TransactionArgs, blockNrOrHash jsonrpc.BlockNumberOrHash, overrides *StateOverride, timeout time.Duration, globalGasCap uint64) (*avm.ExecutionResult, error) {
	defer func(start time.Time) { log.Debug("Executing EVM call finished", "runtime", time.Since(start)) }(time.Now())

	var header block.IHeader
	var err error
	if blockNr, ok := blockNrOrHash.Number(); ok {
		if blockNr < jsonrpc.EarliestBlockNumber {
			header = api.BlockChain().CurrentBlock().Header()
		} else {
			header, err = api.BlockChain().GetHeaderByNumber(types.NewInt64(uint64(blockNr.Int64())))
		}
	}
	if hash, ok := blockNrOrHash.Hash(); ok {
		header, err = api.BlockChain().GetHeaderByHash(mvm_types.ToAmcHash(hash))
	}
	if err != nil {
		return nil, err
	}
	state := api.State(blockNrOrHash).(*statedb.StateDB)

	if err := overrides.Apply(state); err != nil {
		return nil, err
	}
	// Setup context so it may be cancelled the call has completed
	// or, in case of unmetered gas, setup a context with a timeout.
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	// Make sure the context is cancelled when the call has completed
	// this makes sure resources are cleaned up.
	defer cancel()

	// Get a new instance of the EVM.
	msg, err := args.ToMessage(api)
	if err != nil {
		return nil, err
	}

	//todo debug: , Debug: true, Tracer: vm.NewMarkdownLogger(os.Stdout)
	evm, vmError, err := api.GetEvm(ctx, msg, state, header, &vm.Config{NoBaseFee: true})
	if err != nil {
		return nil, err
	}
	// Wait for the context to be done and cancel the evm. Even if the
	// EVM has finished, cancelling may be done (repeatedly)
	go func() {
		<-ctx.Done()
		evm.Cancel()
	}()

	// Execute the message.
	gp := new(common.GasPool).AddGas(math.MaxUint64)
	result, err := avm.ApplyMessage(evm, msg, gp)
	if err := vmError(); err != nil {
		return nil, err
	}

	if evm.Cancelled() {
		return nil, fmt.Errorf("execution aborted (timeout = %v)", timeout)
	}
	if err != nil {
		return result, fmt.Errorf("err: %w (supplied gas %d)", err, msg.Gas())
	}
	return result, nil
}

func newRevertError(result *avm.ExecutionResult) *revertError {
	reason, errUnpack := abi.UnpackRevert(result.Revert())
	err := fmt.Errorf("execution reverted")
	if errUnpack == nil {
		err = fmt.Errorf("execution reverted: %v", reason)
	}
	return &revertError{
		error:  err,
		reason: hexutil.Encode(result.Revert()),
	}
}

// revertError is an API error that encompassas an EVM revertal with JSON error
// code and a binary data blob.
type revertError struct {
	error
	reason string // revert reason hex encoded
}

// ErrorCode returns the JSON error code for a revertal.
// See: https://github.com/ethereum/wiki/wiki/JSON-RPC-Error-Codes-Improvement-Proposal
func (e *revertError) ErrorCode() int {
	return 3
}

// ErrorData returns the hex encoded revert reason.
func (e *revertError) ErrorData() interface{} {
	return e.reason
}

// Call executes the given transaction on the state for the given block number.
//
// Additionally, the caller can specify a batch of contract for fields overriding.
//
// Note, this function doesn't make and changes in the state/blockchain and is
// useful to execute and retrieve values.
func (s *BlockChainAPI) Call(ctx context.Context, args TransactionArgs, blockNrOrHash jsonrpc.BlockNumberOrHash, overrides *StateOverride) (hexutil.Bytes, error) {

	//b, _ := json.Marshal(args)
	//log.Info("TransactionArgs %s", string(b))

	result, err := DoCall(ctx, s.api, args, blockNrOrHash, overrides, rpcEVMTimeout, rpcGasCap)
	if err != nil {
		return nil, err
	}
	// If the result contains a revert reason, try to unpack and return it.
	if len(result.Revert()) > 0 {
		return nil, newRevertError(result)
	}
	return result.Return(), result.Err
}

func BlockByNumber(ctx context.Context, number jsonrpc.BlockNumber, n *API) (block.IBlock, error) {
	// todo
	// Pending block is only known by the miner
	if number == jsonrpc.PendingBlockNumber {
		iblock := n.BlockChain().CurrentBlock()
		return iblock, nil
	}
	// Otherwise resolve and return the block
	if number == jsonrpc.LatestBlockNumber {
		iblock := n.BlockChain().CurrentBlock()
		return iblock, nil
	}
	iblock, err := n.BlockChain().GetBlockByNumber(types.NewInt64(uint64(number)))
	if err != nil {
		return nil, err
	}
	return iblock, nil
}

func BlockByNumberOrHash(ctx context.Context, blockNrOrHash jsonrpc.BlockNumberOrHash, api *API) (block.IBlock, error) {
	if blockNr, ok := blockNrOrHash.Number(); ok {
		if blockNr == jsonrpc.PendingBlockNumber {
			return api.BlockChain().CurrentBlock(), nil
		}
		return BlockByNumber(ctx, blockNr, api)
	}
	if hash, ok := blockNrOrHash.Hash(); ok {
		iblock, err := api.BlockChain().GetBlockByHash(types.Hash(hash))
		if err != nil {
			return nil, err
		}
		if iblock == nil {
			return nil, errors.New("header found, but block body is missing")
		}

		//todo
		//header := iblock.Header()
		//if header == nil {
		//	return nil, errors.New("header for hash not found")
		//}
		//iblock, err = n.BlockChain().GetBlockByNumber(header.Number64())
		//if err != nil {
		//	return nil, err
		//}
		//if blockNrOrHash.RequireCanonical && iblock.Hash() != types.Hash(hash) {
		//	return nil, errors.New("hash is not currently canonical")
		//}
		//iblock, err = n.BlockChain().GetBlockByNumber(n.BlockChain().GetHeader(types.Hash(hash), header.Number64()).Number64())
		//if err != nil {
		//	return nil, err
		//}
		return iblock, nil
	}
	return nil, errors.New("invalid arguments; neither block nor hash specified")
}

func StateAndHeaderByNumber(ctx context.Context, n *API, number jsonrpc.BlockNumber) (*common.IStateDB, *block.IHeader, error) {
	var header block.IHeader
	var err error
	if number == jsonrpc.PendingBlockNumber {
		header = n.BlockChain().CurrentBlock().Header()
	} else {
		header, err = n.BlockChain().GetHeaderByNumber(types.NewInt64(uint64(number)))
	}
	if err != nil {
		return nil, nil, err
	}
	if header == nil {
		return nil, nil, errors.New("header not found")
	}
	stateDb := n.BlockChain().StateAt(header.Hash())
	return &stateDb, &header, nil
}

func StateAndHeaderByNumberOrHash(ctx context.Context, n *API, blockNrOrHash jsonrpc.BlockNumberOrHash) (*common.IStateDB, *block.IHeader, error) {
	if blockNr, ok := blockNrOrHash.Number(); ok {
		return StateAndHeaderByNumber(ctx, n, blockNr)
	}
	if hash, ok := blockNrOrHash.Hash(); ok {
		header, err := n.BlockChain().GetHeaderByHash(types.Hash(hash))
		if err != nil {
			return nil, nil, err
		}
		if header == nil {
			return nil, nil, errors.New("header for hash not found")
		}
		//todo
		//if blockNrOrHash.RequireCanonical && b.eth.blockchain.GetCanonicalHash(header.Number.Uint64()) != hash {
		//	return nil, nil, errors.New("hash is not currently canonical")
		//}
		stateDb := n.BlockChain().StateAt(header.Hash())
		return &stateDb, &header, err
	}
	return nil, nil, errors.New("invalid arguments; neither block nor hash specified")
}

func DoEstimateGas(ctx context.Context, n *API, args TransactionArgs, blockNrOrHash jsonrpc.BlockNumberOrHash, gasCap uint64) (hexutil.Uint64, error) {
	// Binary search the gas requirement, as it may be higher than the amount used
	var (
		lo  = params.TxGas - 1
		hi  uint64
		cap uint64
	)
	// Use zero address if sender unspecified.
	if args.From == nil {
		args.From = new(mvm_common.Address)
	}
	// Determine the highest gas limit can be used during the estimation.
	if args.Gas != nil && uint64(*args.Gas) >= params.TxGas {
		hi = uint64(*args.Gas)
	} else {
		// Retrieve the block to act as the gas ceiling
		iblock, err := BlockByNumberOrHash(ctx, blockNrOrHash, n)
		if err != nil {
			return 0, err
		}
		if iblock == nil {
			return 0, errors.New("block not found")
		}
		hi = iblock.GasLimit()
	}

	var feeCap *big.Int
	if args.GasPrice != nil && (args.MaxFeePerGas != nil || args.MaxPriorityFeePerGas != nil) {
		return 0, errors.New("both gasPrice and (maxFeePerGas or maxPriorityFeePerGas) specified")
	} else if args.GasPrice != nil {
		feeCap = args.GasPrice.ToInt()
	} else if args.MaxFeePerGas != nil {
		feeCap = args.MaxFeePerGas.ToInt()
	} else {
		feeCap = common.Big0
	}
	// Recap the highest gas limit with account's available balance.
	if feeCap.BitLen() != 0 {
		statedb := n.State(blockNrOrHash)
		if statedb == nil {
			return 0, errors.New("cannot load stateDB")
		}
		balance := statedb.GetBalance(*mvm_types.ToAmcAddress(args.From)) // from can't be nil
		available := new(big.Int).Set(balance.ToBig())
		if args.Value != nil {
			if args.Value.ToInt().Cmp(available) >= 0 {
				return 0, errors.New("insufficient funds for transfer")
			}
			available.Sub(available, args.Value.ToInt())
		}
		allowance := new(big.Int).Div(available, feeCap)

		// If the allowance is larger than maximum uint64, skip checking
		if allowance.IsUint64() && hi > allowance.Uint64() {
			transfer := args.Value
			if transfer == nil {
				transfer = new(hexutil.Big)
			}
			log.Warn("Gas estimation capped by limited funds", "original", hi, "balance", balance,
				"sent", transfer.ToInt(), "maxFeePerGas", feeCap, "fundable", allowance)
			hi = allowance.Uint64()
		}
	}
	// Recap the highest gas allowance with specified gascap.
	if gasCap != 0 && hi > gasCap {
		log.Warn("Caller gas above allowance, capping", "requested", hi, "cap", gasCap)
		hi = gasCap
	}
	cap = hi

	// Create a helper to check if a gas allowance results in an executable transaction
	executable := func(gas uint64) (bool, *avm.ExecutionResult, error) {
		args.Gas = (*hexutil.Uint64)(&gas)
		result, err := DoCall(ctx, n, args, blockNrOrHash, nil, 0, gasCap)
		if err != nil {
			if errors.Is(err, avm.ErrIntrinsicGas) {
				return true, nil, nil // Special case, raise gas limit
			}
			return true, nil, err // Bail out
		}
		return result.Failed(), result, nil
	}
	// Execute the binary search and hone in on an executable gas limit
	for lo+1 < hi {
		mid := (hi + lo) / 2
		failed, _, err := executable(mid)

		// If the error is not nil(consensus error), it means the provided message
		// call or transaction will never be accepted no matter how much gas it is
		// assigened. Return the error directly, don't struggle any more.
		if err != nil {
			return 0, err
		}
		if failed {
			lo = mid
		} else {
			hi = mid
		}
	}
	// Reject the transaction as invalid if it still fails at the highest allowance
	if hi == cap {
		failed, result, err := executable(hi)
		if err != nil {
			return 0, err
		}
		if failed {
			if result != nil && !errors.Is(result.Err, vm.ErrOutOfGas) {
				if len(result.Revert()) > 0 {
					return 0, newRevertError(result)
				}
				return 0, result.Err
			}
			// Otherwise, the specified gas cap is too low
			return 0, fmt.Errorf("gas required exceeds allowance (%d)", cap)
		}
	}
	return hexutil.Uint64(hi), nil
	//return hexutil.Uint64(baseFee), nil
}

// EstimateGas returns an estimate of the amount of gas needed to execute the
// given transaction against the current pending block.
func (s *BlockChainAPI) EstimateGas(ctx context.Context, args TransactionArgs, blockNrOrHash *jsonrpc.BlockNumberOrHash) (hexutil.Uint64, error) {
	bNrOrHash := jsonrpc.BlockNumberOrHashWithNumber(jsonrpc.PendingBlockNumber)
	if blockNrOrHash != nil {
		bNrOrHash = *blockNrOrHash
	}
	return DoEstimateGas(ctx, s.api, args, bNrOrHash, 50000000)
}

// GetBlockByNumber returns the requested canonical block.
//   - When blockNr is -1 the chain head is returned.
//   - When blockNr is -2 the pending chain head is returned.
//   - When fullTx is true all transactions in the block are returned, otherwise
//     only the transaction hash is returned.
func (s *BlockChainAPI) GetBlockByNumber(ctx context.Context, number jsonrpc.BlockNumber, fullTx bool) (map[string]interface{}, error) {

	var (
		block block.IBlock
		err   error
	)
	// header
	if number == jsonrpc.LatestBlockNumber {
		block = s.api.BlockChain().CurrentBlock()
		err = nil
	} else {
		block, err = s.api.BlockChain().GetBlockByNumber(types.NewInt64(uint64(number.Int64())))
	}

	if block != nil && err == nil {
		response, err := RPCMarshalBlock(block, true, fullTx, s.api.Engine())
		if err == nil && number == jsonrpc.PendingBlockNumber {
			// Pending blocks need to nil out a few fields
			for _, field := range []string{"hash", "nonce", "miner"} {
				response[field] = nil
			}
		}
		return response, err
	}

	return nil, err
}

// GetBlockByHash get block by hash
func (s *BlockChainAPI) GetBlockByHash(ctx context.Context, hash mvm_common.Hash, fullTx bool) (map[string]interface{}, error) {
	block, err := s.api.BlockChain().GetBlockByHash(mvm_types.ToAmcHash(hash))
	if block != nil {
		return RPCMarshalBlock(block, true, fullTx, s.api.Engine())
	}
	return nil, err
}

// TransactionAPI exposes methods for reading and creating transaction data.
type TransactionAPI struct {
	api       *API
	nonceLock *AddrLocker
	//signer    types.Signer
}

// NewTransactionAPI creates a new RPC service with methods for interacting with transactions.
func NewTransactionAPI(api *API, nonceLock *AddrLocker) *TransactionAPI {
	//signer := types.LatestSigner(b.ChainConfig())
	return &TransactionAPI{api, nonceLock}
}

// GetTransactionCount returns the number of transactions the given address has sent for the given block number
func (s *TransactionAPI) GetTransactionCount(ctx context.Context, address mvm_common.Address, blockNrOrHash jsonrpc.BlockNumberOrHash) (*hexutil.Uint64, error) {

	if blockNr, ok := blockNrOrHash.Number(); ok && blockNr == jsonrpc.PendingBlockNumber {
		nonce := s.api.TxsPool().Nonce(*mvm_types.ToAmcAddress(&address))
		return (*hexutil.Uint64)(&nonce), nil
	}

	state := s.api.State(blockNrOrHash)
	if state == nil {
		return nil, nil
	}
	nonce := state.GetNonce(*mvm_types.ToAmcAddress(&address))
	return (*hexutil.Uint64)(&nonce), nil

}

func (s *TransactionAPI) SendRawTransaction(ctx context.Context, input hexutil.Bytes) (mvm_common.Hash, error) {

	//log.Debugf("tx type is : %s", string(input[0]))
	tx := new(mvm_types.Transaction)
	err := tx.UnmarshalBinary(input)
	if err != nil {
		return mvm_common.Hash{}, err
	}
	header := s.api.BlockChain().CurrentBlock().Header() // latest header should always be available
	metaTx, err := tx.ToAmcTransaction(s.api.GetChainConfig(), header.Number64().ToBig())
	if err != nil {
		return mvm_common.Hash{}, err
	}

	return SubmitTransaction(context.Background(), s.api, metaTx)
}

// GetTransactionReceipt returns the transaction receipt for the given transaction hash.
func (s *TransactionAPI) GetTransactionReceipt(ctx context.Context, hash mvm_common.Hash) (map[string]interface{}, error) {

	tx, blockHash, blockNumber, index, err := rawdb.GetTransaction(s.api.Database(), mvm_types.ToAmcHash(hash))
	if err != nil || tx == nil {
		// When the transaction doesn't exist, the RPC method should return JSON null
		// as per specification.
		return nil, nil
	}
	//log.Infof("GetTransactionReceipt, hash %+v , %+v, %+v, %+v", tx, blockHash, blockNumber, index)
	receipts, err := s.api.BlockChain().GetReceipts(blockHash)

	//log.Infof("GetTransactionReceipt, receipts %+v", receipts)
	if err != nil {
		return nil, err
	}
	if len(receipts) <= int(index) {
		return nil, nil
	}
	receipt := receipts[index]

	from, _ := tx.From()
	fields := map[string]interface{}{
		"blockHash":         mvm_types.FromAmcHash(blockHash),
		"blockNumber":       hexutil.Uint64(blockNumber.Uint64()),
		"transactionHash":   hash,
		"transactionIndex":  hexutil.Uint64(index),
		"from":              mvm_types.FromAmcAddress(&from),
		"to":                mvm_types.FromAmcAddress(tx.To()),
		"gasUsed":           hexutil.Uint64(receipt.GasUsed),
		"cumulativeGasUsed": hexutil.Uint64(receipt.CumulativeGasUsed),
		"contractAddress":   nil,
		"logsBloom":         []byte{}, //receipt.Bloom
		"type":              hexutil.Uint(tx.Type()),
	}
	// Assign the effective gas price paid
	//todo !IsLondon
	if false {
		fields["effectiveGasPrice"] = hexutil.Uint64(tx.GasPrice().Uint64())
	} else {
		header, err := s.api.BlockChain().GetHeaderByHash(blockHash)
		if err != nil {
			return nil, err
		}
		gasPrice := new(big.Int).Add(header.BaseFee64().ToBig(), tx.EffectiveGasTipValue(header.BaseFee64()).ToBig())
		fields["effectiveGasPrice"] = hexutil.Uint64(gasPrice.Uint64())
	}
	// Assign receipt status or post state.
	if len(receipt.PostState) > 0 {
		fields["root"] = hexutil.Bytes(receipt.PostState)
	} else {
		fields["status"] = hexutil.Uint(receipt.Status)
	}
	if receipt.Logs == nil {
		fields["logs"] = []*mvm_types.Log{}
	} else {
		fields["logs"] = mvm_types.FromAmcLogs(receipt.Logs)
	}
	// If the ContractAddress is 20 0x0 bytes, assume it is not a contract creation
	if !receipt.ContractAddress.IsNull() {
		fields["contractAddress"] = mvm_types.FromAmcAddress(&receipt.ContractAddress)
	}

	json, _ := json.Marshal(fields)
	log.Infof("GetTransactionReceipt, result %s", string(json))
	return fields, nil
}

// GetBlockTransactionCountByHash returns the number of transactions in the block with the given hash.
func (s *TransactionAPI) GetBlockTransactionCountByHash(ctx context.Context, blockHash mvm_common.Hash) *hexutil.Uint {
	if block, _ := s.api.BlockChain().GetBlockByHash(mvm_types.ToAmcHash(blockHash)); block != nil {
		n := hexutil.Uint(len(block.Transactions()))
		return &n
	}
	return nil
}

// GetTransactionByHash returns the transaction for the given hash
func (s *TransactionAPI) GetTransactionByHash(ctx context.Context, hash mvm_common.Hash) (*RPCTransaction, error) {

	tx, blockHash, blockNumber, index, err := rawdb.GetTransaction(s.api.Database(), mvm_types.ToAmcHash(hash))
	if err != nil {
		// When the transaction doesn't exist, the RPC method should return JSON null
		// as per specification.
		return nil, err
	}

	if tx != nil {
		header, err := s.api.BlockChain().GetHeaderByNumber(blockNumber)
		if err != nil {
			return nil, err
		}
		return newRPCTransaction(tx, blockHash, blockNumber.Uint64(), index, header.BaseFee64().ToBig()), nil
	}

	if tx := s.api.TxsPool().GetTx(mvm_types.ToAmcHash(hash)); tx != nil {
		return newRPCPendingTransaction(tx, s.api.BlockChain().CurrentBlock().Header()), nil
	}

	return nil, nil
}

// GetTransactionByBlockHashAndIndex returns the transaction for the given block hash and index.
func (s *TransactionAPI) GetTransactionByBlockHashAndIndex(ctx context.Context, blockHash mvm_common.Hash, index hexutil.Uint) *RPCTransaction {
	if block, _ := s.api.BlockChain().GetBlockByHash(mvm_types.ToAmcHash(blockHash)); block != nil {
		for i, tx := range block.Transactions() {
			if i == int(index) {
				return newRPCTransaction(tx, mvm_types.ToAmcHash(blockHash), block.Number64().Uint64(), uint64(index), block.Header().BaseFee64().ToBig())
			}
		}
	}
	return nil
}

// SubmitTransaction ?
func SubmitTransaction(ctx context.Context, api *API, tx *transaction.Transaction) (mvm_common.Hash, error) {

	if err := checkTxFee(tx.GasPrice(), tx.Gas(), baseFee); err != nil {
		return mvm_common.Hash{}, err
	}

	if err := api.TxsPool().AddLocal(tx); err != nil {
		return mvm_common.Hash{}, err
	}

	if tx.To() == nil {
		//log.Info("Submitted contract creation", "hash", tx.Hash().Hex(), "from", from, "nonce", tx.Nonce(), "contract", addr.Hex(), "value", tx.Value())
	} else {
		//log.Info("Submitted transaction", "hash", tx.Hash().Hex(), "from", from, "nonce", tx.Nonce(), "recipient", tx.To(), "value", tx.Value())
	}
	hash, err := tx.Hash()
	return mvm_types.FromAmcHash(hash), err
}

// SendTransaction Send Transaction
func (s *TransactionAPI) SendTransaction(ctx context.Context, args TransactionArgs) (mvm_common.Hash, error) {
	// Look up the wallet containing the requested signer
	account := accounts.Account{Address: args.from()}

	//wallet, err := s.b.AccountManager().Find(account)
	wallet, err := s.api.accountManager.Find(account)
	if err != nil {
		return mvm_common.Hash{}, err
	}

	if args.Nonce == nil {
		s.nonceLock.LockAddr(args.from())
		defer s.nonceLock.UnlockAddr(args.from())
	}

	if err := args.setDefaults(ctx, s.api); err != nil {
		return mvm_common.Hash{}, err
	}
	header := s.api.BlockChain().CurrentBlock().Header()
	tx := args.toTransaction(baseFee, header.BaseFee64().ToBig())

	signed, err := wallet.SignTx(account, tx, s.api.GetChainConfig().ChainID)
	if err != nil {
		return mvm_common.Hash{}, err
	}
	// todo sign?
	//signed := tx
	return SubmitTransaction(ctx, s.api, signed)
}

// checkTxFee  todo
func checkTxFee(gasPrice types.Int256, gas uint64, cap float64) error {
	return nil
}

// toHexSlice creates a slice of hex-strings based on []byte.
func toHexSlice(b [][]byte) []string {
	r := make([]string, len(b))
	for i := range b {
		r[i] = hexutil.Encode(b[i])
	}
	return r
}

type Web3API struct {
	stack *API
}

func (s *Web3API) ClientVersion() string {
	return "testName"
}

func (s *Web3API) Sha3(input hexutil.Bytes) hexutil.Bytes {
	return crypto.Keccak256(input)
}

type DebugAPI struct {
	api *API
}

// NewDebugAPI creates a new instance of DebugAPI.
func NewDebugAPI(api *API) *DebugAPI {
	return &DebugAPI{api: api}
}

// SetHead rewinds the head of the blockchain to a previous block.
func (api *DebugAPI) SetHead(number hexutil.Uint64) {
	api.api.Downloader().Close()
	api.api.BlockChain().SetHead(uint64(number))
}

func (debug *DebugAPI) GetAccount(ctx context.Context, address mvm_common.Address) {

}

// NetAPI offers network related RPC methods
type NetAPI struct {
	api            *API
	networkVersion uint64
}

// NewNetAPI creates a new net API instance.
func NewNetAPI(api *API, networkVersion uint64) *NetAPI {
	return &NetAPI{api, networkVersion}
}

// Listening returns an indication if the node is listening for network connections.
func (s *NetAPI) Listening() bool {
	return true // always listening
}

// PeerCount returns the number of connected peers
func (s *NetAPI) PeerCount() hexutil.Uint {
	return hexutil.Uint(s.api.P2pServer().PeerCount())
}

// Version returns the current ethereum protocol version.
func (s *NetAPI) Version() string {
	//todo networkID == chainID？ s.api.GetChainConfig().ChainID
	return fmt.Sprintf("%d", s.networkVersion)
}

// TxsPoolAPI offers and API for the transaction pool. It only operates on data that is non confidential.
type TxsPoolAPI struct {
	api *API
}

// NewTxsPoolAPI creates a new tx pool service that gives information about the transaction pool.
func NewTxsPoolAPI(api *API) *TxsPoolAPI {
	return &TxsPoolAPI{api}
}

// Content returns the transactions contained within the transaction pool.
func (s *TxsPoolAPI) Content() map[string]map[string]map[string]*RPCTransaction {
	content := map[string]map[string]map[string]*RPCTransaction{
		"pending": make(map[string]map[string]*RPCTransaction),
		"queued":  make(map[string]map[string]*RPCTransaction),
	}
	pending, queue := s.api.TxsPool().Content()
	curHeader := s.api.BlockChain().CurrentBlock().Header()
	// Flatten the pending transactions
	for account, txs := range pending {
		dump := make(map[string]*RPCTransaction)
		for _, tx := range txs {
			dump[fmt.Sprintf("%d", tx.Nonce())] = newRPCPendingTransaction(tx, curHeader)
		}
		content["pending"][mvm_types.FromAmcAddress(&account).Hex()] = dump
	}
	// Flatten the queued transactions
	for account, txs := range queue {
		dump := make(map[string]*RPCTransaction)
		for _, tx := range txs {
			dump[fmt.Sprintf("%d", tx.Nonce())] = newRPCPendingTransaction(tx, curHeader)
		}
		content["queued"][mvm_types.FromAmcAddress(&account).Hex()] = dump
	}
	return content
}