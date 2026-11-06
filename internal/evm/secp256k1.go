package evm

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

var (
	curveP, _  = new(big.Int).SetString("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEFFFFFC2F", 16)
	curveN, _  = new(big.Int).SetString("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141", 16)
	curveGx, _ = new(big.Int).SetString("79BE667EF9DCBBAC55A06295CE870B07029BFCDB2DCE28D959F2815B16F81798", 16)
	curveGy, _ = new(big.Int).SetString("483ADA7726A3C4655DA4FBFC0E1108A8FD17B448A68554199C47D08FFB10D4B8", 16)
	halfN      = new(big.Int).Rsh(curveN, 1)
)

type point struct {
	x, y *big.Int
	inf  bool
}

func mod(a, m *big.Int) *big.Int {
	r := new(big.Int).Mod(a, m)
	return r
}

func pointAdd(p, q point) point {
	if p.inf {
		return q
	}
	if q.inf {
		return p
	}
	if p.x.Cmp(q.x) == 0 {
		if p.y.Cmp(q.y) == 0 && p.y.Sign() != 0 {
			return pointDouble(p)
		}
		return point{inf: true}
	}
	num := new(big.Int).Sub(q.y, p.y)
	den := new(big.Int).Sub(q.x, p.x)
	den.ModInverse(mod(den, curveP), curveP)
	l := mod(num.Mul(num, den), curveP)
	x3 := new(big.Int).Mul(l, l)
	x3.Sub(x3, p.x).Sub(x3, q.x)
	x3 = mod(x3, curveP)
	y3 := new(big.Int).Sub(p.x, x3)
	y3.Mul(y3, l).Sub(y3, p.y)
	y3 = mod(y3, curveP)
	return point{x: x3, y: y3}
}

func pointDouble(p point) point {
	if p.inf || p.y.Sign() == 0 {
		return point{inf: true}
	}
	num := new(big.Int).Mul(p.x, p.x)
	num.Mul(num, big.NewInt(3))
	den := new(big.Int).Lsh(p.y, 1)
	den.ModInverse(mod(den, curveP), curveP)
	l := mod(num.Mul(num, den), curveP)
	x3 := new(big.Int).Mul(l, l)
	x3.Sub(x3, new(big.Int).Lsh(p.x, 1))
	x3 = mod(x3, curveP)
	y3 := new(big.Int).Sub(p.x, x3)
	y3.Mul(y3, l).Sub(y3, p.y)
	y3 = mod(y3, curveP)
	return point{x: x3, y: y3}
}

func scalarMult(p point, k *big.Int) point {
	r := point{inf: true}
	for i := k.BitLen() - 1; i >= 0; i-- {
		r = pointDouble(r)
		if k.Bit(i) == 1 {
			r = pointAdd(r, p)
		}
	}
	return r
}

func baseMult(k *big.Int) point { return scalarMult(point{x: curveGx, y: curveGy}, k) }

// PrivateKey is a secp256k1 private key.
type PrivateKey struct {
	d   *big.Int
	pub point
}

// NewPrivateKey parses a 32-byte hex private key (with or without 0x).
func NewPrivateKey(hexKey string) (*PrivateKey, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(hexKey), "0x"))
	if err != nil {
		return nil, fmt.Errorf("private key is not hex: %w", err)
	}
	if len(b) != 32 {
		return nil, errors.New("private key must be 32 bytes")
	}
	d := new(big.Int).SetBytes(b)
	if d.Sign() == 0 || d.Cmp(curveN) >= 0 {
		return nil, errors.New("private key out of range")
	}
	return &PrivateKey{d: d, pub: baseMult(d)}, nil
}

// GenerateKey creates a new random key using crypto/rand.
func GenerateKey() (*PrivateKey, string, error) {
	for {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, "", err
		}
		k, err := NewPrivateKey(hex.EncodeToString(b))
		if err == nil {
			return k, "0x" + hex.EncodeToString(b), nil
		}
	}
}

// Address returns the EIP-55 checksummed address.
func (k *PrivateKey) Address() string {
	pub := append(pad32(k.pub.x.Bytes()), pad32(k.pub.y.Bytes())...)
	return ChecksumAddress(Keccak256(pub)[12:])
}

// Sign signs a 32-byte digest and returns r||s||v (v = 27/28),
// deterministic (RFC 6979) and low-S normalised.
func (k *PrivateKey) Sign(digest []byte) ([]byte, error) {
	if len(digest) != 32 {
		return nil, errors.New("digest must be 32 bytes")
	}
	z := new(big.Int).SetBytes(digest)
	for attempt := 0; attempt < 16; attempt++ {
		nonce := rfc6979(k.d, digest, attempt)
		R := baseMult(nonce)
		r := mod(R.x, curveN)
		if r.Sign() == 0 {
			continue
		}
		kInv := new(big.Int).ModInverse(nonce, curveN)
		s := new(big.Int).Mul(r, k.d)
		s.Add(s, z).Mul(s, kInv)
		s = mod(s, curveN)
		if s.Sign() == 0 {
			continue
		}
		recID := byte(R.y.Bit(0))
		if R.x.Cmp(curveN) >= 0 {
			recID |= 2
		}
		if s.Cmp(halfN) > 0 {
			s.Sub(curveN, s)
			recID ^= 1
		}
		sig := make([]byte, 65)
		copy(sig[0:32], pad32(r.Bytes()))
		copy(sig[32:64], pad32(s.Bytes()))
		sig[64] = 27 + recID
		return sig, nil
	}
	return nil, errors.New("could not produce signature")
}

