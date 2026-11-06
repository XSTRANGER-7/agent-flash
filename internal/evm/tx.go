package evm

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
)

// --- RLP -------------------------------------------------------------------

func rlpBytes(b []byte) []byte {
	if len(b) == 1 && b[0] < 0x80 {
		return b
	}
	return append(rlpHeader(0x80, 0xb7, len(b)), b...)
}

func rlpList(items ...[]byte) []byte {
	var payload []byte
	for _, it := range items {
		payload = append(payload, it...)
	}
	return append(rlpHeader(0xc0, 0xf7, len(payload)), payload...)
}

func rlpHeader(short, long byte, n int) []byte {
	if n <= 55 {
		return []byte{short + byte(n)}
	}
	lb := big.NewInt(int64(n)).Bytes()
	return append([]byte{long + byte(len(lb))}, lb...)
}

func rlpBig(n *big.Int) []byte {
	if n == nil || n.Sign() == 0 {
		return rlpBytes(nil)
	}
	return rlpBytes(n.Bytes())
}

// --- EIP-1559 transactions --------------------------------------------------

// Tx is an EIP-1559 (type 2) transaction.
type Tx struct {
	ChainID *big.Int
	Nonce   uint64
	TipCap  *big.Int // maxPriorityFeePerGas
	FeeCap  *big.Int // maxFeePerGas
	Gas     uint64
	To      string
	Value   *big.Int
	Data    []byte
}

func (t *Tx) fields() ([][]byte, error) {
	to, err := HexToAddressBytes(t.To)
	if err != nil {
		return nil, err
	}
	return [][]byte{
		rlpBig(t.ChainID),
		rlpBig(new(big.Int).SetUint64(t.Nonce)),
		rlpBig(t.TipCap),
		rlpBig(t.FeeCap),
		rlpBig(new(big.Int).SetUint64(t.Gas)),
		rlpBytes(to),
		rlpBig(t.Value),
		rlpBytes(t.Data),
		rlpList(), // empty access list
	}, nil
}

// SignTx signs and returns the raw 0x-prefixed transaction and its hash.
func SignTx(t *Tx, key *PrivateKey) (raw string, hash string, err error) {
	if t.ChainID == nil {
		return "", "", errors.New("chain id required")
	}
	f, err := t.fields()
	if err != nil {
		return "", "", err
	}
	sighash := Keccak256([]byte{0x02}, rlpList(f...))
	sig, err := key.Sign(sighash)
	if err != nil {
		return "", "", err
	}
	yParity := big.NewInt(int64(sig[64] - 27))
	r := new(big.Int).SetBytes(sig[0:32])
	s := new(big.Int).SetBytes(sig[32:64])
	signed := append(f, rlpBig(yParity), rlpBig(r), rlpBig(s))
	var buf bytes.Buffer
	buf.WriteByte(0x02)
	buf.Write(rlpList(signed...))
	rawBytes := buf.Bytes()
	return "0x" + hex.EncodeToString(rawBytes), "0x" + hex.EncodeToString(Keccak256(rawBytes)), nil
}

// --- ERC-20 approve helpers -------------------------------------------------

var approveSelector = []byte{0x09, 0x5e, 0xa7, 0xb3}

// ParseApprove decodes approve(spender, amount) calldata.
func ParseApprove(data []byte) (spender string, amount *big.Int, ok bool) {
	if len(data) != 4+64 || !bytes.Equal(data[:4], approveSelector) {
		return "", nil, false
	}
	return ChecksumAddress(data[16:36]), new(big.Int).SetBytes(data[36:68]), true
}

// EncodeApprove builds approve(spender, amount) calldata.
func EncodeApprove(spender string, amount *big.Int) ([]byte, error) {
	sp, err := HexToAddressBytes(spender)
	if err != nil {
		return nil, err
	}
	out := append([]byte{}, approveSelector...)
	out = append(out, pad32(sp)...)
	out = append(out, pad32(amount.Bytes())...)
	return out, nil
}

// DecodeHex decodes a 0x hex string.
func DecodeHex(s string) ([]byte, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "0x")
	if len(s)%2 == 1 {
		s = "0" + s
	}
	return hex.DecodeString(s)
}

var (
	transferSelector  = []byte{0xa9, 0x05, 0x9c, 0xbb} // transfer(address,uint256)
	allowanceSelector = []byte{0xdd, 0x62, 0xed, 0x3e} // allowance(address,address)
)

// EncodeTransfer builds ERC-20 transfer(to, amount) calldata.
func EncodeTransfer(to string, amount *big.Int) ([]byte, error) {
	a, err := HexToAddressBytes(to)
	if err != nil {
		return nil, err
	}
	out := append([]byte{}, transferSelector...)
	out = append(out, pad32(a)...)
	return append(out, pad32(amount.Bytes())...), nil
}

// EncodeAllowance builds ERC-20 allowance(owner, spender) calldata (eth_call).
func EncodeAllowance(owner, spender string) ([]byte, error) {
	o, err := HexToAddressBytes(owner)
	if err != nil {
		return nil, err
	}
	s, err := HexToAddressBytes(spender)
	if err != nil {
		return nil, err
	}
	out := append([]byte{}, allowanceSelector...)
	out = append(out, pad32(o)...)
	return append(out, pad32(s)...), nil
}
