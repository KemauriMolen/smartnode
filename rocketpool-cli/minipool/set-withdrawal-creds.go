package minipool

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/rocket-pool/smartnode/shared/services/rocketpool"
	cliutils "github.com/rocket-pool/smartnode/shared/utils/cli"
	"github.com/rocket-pool/smartnode/shared/utils/cli/migration"
	"github.com/urfave/cli"
)

func setWithdrawalCreds(c *cli.Context, minipoolAddress common.Address) error {

	// Get RP client
	rp, err := rocketpool.NewClientFromCtx(c)
	if err != nil {
		return err
	}
	defer rp.Close()

	// Check and assign the EC status
	err = cliutils.CheckClientStatus(rp)
	if err != nil {
		return err
	}

	fmt.Printf("This will convert the withdrawal credentials for minipool %s's validator from the old 0x00 (BLS) value to the minipool address. This is meant for solo validator conversion **only**.\n\n", minipoolAddress.Hex())

	success := migration.ChangeWithdrawalCreds(rp, minipoolAddress, "")
	if !success {
		fmt.Println("Your withdrawal credentials cannot be automatically changed at this time. Import aborted.\nYou can try again later by using `rocketpool minipool set-withdrawal-creds`.")
	}

	return nil
}