// Verify checks a 65-byte signature against this key's public key.
func (k *PrivateKey) Verify(digest, sig []byte) bool {
	if len(sig) < 64 {
		return false
	}
	r := new(big.Int).SetBytes(sig[0:32])
	s := new(big.Int).SetBytes(sig[32:64])
	if r.Sign() == 0 || s.Sign() == 0 || r.Cmp(curveN) >= 0 || s.Cmp(curveN) >= 0 {
		return false
	}
	z := new(big.Int).SetBytes(digest)
	w := new(big.Int).ModInverse(s, curveN)
	u1 := mod(new(big.Int).Mul(z, w), curveN)
	u2 := mod(new(big.Int).Mul(r, w), curveN)
	p := pointAdd(baseMult(u1), scalarMult(k.pub, u2))
	if p.inf {
		return false
	}
	return mod(p.x, curveN).Cmp(r) == 0
}

// rfc6979 derives a deterministic nonce (HMAC-SHA256). extra>0 only on the
// astronomically rare retry path.
func rfc6979(d *big.Int, hash []byte, extra int) *big.Int {
	x := pad32(d.Bytes())
	h := pad32(mod(new(big.Int).SetBytes(hash), curveN).Bytes())
	v := make([]byte, 32)
	for i := range v {
		v[i] = 0x01
	}
	kk := make([]byte, 32)
	mac := func(key []byte, parts ...[]byte) []byte {
		m := hmac.New(sha256.New, key)
		for _, p := range parts {
			m.Write(p)
		}
		return m.Sum(nil)
	}
	var extraBytes []byte
	if extra > 0 {
		extraBytes = pad32(big.NewInt(int64(extra)).Bytes())
	}
	kk = mac(kk, v, []byte{0x00}, x, h, extraBytes)
	v = mac(kk, v)
	kk = mac(kk, v, []byte{0x01}, x, h, extraBytes)
	v = mac(kk, v)
	for {
		v = mac(kk, v)
		k := new(big.Int).SetBytes(v)
		if k.Sign() > 0 && k.Cmp(curveN) < 0 {
			return k
		}
		kk = mac(kk, v, []byte{0x00})
		v = mac(kk, v)
	}
}

func pad32(b []byte) []byte {
	if len(b) >= 32 {
		return b[len(b)-32:]
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

// ChecksumAddress returns the EIP-55 form of a 20-byte address.
func ChecksumAddress(addr []byte) string {
	lower := hex.EncodeToString(addr)
	hash := hex.EncodeToString(Keccak256([]byte(lower)))
	out := make([]byte, len(lower))
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		if c >= 'a' && c <= 'f' && hash[i] >= '8' {
			c -= 32
		}
		out[i] = c
	}
	return "0x" + string(out)
}

// HexToAddressBytes parses a 0x address.
func HexToAddressBytes(s string) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "0x"))
	if err != nil || len(b) != 20 {
		return nil, fmt.Errorf("invalid address %q", s)
	}
	return b, nil
}

// RecoverAddress returns the signer of a 65-byte r||s||v signature over a
// 32-byte digest (ecrecover). v may be 0/1 or 27/28. High-S is rejected, like
// Ethereum does for transactions, so each message has one valid signature.
func RecoverAddress(digest, sig []byte) (string, error) {
	if len(digest) != 32 || len(sig) != 65 {
		return "", errors.New("recover: need a 32-byte digest and a 65-byte signature")
	}
	r := new(big.Int).SetBytes(sig[0:32])
	s := new(big.Int).SetBytes(sig[32:64])
	v := sig[64]
	if v >= 27 {
		v -= 27
	}
	if v > 3 || r.Sign() == 0 || s.Sign() == 0 || r.Cmp(curveN) >= 0 || s.Cmp(halfN) > 0 {
		return "", errors.New("recover: invalid signature values")
	}
	x := new(big.Int).Set(r)
	if v&2 != 0 {
		x.Add(x, curveN)
	}
	if x.Cmp(curveP) >= 0 {
		return "", errors.New("recover: invalid r")
	}
	// y^2 = x^3 + 7 (mod p); p = 3 mod 4, so y = c^((p+1)/4).
	c := new(big.Int).Exp(x, big.NewInt(3), curveP)
	c.Add(c, big.NewInt(7)).Mod(c, curveP)
	y := new(big.Int).Exp(c, new(big.Int).Rsh(new(big.Int).Add(curveP, big.NewInt(1)), 2), curveP)
	if new(big.Int).Exp(y, big.NewInt(2), curveP).Cmp(c) != 0 {
		return "", errors.New("recover: r is not on the curve")
	}
	if y.Bit(0) != uint(v&1) {
		y.Sub(curveP, y)
	}
	R := point{x: x, y: y}
	rInv := new(big.Int).ModInverse(r, curveN)
	z := new(big.Int).SetBytes(digest)
	negZ := mod(new(big.Int).Neg(z), curveN)
	// Q = r^-1 * (s*R - z*G)
	q := pointAdd(scalarMult(R, s), baseMult(negZ))
	q = scalarMult(q, rInv)
	if q.inf {
		return "", errors.New("recover: point at infinity")
	}
	pub := append(pad32(q.x.Bytes()), pad32(q.y.Bytes())...)
	return ChecksumAddress(Keccak256(pub)[12:]), nil
}
