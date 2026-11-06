// Package wallet manages the Level-1 sub-wallets (one EOA per agent and per
// follower) and the Signer abstraction. Agents never touch keys: only the
// engine calls a Signer, after the referee has approved the trade.
package wallet

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/vikram/agent-arena/internal/atomicfile"
	"github.com/vikram/agent-arena/internal/evm"
)

// Signer is the only thing that can produce signatures. Swap LocalSigner for
// a KMS / HSM / smart-account session-key signer without touching the engine.
type Signer interface {
	Address() string
	SignTypedData(typedDataJSON string) (string, error) // EIP-712, 0x hex
	SignPersonal(msg string) (string, error)            // EIP-191, 0x hex
	Key() *evm.PrivateKey                               // nil for remote signers
}

type LocalSigner struct{ k *evm.PrivateKey }

func (s *LocalSigner) Address() string      { return s.k.Address() }
func (s *LocalSigner) Key() *evm.PrivateKey { return s.k }
func (s *LocalSigner) SignPersonal(m string) (string, error) {
	sig, err := s.k.Sign(evm.PersonalMessageHash([]byte(m)))
	if err != nil {
		return "", err
	}
	return evm.SigHex(sig), nil
}
func (s *LocalSigner) SignTypedData(raw string) (string, error) {
	td, err := evm.ParseTypedData(raw)
	if err != nil {
		return "", err
	}
	d, err := td.Digest()
	if err != nil {
		return "", err
	}
	sig, err := s.k.Sign(d)
	if err != nil {
		return "", err
	}
	return evm.SigHex(sig), nil
}

// Role separates competing agents from human followers.
type Role string

const (
	RoleAgent    Role = "agent"
	RoleFollower Role = "follower"
)

type Wallet struct {
	ID     string `json:"id"`
	Role   Role   `json:"role"`
	Signer Signer `json:"-"`
}

func (w *Wallet) Address() string { return w.Signer.Address() }

type Registry struct {
	mu   sync.RWMutex
	path string
	byID map[string]*Wallet
}

// fileFormat: {"agents":{"trendhunter":"0xKEY"}, "followers":{"f1":"0xKEY"}}
type fileFormat struct {
	Agents    map[string]string `json:"agents"`
	Followers map[string]string `json:"followers"`
}

