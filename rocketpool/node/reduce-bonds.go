package node

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/docker/docker/client"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/rocket-pool/rocketpool-go/minipool"
	"github.com/rocket-pool/rocketpool-go/rocketpool"
	"github.com/rocket-pool/rocketpool-go/types"
	"github.com/rocket-pool/rocketpool-go/utils/eth"
	"github.com/urfave/cli"

	rpstate "github.com/rocket-pool/rocketpool-go/utils/state"
	"github.com/rocket-pool/smartnode/shared/services"
	"github.com/rocket-pool/smartnode/shared/services/config"
	rpgas "github.com/rocket-pool/smartnode/shared/services/gas"
	"github.com/rocket-pool/smartnode/shared/services/state"
	"github.com/rocket-pool/smartnode/shared/services/wallet"
	"github.com/rocket-pool/smartnode/shared/utils/api"
	"github.com/rocket-pool/smartnode/shared/utils/log"
)

// The fraction of the timeout period to trigger overdue transactions
const reduceBondTimeoutSafetyFactor int = 2

// Reduce bonds task
type reduceBonds struct {
	c              *cli.Context
	log            log.ColorLogger
	cfg            *config.RocketPoolConfig
	w              *wallet.Wallet
	rp             *rocketpool.RocketPool
	d              *client.Client
	gasThreshold   float64
	maxFee         *big.Int
	maxPriorityFee *big.Int
	gasLimit       uint64
	m              *state.NetworkStateManager
	s              *state.NetworkState
}

// Details required to check for bond reduction eligibility
type minipoolBondReductionDetails struct {
	Address             common.Address
	DepositBalance      *big.Int
	ReduceBondTime      time.Time
	ReduceBondCancelled bool
	Status              types.MinipoolStatus
}

// Create reduce bonds task
func newReduceBonds(c *cli.Context, logger log.ColorLogger, m *state.NetworkStateManager) (*reduceBonds, error) {

	// Get services
	cfg, err := services.GetConfig(c)
	if err != nil {
		return nil, err
	}
	w, err := services.GetWallet(c)
	if err != nil {
		return nil, err
	}
	rp, err := services.GetRocketPool(c)
	if err != nil {
		return nil, err
	}
	d, err := services.GetDocker(c)
	if err != nil {
		return nil, err
	}

	// Check if auto-staking is disabled
	gasThreshold := cfg.Smartnode.MinipoolStakeGasThreshold.Value.(float64)

	// Get the user-requested max fee
	maxFeeGwei := cfg.Smartnode.ManualMaxFee.Value.(float64)
	var maxFee *big.Int
	if maxFeeGwei == 0 {
		maxFee = nil
	} else {
		maxFee = eth.GweiToWei(maxFeeGwei)
	}

	// Get the user-requested max fee
	priorityFeeGwei := cfg.Smartnode.PriorityFee.Value.(float64)
	var priorityFee *big.Int
	if priorityFeeGwei == 0 {
		logger.Println("WARNING: priority fee was missing or 0, setting a default of 2.")
		priorityFee = eth.GweiToWei(2)
	} else {
		priorityFee = eth.GweiToWei(priorityFeeGwei)
	}

	// Return task
	return &reduceBonds{
		c:              c,
		log:            logger,
		cfg:            cfg,
		w:              w,
		rp:             rp,
		d:              d,
		gasThreshold:   gasThreshold,
		maxFee:         maxFee,
		maxPriorityFee: priorityFee,
		gasLimit:       0,
		m:              m,
	}, nil

}

// Reduce bonds
func (t *reduceBonds) run(isAtlasDeployed bool) error {

	// Reload the wallet (in case a call to `node deposit` changed it)
	if err := t.w.Reload(); err != nil {
		return err
	}

	// Wait for eth client to sync
	if err := services.WaitEthClientSynced(t.c, true); err != nil {
		return err
	}

	// Check if Atlas has been deployed yet
	if !isAtlasDeployed {
		return nil
	}

	// Log
	t.log.Println("Checking for minipool bonds to reduce...")

	// Get the latest state
	t.s = t.m.GetLatestState()
	opts := &bind.CallOpts{
		BlockNumber: big.NewInt(0).SetUint64(t.s.ElBlockNumber),
	}

	// Get node account
	nodeAccount, err := t.w.GetNodeAccount()
	if err != nil {
		return err
	}

	// Get the bond reduction details
	windowStart := t.s.NetworkDetails.BondReductionWindowStart
	windowLength := t.s.NetworkDetails.BondReductionWindowLength

	// Get the time of the latest block
	latestEth1Block, err := t.rp.Client.HeaderByNumber(context.Background(), opts.BlockNumber)
	if err != nil {
		return fmt.Errorf("can't get the latest block time: %w", err)
	}
	latestBlockTime := time.Unix(int64(latestEth1Block.Time), 0)

	// Get reduceable minipools
	minipools, err := t.getReduceableMinipools(nodeAccount.Address, windowStart, windowLength, latestBlockTime, opts)
	if err != nil {
		return err
	}
	if len(minipools) == 0 {
		return nil
	}

	// Log
	t.log.Printlnf("%d minipool(s) are ready for bond reduction...", len(minipools))

	// Reduce bonds
	successCount := 0
	for _, mp := range minipools {
		success, err := t.reduceBond(mp, windowStart, windowLength, latestBlockTime, opts)
		if err != nil {
			t.log.Println(fmt.Errorf("could not reduce bond for minipool %s: %w", mp.MinipoolAddress.Hex(), err))
			return err
		}
		if success {
			successCount++
		}
	}

	// Return
	return nil

}

