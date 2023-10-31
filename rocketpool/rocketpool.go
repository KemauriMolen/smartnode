package main

import (
	"fmt"
	"os"

	"github.com/urfave/cli"

	"github.com/rocket-pool/smartnode/rocketpool/api"
	"github.com/rocket-pool/smartnode/rocketpool/common/services"
	"github.com/rocket-pool/smartnode/rocketpool/node"
	"github.com/rocket-pool/smartnode/rocketpool/watchtower"
	"github.com/rocket-pool/smartnode/shared"
	apiutils "github.com/rocket-pool/smartnode/shared/utils/api"
)

// Run
func main() {

	// Initialise application
	app := cli.NewApp()

	// Set application info
	app.Name = "rocketpool"
	app.Usage = "Rocket Pool service"
	app.Version = shared.RocketPoolVersion
	app.Authors = []cli.Author{
		{
			Name:  "David Rugendyke",
			Email: "david@rocketpool.net",
		},
		{
			Name:  "Jake Pospischil",
			Email: "jake@rocketpool.net",
		},
		{
			Name:  "Joe Clapis",
			Email: "joe@rocketpool.net",
		},
		{
			Name:  "Kane Wallmann",
			Email: "kane@rocketpool.net",
		},
	}
	app.Copyright = "(C) 2023 Rocket Pool Pty Ltd"

	// Set application flags
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "settings, s",
			Usage: "Rocket Pool service user config absolute `path`",
			Value: "/.rocketpool/user-settings.yml",
		},
		cli.StringFlag{
			Name:  "metricsAddress, m",
			Usage: "Address to serve metrics on if enabled",
			Value: "0.0.0.0",
		},
		cli.UintFlag{
			Name:  "metricsPort, r",
			Usage: "Port to serve metrics on if enabled",
			Value: 9102,
		},
		cli.BoolFlag{
			Name:  "ignore-sync-check",
			Usage: "Set this to true if you already checked the sync status of the execution client(s) and don't need to re-check it for this command",
		},
		cli.BoolFlag{
			Name:  "force-fallbacks",
			Usage: "Set this to true if you know the primary EC or CC is offline and want to bypass its health checks, and just use the fallback EC and CC instead",
		},
		cli.BoolFlag{
			Name:  "use-protected-api",
			Usage: "Set this to true to use the Flashbots Protect RPC instead of your local Execution Client. Useful to ensure your transactions aren't front-run.",
		},
	}

	// Register primary daemon
	app.Commands = append(app.Commands, cli.Command{
		Name:    "node",
		Aliases: []string{"n"},
		Usage:   "Run primary Rocket Pool node activity daemon and API server",
		Action: func(c *cli.Context) error {
			// Create the service provider
			sp, err := services.NewServiceProvider(c)
			if err != nil {
				return fmt.Errorf("error creating service provider: %w", err)
			}

			// Create the API server
			apiMgr := api.NewApiManager(sp)
			err = apiMgr.Start()
			if err != nil {
				return fmt.Errorf("error starting API server: %w", err)
			}

			return node.Run(sp)
		},
	})

	// Register watchtower daemon
	app.Commands = append(app.Commands, cli.Command{
		Name:    "watchtower",
		Aliases: []string{"w"},
		Usage:   "Run Rocket Pool watchtower activity daemon for Oracle DAO duties",
		Action: func(c *cli.Context) error {
			// Create the service provider
			sp, err := services.NewServiceProvider(c)
			if err != nil {
				return fmt.Errorf("error creating service provider: %w", err)
			}
			return watchtower.Run(sp)
		},
	})

	// Get command being run
	var commandName string
	app.Before = func(c *cli.Context) error {
		commandName = c.Args().First()
		return nil
	}

	// Run application
	if err := app.Run(os.Args); err != nil {
		if commandName == "api" {
			apiutils.PrintErrorResponse(err)
		} else {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

}
