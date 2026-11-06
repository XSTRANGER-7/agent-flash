package evm

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"
)

func TestRecoverAddressRoundTrip(t *testing.T) {
	for i := 0; i < 8; i++ {
		k, _, err := GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		digest := Keccak256([]byte{byte(i), 1, 2, 3})
		sig, err := k.Sign(digest)
		if err != nil {
			t.Fatal(err)
		}
		got, err := RecoverAddress(digest, sig)
		if err != nil || got != k.Address() {
			t.Fatalf("recover: got %s err %v want %s", got, err, k.Address())
		}
		bad := append([]byte{}, sig...)
		bad[10] ^= 1
		if a, err := RecoverAddress(digest, bad); err == nil && a == k.Address() {
			t.Fatal("tampered signature recovered the signer")
		}
	}
}

// Known vector: the private key 0x...01 signs; its address is fixed.
func TestRecoverKnownKey(t *testing.T) {
	k, err := NewPrivateKey("0x" + strings.Repeat("0", 63) + "1")
	if err != nil {
		t.Fatal(err)
	}
	if k.Address() != "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf" {
		t.Fatalf("address %s", k.Address())
	}
	d := PersonalMessageHash([]byte("hello"))
	sig, _ := k.Sign(d)
	got, err := RecoverAddress(d, sig)
	if err != nil || got != k.Address() {
		t.Fatalf("got %s %v (sig %s)", got, err, hex.EncodeToString(sig))
	}
}

func TestTransferAndAllowanceCalldata(t *testing.T) {
	b, _ := EncodeTransfer("0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf", func() *big.Int { n, _ := parseBig("1000000"); return n }())
	if hex.EncodeToString(b[:4]) != "a9059cbb" || len(b) != 68 {
		t.Fatalf("transfer calldata %x", b)
	}
	a, _ := EncodeAllowance("0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf", "0x5d00000873b6BF41539e6f5365B0Ff7d3c368f78")
	if hex.EncodeToString(a[:4]) != "dd62ed3e" || len(a) != 68 {
		t.Fatalf("allowance calldata %x", a)
	}
}