// Load reads the wallets file. It refuses files readable by group/others.
func Load(path string) (*Registry, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("wallets file: %w (run `arena keygen` first)", err)
	}
	// Windows has no Unix permission bits (Go always reports 0666 there; access
	// is controlled by ACLs), so the check only means something elsewhere.
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("wallets file %s must be chmod 600 (is %v)", path, st.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f fileFormat
	if err := json.Unmarshal(b, &f); err != nil {
		// A crash while adding a wallet: the synced side copy has every key.
		side, serr := os.ReadFile(path + ".new")
		if serr != nil || json.Unmarshal(side, &f) != nil {
			return nil, fmt.Errorf("wallets file: %w (no usable %s.new; restore wallets.json from your backup)", err, filepath.Base(path))
		}
		if werr := atomicfile.WriteInPlace(path, side, 0o600); werr != nil {
			return nil, fmt.Errorf("wallets file recovered from %s.new but could not be rewritten: %w", filepath.Base(path), werr)
		}
	}
	r := &Registry{path: path, byID: map[string]*Wallet{}}
	add := func(id string, role Role, key string) error {
		id = strings.ToLower(strings.TrimSpace(id))
		if id == "" {
			return errors.New("empty wallet id")
		}
		if _, dup := r.byID[id]; dup {
			return fmt.Errorf("duplicate wallet id %s", id)
		}
		k, err := evm.NewPrivateKey(key)
		if err != nil {
			return fmt.Errorf("wallet %s: %w", id, err)
		}
		r.byID[id] = &Wallet{ID: id, Role: role, Signer: &LocalSigner{k: k}}
		return nil
	}
	for id, k := range f.Agents {
		if err := add(id, RoleAgent, k); err != nil {
			return nil, err
		}
	}
	for id, k := range f.Followers {
		if err := add(id, RoleFollower, k); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *Registry) Get(id string) (*Wallet, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	w, ok := r.byID[strings.ToLower(id)]
	return w, ok
}

func (r *Registry) List(role Role) []*Wallet {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*Wallet
	for _, w := range r.byID {
		if role == "" || w.Role == role {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Generate writes a fresh wallets file with the given ids (chmod 600).
func Generate(path string, agents, followers []string) (map[string]string, error) {
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("%s already exists; refusing to overwrite keys", path)
	}
	f := fileFormat{Agents: map[string]string{}, Followers: map[string]string{}}
	addrs := map[string]string{}
	for _, set := range []struct {
		ids []string
		m   map[string]string
	}{{agents, f.Agents}, {followers, f.Followers}} {
		for _, id := range set.ids {
			k, hexKey, err := evm.GenerateKey()
			if err != nil {
				return nil, err
			}
			set.m[id] = hexKey
			addrs[id] = k.Address()
		}
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	if err := atomicfile.Write(path, b, 0o600); err != nil {
		return nil, err
	}
	return addrs, nil
}

var validID = func(id string) bool {
	if len(id) < 2 || len(id) > 32 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// Create adds a new follower sub-wallet with a fresh key and saves the file
// in place (keeping its permissions / Windows ACL). The key never leaves
// this process; only the address is returned.
func (r *Registry) Create(id string) (*Wallet, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if !validID(id) {
		return nil, errors.New("wallet id must be 2-32 chars of a-z, 0-9, - or _")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byID[id]; dup {
		return nil, fmt.Errorf("wallet %s already exists", id)
	}
	if r.path == "" {
		return nil, errors.New("registry has no file")
	}
	b, err := os.ReadFile(r.path)
	if err != nil {
		return nil, err
	}
	var f fileFormat
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	if f.Followers == nil {
		f.Followers = map[string]string{}
	}
	k, hexKey, err := evm.GenerateKey()
	if err != nil {
		return nil, err
	}
	f.Followers[id] = hexKey
	out, _ := json.MarshalIndent(f, "", "  ")
	// In place keeps the file's ACL; a synced side copy guards the keys
	// against a crash mid-write (Load recovers from it).
	if err := atomicfile.WriteInPlace(r.path, out, 0o600); err != nil {
		return nil, err
	}
	w := &Wallet{ID: id, Role: RoleFollower, Signer: &LocalSigner{k: k}}
	r.byID[id] = w
	return w, nil
}

// RoleSelf is a person's own wallet (MetaMask, Coinbase Wallet, any EOA):
// the arena only knows its address. Orders are signed in the person's
// wallet; this service can never sign for it.
const RoleSelf Role = "self"

type watchSigner struct{ addr string }

var ErrSelfCustody = errors.New("self-custody wallet: this service cannot sign for it; sign in your own wallet")

func (s watchSigner) Address() string                      { return s.addr }
func (s watchSigner) Key() *evm.PrivateKey                 { return nil }
func (s watchSigner) SignTypedData(string) (string, error) { return "", ErrSelfCustody }
func (s watchSigner) SignPersonal(string) (string, error)  { return "", ErrSelfCustody }

// SelfID is the wallet id used for a person's own address.
func SelfID(addr string) string {
	a := strings.ToLower(strings.TrimPrefix(addr, "0x"))
	if len(a) > 10 {
		a = a[:10]
	}
	return "self-" + a
}

// Watch registers a person's own address (no key). Idempotent.
func (r *Registry) Watch(addr string) (*Wallet, error) {
	b, err := evm.HexToAddressBytes(addr)
	if err != nil || len(b) != 20 {
		return nil, errors.New("not a 0x address")
	}
	checksum := evm.ChecksumAddress(b)
	id := SelfID(checksum)
	r.mu.Lock()
	defer r.mu.Unlock()
	if w, ok := r.byID[id]; ok {
		if !strings.EqualFold(w.Address(), checksum) {
			return nil, errors.New("wallet id clash; use another address")
		}
		return w, nil
	}
	for _, w := range r.byID {
		if strings.EqualFold(w.Address(), checksum) {
			return nil, errors.New("that address is one of the arena's own sub-wallets")
		}
	}
	w := &Wallet{ID: id, Role: RoleSelf, Signer: watchSigner{addr: checksum}}
	r.byID[id] = w
	return w, nil
}

// RemoveFromFile takes the given follower ids out of a wallets file. Their
// keys are not deleted: they are written (synced) to backupPath first, so a
// mistake can be undone. Agent wallets are never removed.
func RemoveFromFile(path string, ids []string, backupPath string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var f fileFormat
	if err := json.Unmarshal(b, &f); err != nil {
		return 0, err
	}
	removed := fileFormat{Agents: map[string]string{}, Followers: map[string]string{}}
	for _, id := range ids {
		if k, ok := f.Followers[id]; ok {
			removed.Followers[id] = k
			delete(f.Followers, id)
		}
	}
	if len(removed.Followers) == 0 {
		return 0, nil
	}
	rb, _ := json.MarshalIndent(removed, "", "  ")
	if err := atomicfile.Write(backupPath, rb, 0o600); err != nil {
		return 0, fmt.Errorf("backup of removed keys: %w", err)
	}
	out, _ := json.MarshalIndent(f, "", "  ")
	if err := atomicfile.WriteInPlace(path, out, 0o600); err != nil {
		return 0, err
	}
	return len(removed.Followers), nil
}
