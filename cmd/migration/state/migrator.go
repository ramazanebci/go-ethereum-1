package main

import (
	"encoding/json"
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/trie/trienode"
	"github.com/ethereum/go-ethereum/trie/zk"
	"math/big"
	"strings"
	"time"
)

type migrator struct {
	db             ethdb.Database
	zkdb           *trie.Database
	mptdb          *trie.Database
	genesisAccount map[common.Hash]common.Address
	genesisStorage map[common.Hash][]byte
	blockChain     BlockChain
}

func newMigrator(db ethdb.Database, genesisFilePath string, rpc string) *migrator {
	genesisAccount, genesisStorage := readGenesisAlloc(genesisFilePath)
	return &migrator{
		db: db,
		zkdb: trie.NewDatabase(db, &trie.Config{
			Preimages:   true,
			Zktrie:      true,
			KromaZKTrie: true,
		}),
		mptdb: trie.NewDatabase(db, &trie.Config{
			Preimages: true,
		}),
		genesisAccount: genesisAccount,
		genesisStorage: genesisStorage,
		blockChain:     &httpClient{rpc},
	}
}

func (m *migrator) start() {
	var root *migrationRoot
	if jsonBytes, _ := m.db.Get([]byte("migration-root")); len(jsonBytes) > 0 {
		if err := json.Unmarshal(jsonBytes, &root); err != nil {
			fmt.Println("invalid migration-root format", string(jsonBytes))
		}
	}
	if root == nil {
		root = m.migrateAccount() // head block 을 기준으로 전체 마이그레이션 시작 (오래걸림. mainnet 기준 4시간 이상)
		must(m.db.Put([]byte("migration-root"), must1(json.Marshal(root))))
	}
	for {
		if nextRoot := m.applyNewStateTransition(*root); nextRoot == nil { // 상태 변경된 account, storage 추가 반영
			time.Sleep(2 * time.Second)
		} else {
			must(m.db.Put([]byte("migration-root"), must1(json.Marshal(nextRoot))))
			root = nextRoot
		}
	}
}

func (m *migrator) migrateAccount() *migrationRoot {
	header := rawdb.ReadHeadHeader(m.db)
	fmt.Println("start migration at account root.", header.Root, "block number", header.Number)

	status := newStatus()
	mpt := m.newMPT(trie.TrieID(types.EmptyRootHash)) // 이전 mpt 상태가 없기 때문에 EmptyRootHash 로 시작
	for it := m.openZkIterator(header.Root); it.Next(false); {
		if !it.Leaf() {
			continue
		}
		storageStatus := newStatus()
		address := common.BytesToAddress(must1(m.readPreimage(it.LeafKey())))
		acc := must1(types.NewStateAccount(it.LeafBlob(), true))
		acc.Root = m.migrateStorage(address, acc.Root, storageStatus)
		must(mpt.UpdateAccount(address, acc))
		if storageStatus.count > 0 {
			storageStatus.emitCompleteLog("contract", address.Hex(), "index", common.BytesToHash(it.LeafKey()).Hex())
		}
		status.emitLog(false, "account ", address.Hex(), "index", common.BytesToHash(it.LeafKey()).Hex())
	}
	m.checkHashCollision(mpt)
	status.startDBCommit()
	root := m.commit(mpt)
	status.emitCompleteLog("account ")
	fmt.Println("state root", root.Hex(), "block number", header.Number)
	return &migrationRoot{root, header.Number.Uint64()}
}

func (m *migrator) migrateStorage(
	address common.Address,
	zkStorageRoot common.Hash,
	status *status,
) common.Hash {
	if zkStorageRoot == types.GetEmptyRootHash(true) {
		return types.EmptyRootHash
	}
	mpt := m.newMPT(trie.StorageTrieID(types.EmptyRootHash, crypto.Keccak256Hash(address.Bytes()), types.EmptyRootHash))
	for it := m.openZkIterator(zkStorageRoot); it.Next(false); {
		if !it.Leaf() {
			continue
		}
		slot, err := m.readPreimage(it.LeafKey())
		if err != nil {
			if address.Hex() == "0x4200000000000000000000000000000000000070" { // devnet 으로 띄운 경우, 없는 경우가 존재해서 임시로 회피 로직 추가. mainnet 에서 돌릴시 삭제 필요
				fmt.Println("contract", address.Hex(), "slot migration failed. ignore", it.LeafKey())
				continue
			} else {
				panic(fmt.Errorf("contract %s migration failed. %w", address.Hex(), err))
			}
		}
		must(mpt.UpdateStorage(common.Address{}, slot, encodeToRlp(it.LeafBlob())))
		status.emitLog(false, "contract", address.Hex(), "index", common.BytesToHash(it.LeafKey()).Hex())
	}
	m.checkHashCollision(mpt)
	status.startDBCommit()
	return m.commit(mpt)
}

