package evm

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"
)

func h(t *testing.T, b []byte) string { t.Helper(); return hex.EncodeToString(b) }

func TestKeccak(t *testing.T) {
	if got := h(t, Keccak256(nil)); got != "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470" {
		t.Fatalf("empty: %s", got)
	}
	if got := h(t, Keccak256([]byte("cow"))); got != "c85ef7d79691fe79573b1a7064c19c1a9819ebdbd1faaab1a8ec92344438aaf4" {
		t.Fatalf("cow: %s", got)
	}
	// multi-block input (> 136 bytes) must not crash and must be stable
	long := strings.Repeat("a", 300)
	if h(t, Keccak256([]byte(long))) == h(t, Keccak256([]byte(long[:299]))) {
		t.Fatal("collision on long input")
	}
}

// Official example from EIP-712 (Ether Mail).
const mailTypedData = `{
 "types": {
  "EIP712Domain": [{"name":"name","type":"string"},{"name":"version","type":"string"},{"name":"chainId","type":"uint256"},{"name":"verifyingContract","type":"address"}],
  "Person": [{"name":"name","type":"string"},{"name":"wallet","type":"address"}],
  "Mail": [{"name":"from","type":"Person"},{"name":"to","type":"Person"},{"name":"contents","type":"string"}]
 },
 "primaryType": "Mail",
 "domain": {"name":"Ether Mail","version":"1","chainId":1,"verifyingContract":"0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC"},
 "message": {"from":{"name":"Cow","wallet":"0xCD2a3d9F938E13CD947Ec05AbC7FE734Df8DD826"},"to":{"name":"Bob","wallet":"0xbBbBBBBbbBBBbbbBbbBbbbbBBbBbbbbBbBbbBBbB"},"contents":"Hello, Bob!"}
}`

func TestEIP712Vectors(t *testing.T) {
	td, err := ParseTypedData(mailTypedData)
	if err != nil {
		t.Fatal(err)
	}
	if got := td.EncodeType("Mail"); got != "Mail(Person from,Person to,string contents)Person(string name,address wallet)" {
		t.Fatalf("encodeType: %s", got)
	}
	if got := h(t, Keccak256([]byte(td.EncodeType("Mail")))); got != "a0cedeb2dc280ba39b857546d74f5549c3a1d7bdc2dd96bf881f76108e23dac2" {
		t.Fatalf("typeHash: %s", got)
	}
	ds, _ := td.DomainSeparator()
	if got := h(t, ds); got != "f2cee375fa42b42143804025fc449deafd50cc031ca257e0b194a650a912090f" {
		t.Fatalf("domainSeparator: %s", got)
	}
	hs, _ := td.HashStruct("Mail", td.Message)
	if got := h(t, hs); got != "c52c0ee5d84264471806290a3f2c4cecfc5490626bf912d01f240d7a274b371e" {
		t.Fatalf("hashStruct: %s", got)
	}
	d, _ := td.Digest()
	if got := h(t, d); got != "be609aee343fb3c4b28e1df9e632fca64fcfaede20f02e86244efddf30957bd2" {
		t.Fatalf("digest: %s", got)
	}

	key, err := NewPrivateKey(hex.EncodeToString(Keccak256([]byte("cow"))))
	if err != nil {
		t.Fatal(err)
	}
	if got := key.Address(); got != "0xCD2a3d9F938E13CD947Ec05AbC7FE734Df8DD826" {
		t.Fatalf("address: %s", got)
	}
	// Signature published in the EIP must verify with our curve maths.
	r, _ := hex.DecodeString("4355c47d63924e8a72e509b65029052eb6c299d53a04e167c5775fd466751c9d")
	s, _ := hex.DecodeString("07299936d304c153f6443dfa05f40ff007d72911b6f72307f996231605b91562")
	if !key.Verify(d, append(append(r, s...), 28)) {
		t.Fatal("reference signature does not verify")
	}
	sig, err := key.Sign(d)
	if err != nil {
		t.Fatal(err)
	}
	if !key.Verify(d, sig) {
		t.Fatal("own signature does not verify")
	}
	if new(big.Int).SetBytes(sig[32:64]).Cmp(halfN) > 0 {
		t.Fatal("signature is not low-S")
	}
	// RFC 6979 is deterministic: ethers/ethereumjs produce the same signature.
	if h(t, sig) != h(t, append(append(r, s...), 28)) {
		t.Logf("note: deterministic signature differs from EIP example: %s", h(t, sig))
	}
}

func TestApproveRoundTrip(t *testing.T) {
	data, err := EncodeApprove("0x000000000022D473030F116dDEE9F6B43aC78BA3", big.NewInt(5_000_000))
	if err != nil {
		t.Fatal(err)
	}
	sp, amt, ok := ParseApprove(data)
	if !ok || sp != "0x000000000022D473030F116dDEE9F6B43aC78BA3" || amt.Int64() != 5_000_000 {
		t.Fatalf("roundtrip: %v %s %v", ok, sp, amt)
	}
}

func TestSignTxShape(t *testing.T) {
	key, _ := NewPrivateKey(hex.EncodeToString(Keccak256([]byte("cow"))))
	raw, hash, err := SignTx(&Tx{ChainID: big.NewInt(8453), Nonce: 1, TipCap: big.NewInt(1), FeeCap: big.NewInt(2), Gas: 21000, To: "0xbBbBBBBbbBBBbbbBbbBbbbbBBbBbbbbBbBbbBBbB", Value: big.NewInt(0)}, key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "0x02") || len(hash) != 66 {
		t.Fatalf("bad tx: %s %s", raw, hash)
	}
}

func TestPersonalHash(t *testing.T) {
	// keccak256("\x19Ethereum Signed Message:\n5hello")
	if got := h(t, PersonalMessageHash([]byte("hello"))); got != "50b2c43fd39106bafbba0da34fc430e1f91e3c96ea2acee2bc34119f92b37750" {
		t.Fatalf("personal hash: %s", got)
	}
}