// Get reduceable minipools
func (t *reduceBonds) getReduceableMinipools(nodeAddress common.Address, windowStart time.Duration, windowLength time.Duration, latestBlockTime time.Time, opts *bind.CallOpts) ([]*rpstate.NativeMinipoolDetails, error) {

	// Filter minipools
	reduceableMinipools := []*rpstate.NativeMinipoolDetails{}
	for _, mpd := range t.s.MinipoolDetailsByNode[nodeAddress] {

		// TEMP
		reduceBondTime, err := minipool.GetReduceBondTime(t.rp, mpd.MinipoolAddress, opts)
		if err != nil {
			return nil, fmt.Errorf("error getting reduce bond time for minipool %s: %w", mpd.MinipoolAddress.Hex(), err)
		}
		reduceBondCancelled, err := minipool.GetReduceBondCancelled(t.rp, mpd.MinipoolAddress, opts)
		if err != nil {
			return nil, fmt.Errorf("error getting reduce bond cancelled for minipool %s: %w", mpd.MinipoolAddress.Hex(), err)
		}

		depositBalance := eth.WeiToEth(mpd.NodeDepositBalance)
		timeSinceReductionStart := latestBlockTime.Sub(reduceBondTime)

		if depositBalance == 16 &&
			timeSinceReductionStart < (windowStart+windowLength) &&
			!reduceBondCancelled &&
			mpd.Status == types.Staking {
			if timeSinceReductionStart > windowStart {
				reduceableMinipools = append(reduceableMinipools, mpd)
			} else {
				remainingTime := windowStart - timeSinceReductionStart
				t.log.Printlnf("Minipool %s has %s left until it can have its bond reduced.", mpd.MinipoolAddress.Hex(), remainingTime)
			}
		}
	}

	// Return
	return reduceableMinipools, nil

}

// Reduce a minipool's bond
func (t *reduceBonds) reduceBond(mpd *rpstate.NativeMinipoolDetails, windowStart time.Duration, windowLength time.Duration, latestBlockTime time.Time, callOpts *bind.CallOpts) (bool, error) {

	// Log
	t.log.Printlnf("Reducing bond for minipool %s...", mpd.MinipoolAddress.Hex())

	// Get transactor
	opts, err := t.w.GetNodeAccountTransactor()
	if err != nil {
		return false, err
	}

	// Make the minipool binding
	mpBinding, err := minipool.NewMinipoolFromVersion(t.rp, mpd.MinipoolAddress, mpd.Version, nil)
	if err != nil {
		return false, fmt.Errorf("error creating minipool binding for %s: %w", mpd.MinipoolAddress.Hex(), err)
	}

	// Get the updated minipool interface
	mpv3, success := minipool.GetMinipoolAsV3(mpBinding)
	if !success {
		return false, fmt.Errorf("cannot reduce bond for minipool %s because its delegate version is too low (v%d); please update the delegate", mpBinding.GetAddress().Hex(), mpBinding.GetVersion())
	}

	// Get the gas limit
	gasInfo, err := mpv3.EstimateReduceBondAmountGas(opts)
	if err != nil {
		return false, fmt.Errorf("could not estimate the gas required to reduce bond: %w", err)
	}
	var gas *big.Int
	if t.gasLimit != 0 {
		gas = new(big.Int).SetUint64(t.gasLimit)
	} else {
		gas = new(big.Int).SetUint64(gasInfo.SafeGasLimit)
	}

	// Get the max fee
	maxFee := t.maxFee
	if maxFee == nil || maxFee.Uint64() == 0 {
		maxFee, err = rpgas.GetHeadlessMaxFeeWei()
		if err != nil {
			return false, err
		}
	}

	// TEMP
	reduceBondTime, err := minipool.GetReduceBondTime(t.rp, mpd.MinipoolAddress, callOpts)
	if err != nil {
		return false, fmt.Errorf("error getting reduce bond time for minipool %s: %w", mpd.MinipoolAddress.Hex(), err)
	}

	// Print the gas info
	if !api.PrintAndCheckGasInfo(gasInfo, true, t.gasThreshold, t.log, maxFee, t.gasLimit) {
		// Check for the timeout buffer
		timeSinceReductionStart := latestBlockTime.Sub(reduceBondTime)
		remainingTime := (windowStart + windowLength) - timeSinceReductionStart
		isDue := remainingTime < (windowLength / time.Duration(reduceBondTimeoutSafetyFactor))
		if !isDue {
			timeUntilDue := remainingTime - (windowLength / time.Duration(reduceBondTimeoutSafetyFactor))
			t.log.Printlnf("Time until bond reduction will be forced: %s", timeUntilDue)
			return false, nil
		}

		t.log.Println("NOTICE: The minipool has exceeded half of the timeout period, so its bond reduction will be forced at the current gas price.")
	}

	opts.GasFeeCap = maxFee
	opts.GasTipCap = t.maxPriorityFee
	opts.GasLimit = gas.Uint64()

	// Reduce bond
	hash, err := mpv3.ReduceBondAmount(opts)
	if err != nil {
		return false, err
	}

	// Print TX info and wait for it to be included in a block
	err = api.PrintAndWaitForTransaction(t.cfg, hash, t.rp.Client, t.log)
	if err != nil {
		return false, err
	}

	// Log
	t.log.Printlnf("Successfully reduced bond for minipool %s.", mpd.MinipoolAddress.Hex())

	// Return
	return true, nil

}