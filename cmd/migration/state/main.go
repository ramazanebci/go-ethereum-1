package main

import "github.com/ethereum/go-ethereum/core/rawdb"

func main() {
	// http://apne2c-mainnet-debug01.kroma.network:8545
	dbDir := "/Users/logan/Downloads/geth/chaindata"
	genesisFile := "/Users/logan/Projects/kroma-network/kroma/.devnet/genesis-l2.json"

	//dbDir := "/.kroma/db/migration/geth/chaindata"
	//genesisFile := "/.kroma/db/migration/migration/genesis.json"

	db := must1(rawdb.Open(rawdb.OpenOptions{
		Type:      "",
		Directory: dbDir,
		Namespace: "",
		Cache:     0,
		Handles:   0,
		ReadOnly:  false,
	}))

	newMigrator(db, genesisFile, "http://localhost:9545").start()
}
