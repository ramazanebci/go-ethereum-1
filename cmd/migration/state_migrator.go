package main

import (
	"bytes"
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/trie/trienode"
	"github.com/ethereum/go-ethereum/trie/zk"
	"math/big"
	"strings"
	"time"
)

var (
	// BedrockTransitionBlockExtraData represents the extradata
	// set in the very first bedrock block. This value must be
	// less than 32 bytes long or it will create an invalid block.
	BedrockTransitionBlockExtraData = []byte("BEDROCK")
	L2GenesisBlockGasLimit          = 0
	L2OutputOracleStartingTimestamp = 0
	InitialBaseFee                  = int64(0)
)

type stateMigrator struct {
	db             ethdb.Database
	zkdb           *trie.Database
	mptdb          *trie.Database
	genesisAccount map[common.Hash]common.Address
	genesisStorage map[common.Hash][]byte
}

func newStateMigrator(db ethdb.Database) (*stateMigrator, error) {
	genesisAccount, genesisStorage, err := readGenesisAlloc(db)
	if err != nil {
		return nil, err
	}

	return &stateMigrator{
		db: db,
		zkdb: trie.NewDatabase(db, &trie.Config{
			Preimages:   true,
			Zktrie:      true,
			KromaZKTrie: true,
		}),
		mptdb:          trie.NewDatabase(db, &trie.Config{Preimages: true}),
		genesisAccount: genesisAccount,
		genesisStorage: genesisStorage,
	}, nil
}

func (m *stateMigrator) migrateAccount() (common.Hash, error) {
	header := rawdb.ReadHeadHeader(m.db)
	fmt.Println("start migration at account root.", header.Root, "block number", header.Number)

	mpt, err := trie.NewStateTrie(trie.TrieID(types.EmptyRootHash), m.mptdb)
	if err != nil {
		return common.Hash{}, err
	}
	mergedNodeSet := trienode.NewMergedNodeSet()
	status := newStatus()
	zkAccIt, err := openZkIterator(m.zkdb, header.Root)
	if err != nil {
		return common.Hash{}, err
	}
	for zkAccIt.Next(false) {
		if !zkAccIt.Leaf() {
			continue
		}
		address := common.BytesToAddress(m.readPreimage(zkAccIt.LeafKey()))
		storageStatus := newStatus()
		acc, err := types.NewStateAccount(zkAccIt.LeafBlob(), true)
		if err != nil {
			return common.Hash{}, err
		}
		acc.Root, err = m.migrateStorage(address, acc.Root, storageStatus)
		if err != nil {
			return common.Hash{}, err
		}
		if err := mpt.UpdateAccount(address, acc); err != nil {
			return common.Hash{}, err
		}
		if storageStatus.count > 0 {
			storageStatus.emitCompleteLog("contract", address.Hex(), "index", common.BytesToHash(zkAccIt.LeafKey()).Hex())
		}
		status.emitLog(false, "account ", address.Hex(), "index", common.BytesToHash(zkAccIt.LeafKey()).Hex())
	}
	status.startDBCommit()
	accountRoot, set, err := mpt.Commit(true)
	if err != nil {
		return common.Hash{}, err
	}
	if err := mergedNodeSet.Merge(set); err != nil {
		return common.Hash{}, err
	}
	if err := m.mptdb.Update(accountRoot, types.EmptyRootHash, 0, mergedNodeSet, nil); err != nil {
		return common.Hash{}, err
	}
	if err := m.mptdb.Commit(accountRoot, false); err != nil {
		return common.Hash{}, err
	}
	status.emitCompleteLog("account ")

	return accountRoot, nil
}

func (m *stateMigrator) migrateStorage(
	address common.Address,
	zkStorageRoot common.Hash,
	status *status,
) (common.Hash, error) {
	if zkStorageRoot == types.GetEmptyRootHash(true) {
		return types.EmptyRootHash, nil
	}
	id := trie.StorageTrieID(types.EmptyRootHash, crypto.Keccak256Hash(address.Bytes()), types.EmptyRootHash)
	mpt, err := trie.NewStateTrie(id, trie.NewDatabase(m.db, &trie.Config{Preimages: true}))
	if err != nil {
		return common.Hash{}, err
	}
	mergedNodeSet := trienode.NewMergedNodeSet()
	zkStorageIt, err := openZkIterator(m.zkdb, zkStorageRoot)
	if err != nil {
		return common.Hash{}, err
	}
	for zkStorageIt.Next(false) {
		if !zkStorageIt.Leaf() {
			continue
		}
		slot := m.readPreimage(zkStorageIt.LeafKey())
		if err := mpt.UpdateStorage(common.Address{}, slot, zkStorageIt.LeafBlob()); err != nil {
			return common.Hash{}, err
		}
		status.emitLog(false, "contract", address.Hex(), "index", common.BytesToHash(zkStorageIt.LeafKey()).Hex())
	}
	status.startDBCommit()
	storageRoot, set, err := mpt.Commit(true)
	if err != nil {
		return common.Hash{}, err
	}
	if err := mergedNodeSet.Merge(set); err != nil {
		return common.Hash{}, err
	}
	if err := m.mptdb.Update(storageRoot, types.EmptyRootHash, 0, mergedNodeSet, nil); err != nil {
		return common.Hash{}, err
	}
	if err := m.mptdb.Commit(storageRoot, false); err != nil {
		return common.Hash{}, err
	}
	return storageRoot, nil
}

