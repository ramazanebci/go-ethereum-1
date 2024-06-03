package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/rpc"
	"io"
	"net/http"
)

type BlockChain interface {
	eth_blockNumber() uint64
	debug_traceBlockByNumber(blockNumber uint64, callback func(address common.Address, state map[string]any))
	eth_getProof(blockNumber uint64, address string) *AccountResult
}

type node struct {
	chainApi *ethapi.BlockChainAPI
	traceApi *tracers.API
}

func (n *node) eth_blockNumber() uint64 { return uint64(n.chainApi.BlockNumber()) }

func (n *node) debug_traceBlockByNumber(blockNumber uint64, callback func(address common.Address, state map[string]any)) {
	tracer := "prestateTracer"
	result := must1(n.traceApi.TraceBlockByNumber(context.Background(), rpc.BlockNumber(blockNumber), &tracers.TraceConfig{
		LogConfig:    nil,
		Tracer:       &tracer,
		Timeout:      nil,
		Reexec:       nil,
		TracerConfig: must1(json.Marshal(`{"diffMode": true}`)),
	}))
	for _, tx := range result {
		txPostState := tx.Result.(map[string]any)["post"].(map[string]any)
		for address, state := range txPostState {
			callback(common.HexToAddress(address), state.(map[string]any))
		}
	}
}

func (n *node) eth_getProof(blockNumber uint64, address string) *AccountResult {
	//TODO implement me
	panic("implement me")
}

type httpClient struct{ rpc string }

func (h *httpClient) eth_blockNumber() uint64 {
	return must1(send[hexutil.Big](h.rpc, "eth_blockNumber", []any{})).ToInt().Uint64()
}

func (h *httpClient) eth_getProof(blockNumber uint64, address string) *AccountResult {
	return must1(send[AccountResult](h.rpc, "eth_getProof", []any{address, []any{}, fmt.Sprintf("0x%x", blockNumber)}))
}

func (h *httpClient) debug_traceBlockByNumber(blockNumber uint64, callback func(address common.Address, state map[string]any)) {
	res := must1(http.Post(h.rpc, "application/json", bytes.NewReader([]byte(fmt.Sprintf(`
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "debug_traceBlockByNumber",
  "params": [
    "%v",
    {
      "tracer": "prestateTracer",
      "tracerConfig": {"diffMode": true}
    }
  ]
}
`, fmt.Sprintf("0x%x", blockNumber))))))
	result := make(map[string]any)
	must(json.Unmarshal(must1(io.ReadAll(res.Body)), &result))
	for _, tx := range result["result"].([]any) {
		txPostState := tx.(map[string]any)["result"].(map[string]any)["post"].(map[string]any)
		for address, state := range txPostState {
			callback(common.HexToAddress(address), state.(map[string]any))
		}
	}
}

func send[T any](address string, method string, params any) (*T, error) {
	jsonBytes := must1(json.Marshal(&request{"2.0", method, params, "0"}))
	httpResponse := must1(http.Post(address, "application/json", bytes.NewReader(jsonBytes)))
	var response response[T]
	must(json.Unmarshal(must1(io.ReadAll(httpResponse.Body)), &response))
	if response.Error != nil {
		return nil, response.Error
	}
	return response.Result, nil
}

type request struct {
	Jsonrpc string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
	Id      string `json:"id"`
}

type response[T any] struct {
	Jsonrpc string        `json:"jsonrpc"`
	Result  *T            `json:"result"`
	Error   *JsonRpcError `json:"error"`
	Id      string        `json:"id"`
}

type JsonRpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

func (j *JsonRpcError) Error() string { return fmt.Sprintf("[%d] %s", j.Code, j.Message) }

// AccountResult is the result of a GetProof operation.
type AccountResult struct {
	Address      common.Address  `json:"address"`
	AccountProof []string        `json:"accountProof"`
	Balance      *hexutil.Big    `json:"balance"`
	CodeHash     common.Hash     `json:"codeHash"`
	Nonce        *hexutil.Uint64 `json:"nonce"`
	StorageHash  common.Hash     `json:"storageHash"`
	StorageProof []StorageResult `json:"storageProof"`
}

// StorageResult provides a proof for a key-value pair.
type StorageResult struct {
	Key   string       `json:"key"`
	Value *hexutil.Big `json:"value"`
	Proof []string     `json:"proof"`
}
