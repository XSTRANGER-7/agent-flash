package evm

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

// TypedField is one member of an EIP-712 struct type.
type TypedField struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// TypedData is an EIP-712 payload, as returned by Flash in
// evm.orderTypedData / evm.permitTypedData (JSON strings).
type TypedData struct {
	Types       map[string][]TypedField `json:"types"`
	PrimaryType string                  `json:"primaryType"`
	Domain      map[string]any          `json:"domain"`
	Message     map[string]any          `json:"message"`
}

// ParseTypedData decodes a JSON typed-data string, keeping big numbers exact.
func ParseTypedData(raw string) (*TypedData, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var td TypedData
	if err := dec.Decode(&td); err != nil {
		return nil, fmt.Errorf("typed data: %w", err)
	}
	if td.PrimaryType == "" || td.Types == nil {
		return nil, errors.New("typed data: missing primaryType or types")
	}
	return &td, nil
}

// Digest returns keccak256(0x19 0x01 || domainSeparator || hashStruct(message)).
func (td *TypedData) Digest() ([]byte, error) {
	ds, err := td.DomainSeparator()
	if err != nil {
		return nil, err
	}
	hs, err := td.HashStruct(td.PrimaryType, td.Message)
	if err != nil {
		return nil, err
	}
	return Keccak256([]byte{0x19, 0x01}, ds, hs), nil
}

func (td *TypedData) domainFields() []TypedField {
	if f, ok := td.Types["EIP712Domain"]; ok {
		return f
	}
	// Derive from the keys present, in canonical order.
	var out []TypedField
	order := []TypedField{{"name", "string"}, {"version", "string"}, {"chainId", "uint256"}, {"verifyingContract", "address"}, {"salt", "bytes32"}}
	for _, f := range order {
		if _, ok := td.Domain[f.Name]; ok {
			out = append(out, f)
		}
	}
	return out
}

// DomainSeparator hashes the EIP712Domain struct.
func (td *TypedData) DomainSeparator() ([]byte, error) {
	types := map[string][]TypedField{}
	for k, v := range td.Types {
		types[k] = v
	}
	types["EIP712Domain"] = td.domainFields()
	tmp := &TypedData{Types: types}
	return tmp.HashStruct("EIP712Domain", td.Domain)
}

// HashStruct = keccak256(typeHash || encodeData).
func (td *TypedData) HashStruct(name string, data map[string]any) ([]byte, error) {
	enc, err := td.encodeData(name, data)
	if err != nil {
		return nil, err
	}
	return Keccak256(enc), nil
}

// EncodeType returns e.g. "Mail(Person from,Person to,string contents)Person(string name,address wallet)".
func (td *TypedData) EncodeType(name string) string {
	deps := map[string]bool{}
	td.findDeps(name, deps)
	delete(deps, name)
	sorted := make([]string, 0, len(deps))
	for d := range deps {
		sorted = append(sorted, d)
	}
	sort.Strings(sorted)
	var b strings.Builder
	for _, t := range append([]string{name}, sorted...) {
		b.WriteString(t)
		b.WriteByte('(')
		for i, f := range td.Types[t] {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(f.Type)
			b.WriteByte(' ')
			b.WriteString(f.Name)
		}
		b.WriteByte(')')
	}
	return b.String()
}

func baseType(t string) string {
	if i := strings.Index(t, "["); i >= 0 {
		return t[:i]
	}
	return t
}

func (td *TypedData) findDeps(name string, seen map[string]bool) {
	if seen[name] {
		return
	}
	if _, ok := td.Types[name]; !ok {
		return
	}
	seen[name] = true
	for _, f := range td.Types[name] {
		td.findDeps(baseType(f.Type), seen)
	}
}

func (td *TypedData) encodeData(name string, data map[string]any) ([]byte, error) {
	fields, ok := td.Types[name]
	if !ok {
		return nil, fmt.Errorf("unknown type %s", name)
	}
	var buf bytes.Buffer
	buf.Write(Keccak256([]byte(td.EncodeType(name))))
	for _, f := range fields {
		v, err := td.encodeValue(f.Type, data[f.Name])
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", name, f.Name, err)
		}
		buf.Write(v)
	}
	return buf.Bytes(), nil
}