func (m *stateMigrator) readPreimage(key []byte) []byte {
	keyHash := *trie.IteratorKeyToHash(key, true)
	if addr, ok := m.genesisAccount[keyHash]; ok {
		return addr.Bytes()
	}
	if slot, ok := m.genesisStorage[keyHash]; ok {
		return slot
	}
	if preimage := m.zkdb.Preimage(keyHash); common.BytesToHash(zk.MustNewSecureHash(preimage).Bytes()).Hex() == keyHash.Hex() {
		return preimage
	}
	panic(fmt.Sprintf("%v preimage does not exist.", keyHash.Hex()))
}

type MigrationResult struct {
	TransitionHeight    uint64
	TransitionTimestamp uint64
	TransitionBlockHash common.Hash
}

func (m *stateMigrator) migrateHeadAndGenesis(stateRoot common.Hash) (*MigrationResult, error) {
	// Grab the hash of the tip of the legacy chain.
	hash := rawdb.ReadHeadHeaderHash(m.db)
	log.Info("Reading chain tip from database", "hash", hash)

	// Grab the header number.
	num := rawdb.ReadHeaderNumber(m.db, hash)
	if num == nil {
		return nil, fmt.Errorf("cannot find header number for %s", hash)
	}

	// Grab the full header.
	header := rawdb.ReadHeader(m.db, hash, *num)
	log.Info("Read header from database", "number", *num)

	// Ensure that the extradata is valid.
	if size := len(BedrockTransitionBlockExtraData); size > 32 {
		return nil, fmt.Errorf("transition block extradata too long: %d", size)
	}

	// We write special extra data into the Bedrock transition block to indicate that the migration
	// has already happened. If we detect this extra data, we can skip the migration.
	if bytes.Equal(header.Extra, BedrockTransitionBlockExtraData) {
		log.Info("Detected migration already happened", "root", header.Root, "blockhash", header.Hash())

		return &MigrationResult{
			TransitionHeight:    *num,
			TransitionTimestamp: header.Time,
			TransitionBlockHash: hash,
		}, nil
	}

	dbFactory := func() (*state.StateDB, error) {
		// Set up the backing store.
		underlyingDB := state.NewDatabaseWithConfig(m.db, &trie.Config{
			Preimages: true,
			IsVerkle:  false,
		})

		// Open up the state database.
		db, err := state.New(stateRoot, underlyingDB, nil)
		if err != nil {
			return nil, fmt.Errorf("cannot open StateDB: %w", err)
		}

		return db, nil
	}

	db, err := dbFactory()
	if err != nil {
		return nil, err
	}

	newRoot, err := db.Commit(0, true)
	if err != nil {
		return nil, err
	}

	// Create the header for the Bedrock transition block.
	bedrockHeader := &types.Header{
		ParentHash:  header.Hash(),
		UncleHash:   types.EmptyUncleHash,
		Coinbase:    params.KromaProtocolVault,
		Root:        newRoot,
		TxHash:      types.EmptyRootHash,
		ReceiptHash: types.EmptyRootHash,
		Bloom:       types.Bloom{},
		Difficulty:  common.Big0,
		Number:      new(big.Int).Add(header.Number, common.Big1),
		GasLimit:    (uint64)(L2GenesisBlockGasLimit),
		GasUsed:     0,
		Time:        uint64(L2OutputOracleStartingTimestamp),
		Extra:       BedrockTransitionBlockExtraData,
		MixDigest:   common.Hash{},
		Nonce:       types.BlockNonce{},
		BaseFee:     big.NewInt(InitialBaseFee),
	}

	// Create the Bedrock transition block from the header. Note that there are no transactions,
	// uncle blocks, or receipts in the Bedrock transition block.
	bedrockBlock := types.NewBlock(bedrockHeader, nil, nil, nil, trie.NewStackTrie(nil))

	// We did it!
	log.Info(
		"Built Bedrock transition",
		"hash", bedrockBlock.Hash(),
		"root", bedrockBlock.Root(),
		"number", bedrockBlock.NumberU64(),
		"gas-used", bedrockBlock.GasUsed(),
		"gas-limit", bedrockBlock.GasLimit(),
	)

	// Create the result of the migration.
	res := &MigrationResult{
		TransitionHeight:    bedrockBlock.NumberU64(),
		TransitionTimestamp: bedrockBlock.Time(),
		TransitionBlockHash: bedrockBlock.Hash(),
	}

	// If we're not actually writing this to disk, then we're done.
	//if !commit {
	//	log.Info("Dry run complete")
	//	return res, nil
	//}

	// Otherwise we need to write the changes to disk. First we commit the state changes.
	log.Info("Committing trie DB")
	if err := db.Database().TrieDB().Commit(newRoot, true); err != nil {
		return nil, err
	}

	// Next we write the Bedrock transition block to the database.
	rawdb.WriteTd(m.db, bedrockBlock.Hash(), bedrockBlock.NumberU64(), bedrockBlock.Difficulty())
	rawdb.WriteBlock(m.db, bedrockBlock)
	rawdb.WriteReceipts(m.db, bedrockBlock.Hash(), bedrockBlock.NumberU64(), nil)
	rawdb.WriteCanonicalHash(m.db, bedrockBlock.Hash(), bedrockBlock.NumberU64())
	rawdb.WriteHeadBlockHash(m.db, bedrockBlock.Hash())
	rawdb.WriteHeadFastBlockHash(m.db, bedrockBlock.Hash())
	rawdb.WriteHeadHeaderHash(m.db, bedrockBlock.Hash())

	// Make the first Bedrock block a finalized block.
	rawdb.WriteFinalizedBlockHash(m.db, bedrockBlock.Hash())

	// We need to update the chain config to set the correct hardforks.
	genesisHash := rawdb.ReadCanonicalHash(m.db, 0)
	cfg := rawdb.ReadChainConfig(m.db, genesisHash)
	if cfg == nil {
		log.Crit("chain config not found")
	}

	// Set the standard options.
	cfg.LondonBlock = bedrockBlock.Number()
	cfg.ArrowGlacierBlock = bedrockBlock.Number()
	cfg.GrayGlacierBlock = bedrockBlock.Number()
	cfg.MergeNetsplitBlock = bedrockBlock.Number()
	cfg.TerminalTotalDifficulty = big.NewInt(0)
	cfg.TerminalTotalDifficultyPassed = true

	// Set the Optimism options.
	cfg.BedrockBlock = bedrockBlock.Number()
	// Enable Regolith from the start of Bedrock
	cfg.RegolithTime = new(uint64)
	// Switch KromaConfig to OptimismConfig
	cfg.Optimism = &params.OptimismConfig{
		EIP1559Denominator:       cfg.Kroma.EIP1559Denominator,
		EIP1559Elasticity:        cfg.Kroma.EIP1559Elasticity,
		EIP1559DenominatorCanyon: cfg.Kroma.EIP1559DenominatorCanyon,
	}

	// Write the chain config to disk.
	rawdb.WriteChainConfig(m.db, genesisHash, cfg)

	// Yay!
	log.Info(
		"wrote chain config",
		"1559-denominator", cfg.Optimism.EIP1559Denominator,
		"1559-elasticity", cfg.Optimism.EIP1559Elasticity,
		"1559-denominator-canyon", cfg.Optimism.EIP1559DenominatorCanyon,
	)

	// We're done!
	log.Info(
		"wrote Bedrock transition block",
		"height", bedrockHeader.Number,
		"root", bedrockHeader.Root.String(),
		"hash", bedrockHeader.Hash().String(),
		"timestamp", bedrockHeader.Time,
	)

	// Return the result and have a nice day.
	return res, nil
}

type status struct {
	startAt       time.Time
	commitStartAt time.Time
	lastLogTime   time.Duration
	count         int
}

func newStatus() *status {
	return &status{startAt: time.Now(), lastLogTime: 30 * time.Second}
}

func (s *status) emitLog(force bool, prefix ...string) {
	s.count++
	if runtime := time.Since(s.startAt); runtime > s.lastLogTime || force {
		s.lastLogTime += 30 * time.Second
		fmt.Println(strings.Join(prefix, " "), "processing", s.count, "\trunning time", runtime)
	}
}

func (s *status) startDBCommit() {
	s.commitStartAt = time.Now()
}

func (s *status) emitCompleteLog(prefix ...string) {
	fmt.Println(strings.Join(prefix, " "), "complete", "processing", s.count, "\trunning time", time.Since(s.startAt), "\tcommit running time", time.Since(s.commitStartAt))
}

func openZkIterator(db *trie.Database, root common.Hash) (trie.NodeIterator, error) {
	tr, err := trie.NewZkMerkleStateTrie(root, db)
	if err != nil {
		return nil, err
	}
	return tr.NodeIterator(nil)
}
