package auction

import (
	"context"
	"fmt"
	"math/big"

	batch "github.com/rocket-pool/batch-query"
	"github.com/rocket-pool/rocketpool-go/auction"
	"github.com/rocket-pool/rocketpool-go/settings"
	"github.com/urfave/cli"
	"golang.org/x/sync/errgroup"

	"github.com/rocket-pool/smartnode/shared/services"
	"github.com/rocket-pool/smartnode/shared/types/api"
)

func recoverRplFromLot(c *cli.Context, lotIndex uint64) (*api.RecoverRPLFromLotResponse, error) {

	// Get services
	if err := services.RequireNodeWallet(c); err != nil {
		return nil, err
	}
	if err := services.RequireRocketStorage(c); err != nil {
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

	// Response
	response := api.RecoverRPLFromLotResponse{}

	// Sync
	var wg errgroup.Group
	var currentBlock uint64

	// Create the bindings
	lot, err := auction.NewAuctionLot(rp, lotIndex)
	if err != nil {
		return nil, fmt.Errorf("error creating lot %d binding: %w", lotIndex, err)
	}
	pSettings, err := settings.NewProtocolDaoSettings(rp)
	if err != nil {
		return nil, fmt.Errorf("error creating pDAO settings binding: %w", err)
	}

	// Get contract state
	wg.Go(func() error {
		err := rp.Query(func(mc *batch.MultiCaller) error {
			lot.GetLotExists(mc)
			lot.GetLotEndBlock(mc)
			lot.GetLotRemainingRplAmount(mc)
			lot.GetLotRplRecovered(mc)
			pSettings.GetBidOnAuctionLotEnabled(mc)
			return nil
		}, nil)
		if err != nil {
			return fmt.Errorf("error getting contract state: %w", err)
		}
		return nil
	})

	// Get the current block
	wg.Go(func() error {
		header, err := rp.Client.HeaderByNumber(context.Background(), nil)
		if err == nil {
			currentBlock = header.Number.Uint64()
		}
		return fmt.Errorf("error getting current EL block header: %w", err)
	})

	// Wait for data
	if err := wg.Wait(); err != nil {
		return nil, err
	}

	// Check for validity
	response.DoesNotExist = !lot.Details.Exists
	response.BiddingNotEnded = !(currentBlock >= lot.Details.EndBlock.Formatted())
	response.NoUnclaimedRPL = (lot.Details.RemainingRplAmount.Cmp(big.NewInt(0)) == 0)
	response.RPLAlreadyRecovered = lot.Details.RplRecovered
	response.CanRecover = !(response.DoesNotExist || response.BiddingNotEnded || response.NoUnclaimedRPL || response.RPLAlreadyRecovered)

	// Get tx info
	if response.CanRecover {
		opts, err := w.GetNodeAccountTransactor()
		if err != nil {
			return nil, fmt.Errorf("error getting node account transactor: %w", err)
		}
		txInfo, err := lot.RecoverUnclaimedRpl(opts)
		if err != nil {
			return nil, fmt.Errorf("error getting TX info for RecoverUnclaimedRpl: %w", err)
		}
		response.TxInfo = txInfo
	}

	return &response, nil
}
