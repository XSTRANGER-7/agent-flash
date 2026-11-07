package evm

import (
	"errors"
	"math/big"
)

// rlpItem is a decoded RLP value: either a byte string or a list.
type rlpItem struct {
	str    []byte
	list   []rlpItem
	isList bool
}

func rlpDecode(b []byte) (rlpItem, []byte, error) {
	if len(b) == 0 {
		return rlpItem{}, nil, errors.New("rlp: empty input")
	}
	p := b[0]
	readLen := func(n int) (int, error) {
		if n == 0 || n > 4 || len(b) < 1+n {
			return 0, errors.New("rlp: bad length prefix")
		}
		l := 0
		for _, c := range b[1 : 1+n] {
			l = l<<8 | int(c)
		}
		return l, nil
	}
	switch {
	case p < 0x80:
		return rlpItem{str: b[:1]}, b[1:], nil
	case p <= 0xb7:
		l := int(p - 0x80)
		if len(b) < 1+l {
			return rlpItem{}, nil, errors.New("rlp: short string")
		}
		return rlpItem{str: b[1 : 1+l]}, b[1+l:], nil
	case p <= 0xbf:
		n := int(p - 0xb7)
		l, err := readLen(n)
		if err != nil || len(b) < 1+n+l {
			return rlpItem{}, nil, errors.New("rlp: short long string")
		}
		return rlpItem{str: b[1+n : 1+n+l]}, b[1+n+l:], nil
	default:
		var body, rest []byte
		if p <= 0xf7 {
			l := int(p - 0xc0)
			if len(b) < 1+l {
				return rlpItem{}, nil, errors.New("rlp: short list")
			}
			body, rest = b[1:1+l], b[1+l:]
		} else {
			n := int(p - 0xf7)
			l, err := readLen(n)
			if err != nil || len(b) < 1+n+l {
				return rlpItem{}, nil, errors.New("rlp: short long list")
			}
			body, rest = b[1+n:1+n+l], b[1+n+l:]
		}
		it := rlpItem{isList: true}
		for len(body) > 0 {
			var child rlpItem
			var err error
			child, body, err = rlpDecode(body)
			if err != nil {
				return rlpItem{}, nil, err
			}
			it.list = append(it.list, child)
		}
		return it, rest, nil
	}
}

// DecodedTx is the part of an EIP-1559 transaction we inspect.
type DecodedTx struct {
	ChainID *big.Int
	Nonce   uint64
	To      string // checksummed, "" for contract creation
	Value   *big.Int
	Data    []byte
}

// DecodeDynamicFeeTx decodes a (signed or unsigned) 0x02 typed transaction:
// 0x02 || rlp([chainId, nonce, tip, feeCap, gas, to, value, data, accessList, ...]).
func DecodeDynamicFeeTx(raw []byte) (*DecodedTx, error) {
	if len(raw) < 2 || raw[0] != 0x02 {
		return nil, errors.New("not an EIP-1559 (0x02) transaction")
	}
	it, rest, err := rlpDecode(raw[1:])
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 || !it.isList || len(it.list) < 9 {
		return nil, errors.New("malformed EIP-1559 transaction")
	}
	f := it.list
	for i := 0; i < 8; i++ {
		if f[i].isList {
			return nil, errors.New("malformed EIP-1559 transaction field")
		}
	}
	tx := &DecodedTx{
		ChainID: new(big.Int).SetBytes(f[0].str),
		Nonce:   new(big.Int).SetBytes(f[1].str).Uint64(),
		Value:   new(big.Int).SetBytes(f[6].str),
		Data:    f[7].str,
	}
	switch len(f[5].str) {
	case 0:
	case 20:
		tx.To = ChecksumAddress(f[5].str)
	default:
		return nil, errors.New("malformed tx recipient")
	}
	return tx, nil
}

// DecodeLegacyTx decodes a (signed or unsigned) legacy transaction:
// rlp([nonce, gasPrice, gas, to, value, data, v, r, s]). ChainID is set only
// when v carries it (EIP-155: unsigned v = chainId, signed v = 35 + 2*id or
// 36 + 2*id); Flash's unsigned BNB Chain setup txs leave v empty (nil here).
func DecodeLegacyTx(raw []byte) (*DecodedTx, error) {
	if len(raw) == 0 || raw[0] < 0xc0 {
		return nil, errors.New("not a legacy transaction")
	}
	it, rest, err := rlpDecode(raw)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 || !it.isList || (len(it.list) != 6 && len(it.list) != 9) {
		return nil, errors.New("malformed legacy transaction")
	}
	f := it.list
	for _, x := range f {
		if x.isList {
			return nil, errors.New("malformed legacy transaction field")
		}
	}
	tx := &DecodedTx{
		Nonce: new(big.Int).SetBytes(f[0].str).Uint64(),
		Value: new(big.Int).SetBytes(f[4].str),
		Data:  f[5].str,
	}
	switch len(f[3].str) {
	case 0:
	case 20:
		tx.To = ChecksumAddress(f[3].str)
	default:
		return nil, errors.New("malformed tx recipient")
	}
	if len(f) == 9 && len(f[6].str) > 0 {
		v := new(big.Int).SetBytes(f[6].str)
		switch {
		case len(f[7].str) == 0 && len(f[8].str) == 0: // unsigned EIP-155: v = chainId
			tx.ChainID = v
		case v.Cmp(big.NewInt(35)) >= 0: // signed EIP-155
			tx.ChainID = new(big.Int).Div(new(big.Int).Sub(v, big.NewInt(35)), big.NewInt(2))
		}
	}
	return tx, nil
}

// DecodeAnyTx decodes an EIP-1559 or a legacy transaction.
func DecodeAnyTx(raw []byte) (*DecodedTx, error) {
	if len(raw) > 0 && raw[0] == 0x02 {
		return DecodeDynamicFeeTx(raw)
	}
	return DecodeLegacyTx(raw)
}
