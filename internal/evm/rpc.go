package evm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// RPC is a minimal Ethereum JSON-RPC client (Base mainnet by default).
type RPC struct {
	url  string
	http *http.Client
	id   atomic.Int64
}

func NewRPC(url string) *RPC {
	return &RPC{url: url, http: &http.Client{Timeout: 20 * time.Second}}
}

type rpcResp struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *RPC) call(ctx context.Context, out any, method string, params ...any) error {
	if params == nil {
		params = []any{}
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id.Add(1), "method": method, "params": params})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var r rpcResp
	if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
		return fmt.Errorf("%s: decode: %w", method, err)
	}
	if r.Error != nil {
		return fmt.Errorf("%s: rpc error %d: %s", method, r.Error.Code, r.Error.Message)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func hexBig(s string) *big.Int {
	n, _ := new(big.Int).SetString(strings.TrimPrefix(s, "0x"), 16)
	if n == nil {
		return big.NewInt(0)
	}
	return n
}

func toHex(n *big.Int) string { return "0x" + n.Text(16) }

// Call is eth_call against the latest block and returns the raw result.
func (c *RPC) Call(ctx context.Context, to string, data []byte) ([]byte, error) {
	var s string
	msg := map[string]string{"to": to, "data": "0x" + fmt.Sprintf("%x", data)}
	if err := c.call(ctx, &s, "eth_call", msg, "latest"); err != nil {
		return nil, err
	}
	return DecodeHex(s)
}

// ChainID is eth_chainId.
func (c *RPC) ChainID(ctx context.Context) (int64, error) {
	var s string
	if err := c.call(ctx, &s, "eth_chainId"); err != nil {
		return 0, err
	}
	return hexBig(s).Int64(), nil
}

func (c *RPC) Nonce(ctx context.Context, addr string) (uint64, error) {
	var s string
	if err := c.call(ctx, &s, "eth_getTransactionCount", addr, "pending"); err != nil {
		return 0, err
	}
	return hexBig(s).Uint64(), nil
}

func (c *RPC) ETHBalance(ctx context.Context, addr string) (*big.Int, error) {
	var s string
	if err := c.call(ctx, &s, "eth_getBalance", addr, "latest"); err != nil {
		return nil, err
	}
	return hexBig(s), nil
}

// GasPrice is eth_gasPrice in wei.
func (c *RPC) GasPrice(ctx context.Context) (*big.Int, error) {
	var s string
	if err := c.call(ctx, &s, "eth_gasPrice"); err != nil {
		return nil, err
	}
	return hexBig(s), nil
}

// BlockNumber is eth_blockNumber.
func (c *RPC) BlockNumber(ctx context.Context) (uint64, error) {
	var s string
	if err := c.call(ctx, &s, "eth_blockNumber"); err != nil {
		return 0, err
	}
	return hexBig(s).Uint64(), nil
}

// Fees returns (tipCap, feeCap) using 2*baseFee + tip.
func (c *RPC) Fees(ctx context.Context) (*big.Int, *big.Int, error) {
	var tipS string
	if err := c.call(ctx, &tipS, "eth_maxPriorityFeePerGas"); err != nil {
		tipS = "0x186a0" // 100k wei fallback, Base tips are tiny
	}
	var blk struct {
		BaseFee string `json:"baseFeePerGas"`
	}
	if err := c.call(ctx, &blk, "eth_getBlockByNumber", "latest", false); err != nil {
		return nil, nil, err
	}
	tip := hexBig(tipS)
	fee := new(big.Int).Mul(hexBig(blk.BaseFee), big.NewInt(2))
	fee.Add(fee, tip)
	return tip, fee, nil
}

func (c *RPC) EstimateGas(ctx context.Context, from, to string, data []byte, value *big.Int) (uint64, error) {
	if value == nil {
		value = big.NewInt(0)
	}
	var s string
	msg := map[string]string{"from": from, "to": to, "data": "0x" + fmt.Sprintf("%x", data), "value": toHex(value)}
	if err := c.call(ctx, &s, "eth_estimateGas", msg); err != nil {
		return 0, err
	}
	return hexBig(s).Uint64(), nil
}

func (c *RPC) SendRaw(ctx context.Context, raw string) (string, error) {
	var h string
	err := c.call(ctx, &h, "eth_sendRawTransaction", raw)
	return h, err
}

// WaitReceipt polls until the tx is mined; returns error if it reverted.
func (c *RPC) WaitReceipt(ctx context.Context, hash string) error {
	t := time.NewTicker(1500 * time.Millisecond)
	defer t.Stop()
	for {
		var rc *struct {
			Status string `json:"status"`
		}
		if err := c.call(ctx, &rc, "eth_getTransactionReceipt", hash); err == nil && rc != nil {
			if rc.Status == "0x1" {
				return nil
			}
			return errors.New("transaction reverted: " + hash)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// SendTx fills nonce/gas/fees, signs and broadcasts, then waits for success.
func (c *RPC) SendTx(ctx context.Context, chainID *big.Int, key *PrivateKey, to string, data []byte, value *big.Int) (string, error) {
	from := key.Address()
	nonce, err := c.Nonce(ctx, from)
	if err != nil {
		return "", err
	}
	gas, err := c.EstimateGas(ctx, from, to, data, value)
	if err != nil {
		return "", fmt.Errorf("estimate gas: %w", err)
	}
	tip, fee, err := c.Fees(ctx)
	if err != nil {
		return "", err
	}
	if value == nil {
		value = big.NewInt(0)
	}
	raw, _, err := SignTx(&Tx{ChainID: chainID, Nonce: nonce, TipCap: tip, FeeCap: fee, Gas: gas * 12 / 10, To: to, Value: value, Data: data}, key)
	if err != nil {
		return "", err
	}
	h, err := c.SendRaw(ctx, raw)
	if err != nil {
		return "", err
	}
	return h, c.WaitReceipt(ctx, h)
}

// CallAs is eth_call from a given sender with an optional state override
// (geth's third eth_call parameter: {address: {balance, stateDiff}}). It runs
// the real contract code on the latest real state, changes nothing onchain,
// and is how the simulation suite gives a wallet test money.
func (c *RPC) CallAs(ctx context.Context, from, to string, data []byte, override map[string]any) ([]byte, error) {
	var s string
	msg := map[string]string{"to": to, "data": "0x" + fmt.Sprintf("%x", data)}
	if from != "" {
		msg["from"] = from
	}
	params := []any{msg, "latest"}
	if override != nil {
		params = append(params, override)
	}
	if err := c.call(ctx, &s, "eth_call", params...); err != nil {
		return nil, err
	}
	return DecodeHex(s)
}
