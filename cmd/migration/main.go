package main

import (
	"fmt"
	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/console/prompt"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/internal/debug"
	"github.com/ethereum/go-ethereum/internal/flags"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"go.uber.org/automaxprocs/maxprocs"
	"os"
	"path/filepath"
	"sort"

	// Force-load the tracer engines to trigger registration
	_ "github.com/ethereum/go-ethereum/eth/tracers/js"
	_ "github.com/ethereum/go-ethereum/eth/tracers/native"

	"github.com/urfave/cli/v2"
)

var app = flags.NewApp("the go-ethereum command line interface")

func init() {
	// Initialize the CLI app and start Geth
	app.Action = migrate
	sort.Sort(cli.CommandsByName(app.Commands))

	app.Flags = flags.Merge(
		utils.DatabaseFlags,
	)
	flags.AutoEnvVars(app.Flags, "GETH")

	app.Before = func(ctx *cli.Context) error {
		maxprocs.Set() // Automatically set GOMAXPROCS to match Linux container CPU quota.
		flags.MigrateGlobalFlags(ctx)
		if err := debug.Setup(ctx); err != nil {
			return err
		}
		flags.CheckEnvVars(ctx, app.Flags, "GETH")
		return nil
	}
	app.After = func(ctx *cli.Context) error {
		debug.Exit()
		prompt.Stdin.Close() // Resets terminal mode.
		return nil
	}
}

func migrate(ctx *cli.Context) error {
	log.SetDefault(log.NewLogger(log.LogfmtHandlerWithLevel(os.Stdout, log.LevelInfo)))

	config := &node.Config{}
	utils.SetNodeConfig(ctx, config)

	db, err := rawdb.Open(rawdb.OpenOptions{
		Type:      config.DBEngine,
		Directory: filepath.Join(config.DataDir, "geth", "chaindata"),
		Namespace: "",
		Cache:     0,
		Handles:   0,
		ReadOnly:  false,
	})
	if err != nil {
		return err
	}

	migrator, err := newStateMigrator(db)
	if err != nil {
		return err
	}
	stateRoot, err := migrator.migrateAccount()
	if err != nil {
		return err
	}
	res, err := migrator.migrateHeadAndGenesis(stateRoot)
	if err != nil {
		return err
	}

	log.Info("Migration task finished", "height", res.TransitionHeight, "timestamp", res.TransitionTimestamp, "blockHash", res.TransitionBlockHash, "stateRoot", stateRoot)

	return nil
}

func main() {
	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
