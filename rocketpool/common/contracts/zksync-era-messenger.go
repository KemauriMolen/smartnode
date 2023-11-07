package contracts

import (
	"fmt"
	"math/big"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	batch "github.com/rocket-pool/batch-query"
	"github.com/rocket-pool/rocketpool-go/core"
)

const (
	zksyncEraMessengerAbiString string = `[{"inputs":[],"name":"rateStale","outputs":[{"internalType":"bool","name":"","type":"bool"}],"stateMutability":"view","type":"function"},{"inputs":[{"internalType":"uint256","name":"_l2GasLimit","type":"uint256"},{"internalType":"uint256","name":"_l2GasPerPubdataByteLimit","type":"uint256"}],"name":"submitRate","outputs":[],"stateMutability":"payable","type":"function"}]`
)

// ABI cache
var zksyncEraMessengerAbi abi.ABI
var zksyncEraOnce sync.Once

// ===============
// === Structs ===
// ===============

// Binding for the zkSync Era Messenger
type ZkSyncEraMessenger struct {

	// === Internal fields ===
	contract *core.Contract
}

// ====================
// === Constructors ===
// ====================

// Creates a new zkSync Era Messenger contract binding
func NewZkSyncEraMessenger(address common.Address, client core.ExecutionClient) (*ZkSyncEraMessenger, error) {
	// Parse the ABI
	var err error
	zksyncEraOnce.Do(func() {
		var parsedAbi abi.ABI
		parsedAbi, err = abi.JSON(strings.NewReader(zksyncEraMessengerAbiString))
		if err == nil {
			zksyncEraMessengerAbi = parsedAbi
		}
	})
	if err != nil {
		return nil, fmt.Errorf("error parsing zkSync Era messenger ABI: %w", err)
	}

	// Create the contract
	contract := &core.Contract{
		Contract: bind.NewBoundContract(address, zksyncEraMessengerAbi, client, client, client),
		Address:  &address,
		ABI:      &zksyncEraMessengerAbi,
		Client:   client,
	}

	return &ZkSyncEraMessenger{
		contract: contract,
	}, nil
}

// =============
// === Calls ===
// =============

// Check if the RPL rate is stale and needs to be updated
func (c *ZkSyncEraMessenger) IsRateStale(mc *batch.MultiCaller, out *bool) {
	core.AddCall(mc, c.contract, out, "rateStale")
}

// ====================
// === Transactions ===
// ====================

// Send the latest RPL rate to the L2
func (c *ZkSyncEraMessenger) SubmitRate(l2GasLimit *big.Int, l2GasPerPubdataByteLimit *big.Int, opts *bind.TransactOpts) (*core.TransactionInfo, error) {
	return core.NewTransactionInfo(c.contract, "submitRate", opts, l2GasLimit, l2GasPerPubdataByteLimit)
}