func (m *migrator) checkHashCollision(t *trie.StateTrie) {
	for it := t.MustNodeIterator(nil); it.Next(true); {
		if !it.Leaf() {
			continue
		}
		// 혹시라도 keccakhash 가 poseidon 와 충돌하는 경우, 그냥 write 할 경우 데이터 소실이 발생함.
		// 만약 충돌이 발생하면 그 때 해결책을 찾아볼것...
		data, _ := m.db.Get(it.Hash().Bytes())
		if len(data) == 0 {
			continue
		}
		if node, err := zk.NewTreeNodeFromBlob(data); err == nil {
			panic(fmt.Sprintf("Hash collision detected: %v %v", it.Hash().Hex(), node))
		}
	}
}

func (m *migrator) readPreimage(key []byte) ([]byte, error) {
	keyHash := *trie.IteratorKeyToHash(key, true)
	if addr, ok := m.genesisAccount[keyHash]; ok {
		return addr.Bytes(), nil
	}
	if slot, ok := m.genesisStorage[keyHash]; ok {
		return slot, nil
	}
	if preimage := m.zkdb.Preimage(keyHash); common.BytesToHash(zk.MustNewSecureHash(preimage).Bytes()).Hex() == keyHash.Hex() {
		return preimage, nil
	}
	return nil, fmt.Errorf("%v preimage does not exist", keyHash.Hex())
}

func (m *migrator) openZkIterator(root common.Hash) trie.NodeIterator {
	tr := must1(trie.NewZkMerkleStateTrie(root, m.zkdb))
	return must1(tr.NodeIterator(nil))
}

func (m *migrator) newMPT(id *trie.ID) *trie.StateTrie {
	return must1(trie.NewStateTrie(id, m.mptdb))
}

func (m *migrator) applyNewStateTransition(root migrationRoot) *migrationRoot {
	if headBlockNumber := m.blockChain.eth_blockNumber(); root.Number <= headBlockNumber {
		fmt.Println("migration start", root.Number, "head", headBlockNumber, "remaining", headBlockNumber-root.Number)
	} else {
		return nil
	}

	mpt := m.newMPT(trie.StateTrieID(root.Hash))
	m.blockChain.debug_traceBlockByNumber(root.Number, func(address common.Address, state map[string]any) {
		must(mpt.UpdateAccount(address, m.updateAccount(address, must1(mpt.GetAccount(address)), state, root.Hash)))
	})
	root.Hash = m.commit(mpt)
	root.Number += 1
	return &root
}

func (m *migrator) updateAccount(address common.Address, account *types.StateAccount, nextState map[string]any, stateRoot common.Hash) *types.StateAccount {
	if account == nil {
		account = types.NewEmptyStateAccount(false)
	}
	if balance, ok := nextState["balance"]; ok {
		balance, ok := new(big.Int).SetString(strings.TrimPrefix(balance.(string), "0x"), 16)
		if !ok {
			panic("")
		}
		account.Balance = balance
		delete(nextState, "balance")
	}
	if nonce, ok := nextState["nonce"]; ok {
		if f, ok := nonce.(float64); ok {
			account.Nonce = uint64(f)
		} else {
			panic(account)
		}
		delete(nextState, "nonce")
	}
	if storage, ok := nextState["storage"]; ok {
		mpt := m.newMPT(trie.StorageTrieID(stateRoot, crypto.Keccak256Hash(address.Bytes()), account.Root))
		for key, value := range storage.(map[string]any) {
			must(mpt.UpdateStorage(common.Address{}, common.HexToHash(key).Bytes(), encodeToRlp([]byte(value.(string)))))
		}
		account.Root = m.commit(mpt)
		delete(nextState, "storage")
	}
	if len(nextState) > 0 {
		panic(account)
	}
	return account
}

func encodeToRlp(bytes []byte) []byte {
	trimmed := common.TrimLeftZeroes(common.BytesToHash(bytes).Bytes())
	encoded, _ := rlp.EncodeToBytes(trimmed)
	return encoded
}

func (m *migrator) commit(mpt *trie.StateTrie) common.Hash {
	root, set := must2(mpt.Commit(true))
	must(m.mptdb.Update(root, types.EmptyRootHash, 0, trienode.NewWithNodeSet(set), nil))
	must(m.mptdb.Commit(root, false))
	return root
}

type migrationRoot struct {
	Hash   common.Hash `json:"hash"`
	Number uint64      `json:"number"`
}
