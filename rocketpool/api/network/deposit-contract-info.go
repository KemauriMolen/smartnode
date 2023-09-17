package network

import (
	"fmt"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	rputils "github.com/rocket-pool/smartnode/rocketpool/utils/rp"
	"github.com/rocket-pool/smartnode/shared/types/api"
)

// ===============
// === Factory ===
// ===============

type networkDepositInfoContextFactory struct {
	handler *NetworkHandler
}

func (f *networkDepositInfoContextFactory) Create(vars map[string]string) (*networkDepositInfoContext, error) {
	c := &networkDepositInfoContext{
		handler: f.handler,
	}
	return c, nil
}

// ===============
// === Context ===
// ===============

type networkDepositInfoContext struct {
	handler *NetworkHandler
}

func (c *networkDepositInfoContext) PrepareData(data *api.NetworkDepositContractInfoData, opts *bind.TransactOpts) error {
	sp := c.handler.serviceProvider
	rp := sp.GetRocketPool()
	cfg := sp.GetConfig()
	bc := sp.GetBeaconClient()

	// Requirements
	err := sp.RequireEthClientSynced()
	if err != nil {
		return err
	}

	// Get the deposit contract info
	info, err := rputils.GetDepositContractInfo(rp, cfg, bc)
	if err != nil {
		return fmt.Errorf("error getting deposit contract info: %w", err)
	}
	data.SufficientSync = true
	data.RPNetwork = info.RPNetwork
	data.RPDepositContract = info.RPDepositContract
	data.BeaconNetwork = info.BeaconNetwork
	data.BeaconDepositContract = info.BeaconDepositContract
	return nil
}