func (td *TypedData) encodeValue(typ string, v any) ([]byte, error) {
	// arrays
	if strings.HasSuffix(typ, "]") {
		inner := typ[:strings.LastIndex(typ, "[")]
		items, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("expected array for %s", typ)
		}
		var buf bytes.Buffer
		for _, it := range items {
			e, err := td.encodeValue(inner, it)
			if err != nil {
				return nil, err
			}
			buf.Write(e)
		}
		return Keccak256(buf.Bytes()), nil
	}
	if _, isStruct := td.Types[typ]; isStruct {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("expected object for %s", typ)
		}
		return td.HashStruct(typ, m)
	}
	switch {
	case typ == "string":
		s, _ := v.(string)
		return Keccak256([]byte(s)), nil
	case typ == "bytes":
		b, err := decodeHexValue(v)
		if err != nil {
			return nil, err
		}
		return Keccak256(b), nil
	case typ == "bool":
		out := make([]byte, 32)
		switch x := v.(type) {
		case bool:
			if x {
				out[31] = 1
			}
		case string:
			if x == "true" {
				out[31] = 1
			}
		}
		return out, nil
	case typ == "address":
		s, _ := v.(string)
		b, err := HexToAddressBytes(s)
		if err != nil {
			return nil, err
		}
		return pad32(b), nil
	case strings.HasPrefix(typ, "bytes"):
		b, err := decodeHexValue(v)
		if err != nil {
			return nil, err
		}
		out := make([]byte, 32)
		copy(out, b)
		return out, nil
	case strings.HasPrefix(typ, "uint"), strings.HasPrefix(typ, "int"):
		n, err := toBigInt(v)
		if err != nil {
			return nil, err
		}
		if n.Sign() < 0 {
			n = new(big.Int).Add(n, new(big.Int).Lsh(big.NewInt(1), 256))
		}
		return pad32(n.Bytes()), nil
	}
	return nil, fmt.Errorf("unsupported type %s", typ)
}

func decodeHexValue(v any) ([]byte, error) {
	s, _ := v.(string)
	s = strings.TrimPrefix(s, "0x")
	if len(s)%2 == 1 {
		s = "0" + s
	}
	return hex.DecodeString(s)
}

// ToBigInt parses JSON numbers, decimal or 0x-hex strings.
func ToBigInt(v any) (*big.Int, error) { return toBigInt(v) }

func toBigInt(v any) (*big.Int, error) {
	switch x := v.(type) {
	case json.Number:
		return parseBig(string(x))
	case string:
		return parseBig(x)
	case float64:
		return big.NewInt(int64(x)), nil
	case int:
		return big.NewInt(int64(x)), nil
	case int64:
		return big.NewInt(x), nil
	case bool:
		if x {
			return big.NewInt(1), nil
		}
		return big.NewInt(0), nil
	case nil:
		return big.NewInt(0), nil
	}
	return nil, fmt.Errorf("cannot parse integer from %T", v)
}

func parseBig(s string) (*big.Int, error) {
	s = strings.TrimSpace(s)
	n := new(big.Int)
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		if _, ok := n.SetString(s[2:], 16); !ok {
			return nil, fmt.Errorf("bad hex integer %q", s)
		}
		return n, nil
	}
	if _, ok := n.SetString(s, 10); !ok {
		// allow scientific notation like 1e18 from loose JSON
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			bf := new(big.Float).SetFloat64(f)
			n, _ = bf.Int(nil)
			return n, nil
		}
		return nil, fmt.Errorf("bad integer %q", s)
	}
	return n, nil
}

// PersonalMessageHash implements EIP-191 personal_sign hashing.
func PersonalMessageHash(msg []byte) []byte {
	prefix := fmt.Sprintf("\x19Ethereum Signed Message:\n%d", len(msg))
	return Keccak256([]byte(prefix), msg)
}

// SigHex encodes a signature as 0x-prefixed hex.
func SigHex(sig []byte) string { return "0x" + hex.EncodeToString(sig) }
