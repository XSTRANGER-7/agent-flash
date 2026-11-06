package httpapi

// "My account": anyone with a wallet (MetaMask, Coinbase Wallet, Rabbit,
// any EOA) gets an arena trading account and never needs ETH.
//
//  1. Sign in: the wallet signs a one-time text message (free, offchain).
//  2. Account: an arena sub-wallet whose payout is locked to that address
//     at creation, so its money can only ever go back to the person.
//  3. Add funds: a USDC EIP-3009 signature (free); the gas tank sends it.
//  4. Invest with / copy an agent, pause, withdraw: Flash pays the order
//     gas, the gas tank pays approvals and the withdrawal.
//
// The server builds every payload; the browser only returns signatures, so a
// client can never change an amount or a destination.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vikram/agent-arena/internal/engine"
	"github.com/vikram/agent-arena/internal/evm"
	"github.com/vikram/agent-arena/internal/feed"
	"github.com/vikram/agent-arena/internal/store"
	"github.com/vikram/agent-arena/internal/wallet"
)

type userSession struct {
	addr string
	exp  time.Time
}

type pendingDeposit struct {
	auth     engine.DepositAuth
	walletID string
	exp      time.Time
}

var users = struct {
	sync.Mutex
	nonces   map[string]userNonce // lower(address) -> sign-in message
	sessions map[string]userSession
	deposits map[string]pendingDeposit // authorization nonce -> request
	hits     map[string][]time.Time    // bucket|ip -> request times
}{nonces: map[string]userNonce{}, sessions: map[string]userSession{}, deposits: map[string]pendingDeposit{}, hits: map[string][]time.Time{}}

type userNonce struct {
	msg string
	exp time.Time
}

const sessionHeader = "X-Arena-Session"

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the OS random source is gone; nothing safe to do
	}
	return hex.EncodeToString(b)
}

func clientIP(r *http.Request) string {
	if f := r.Header.Get("Cf-Connecting-Ip"); f != "" {
		return f
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

// allow is a sliding-window rate limit per client IP and bucket.
func allow(bucket string, r *http.Request, n int, per time.Duration) bool {
	k := bucket + "|" + clientIP(r)
	users.Lock()
	defer users.Unlock()
	var keep []time.Time
	for _, t := range users.hits[k] {
		if time.Since(t) < per {
			keep = append(keep, t)
		}
	}
	if len(keep) >= n {
		users.hits[k] = keep
		return false
	}
	users.hits[k] = append(keep, time.Now())
	return true
}

func checksum(addr string) (string, error) {
	b, err := evm.HexToAddressBytes(strings.TrimSpace(addr))
	if err != nil || len(b) != 20 {
		return "", errors.New("not a 0x address")
	}
	return evm.ChecksumAddress(b), nil
}

// accountID is the trading account's wallet id for an address.
func accountID(addr string) string {
	a := strings.ToLower(strings.TrimPrefix(addr, "0x"))
	return "u-" + a[:10]
}

// meNonce returns the one-time sign-in message for an address.
func (s *Server) meNonce(w http.ResponseWriter, r *http.Request) {
	if !allow("auth", r, 20, time.Minute) {
		writeErr(w, 429, "too many sign-in attempts; wait a minute")
		return
	}
	var in struct {
		Address string `json:"address"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	addr, err := checksum(in.Address)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	msg := fmt.Sprintf("Agent Arena sign-in\n\nAddress: %s\nNonce: %s\nIssued At: %s\n\nThis signature is free. It does not move funds or approve anything.",
		addr, randHex(8), time.Now().UTC().Format(time.RFC3339))
	users.Lock()
	users.nonces[strings.ToLower(addr)] = userNonce{msg: msg, exp: time.Now().Add(10 * time.Minute)}
	users.Unlock()
	writeJSON(w, 200, map[string]any{"message": msg})
}

// meLogin checks the signed message (EIP-191) and opens a 12-hour session.
func (s *Server) meLogin(w http.ResponseWriter, r *http.Request) {
	if !allow("auth", r, 20, time.Minute) {
		writeErr(w, 429, "too many sign-in attempts; wait a minute")
		return
	}
	var in struct {
		Address   string `json:"address"`
		Signature string `json:"signature"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	addr, err := checksum(in.Address)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	users.Lock()
	n, ok := users.nonces[strings.ToLower(addr)]
	delete(users.nonces, strings.ToLower(addr)) // one try per message
	users.Unlock()
	if !ok || time.Now().After(n.exp) {
		writeErr(w, 401, "sign-in expired; try again")
		return
	}
	sig, err := evm.DecodeHex(in.Signature)
	if err != nil {
		writeErr(w, 400, "bad signature")
		return
	}
	who, err := evm.RecoverAddress(evm.PersonalMessageHash([]byte(n.msg)), sig)
	if err != nil || !strings.EqualFold(who, addr) {
		writeErr(w, 401, "the signature is not from "+addr)
		return
	}
	tok := "u_" + randHex(32)
	exp := time.Now().Add(12 * time.Hour)
	users.Lock()
	for k, v := range users.sessions {
		if time.Now().After(v.exp) {
			delete(users.sessions, k)
		}
	}
	users.sessions[tok] = userSession{addr: addr, exp: exp}
	users.Unlock()
	writeJSON(w, 200, map[string]any{"token": tok, "address": addr, "expiresAt": exp.UTC()})
}

// me wraps handlers that need a signed-in wallet.
func (s *Server) me(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get(sessionHeader)
		users.Lock()
		sess, ok := users.sessions[tok]
		users.Unlock()
		if tok == "" || !ok || time.Now().After(sess.exp) {
			writeErr(w, 401, "sign in with your wallet first")
			return
		}
		h(w, r, sess.addr)
	}
}

// myWallet is the caller's account, checked against its locked payout.
func (s *Server) myWallet(addr string) (*wallet.Wallet, bool, error) {
	wl, ok := s.E.Wallets.Get(accountID(addr))
	if !ok {
		return nil, false, nil
	}
	p, _ := s.E.Store.Profile(wl.ID)
	if !strings.EqualFold(p.Payout, addr) {
		return nil, false, errors.New("this account id belongs to another address")
	}
	return wl, true, nil
}

func (s *Server) meView(ctx context.Context, addr string) map[string]any {
	out := map[string]any{"address": addr, "mode": s.E.Cfg.ModeName(), "depositMaxUsd": s.E.Cfg.DepositMaxUSD,
		"gas": s.E.Exec.GasTankStatus(ctx), "maxPositionPct": s.E.Cfg.MaxPositionPct, "maxTradeUsd": s.E.Cfg.MaxTradeUSD}
	if wl, ok, err := s.myWallet(addr); err == nil && ok {
		v := s.E.Wallet(ctx, wl, false)
		out["account"] = v
	}
	return out
}

func (s *Server) meGet(w http.ResponseWriter, r *http.Request, addr string) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	writeJSON(w, 200, s.meView(ctx, addr))
}

// meAccount creates the caller's trading account (idempotent).
func (s *Server) meAccount(w http.ResponseWriter, r *http.Request, addr string) {
	if _, ok, err := s.myWallet(addr); err != nil {
		writeErr(w, 409, err.Error())
		return
	} else if ok {
		writeJSON(w, 200, s.meView(r.Context(), addr))
		return
	}
	if s.E.Wallets.IsArenaAddress(addr) {
		writeErr(w, 400, "sign in with your own wallet, not one of the arena's")
		return
	}
	if !allow("account", r, 3, time.Hour) {
		writeErr(w, 429, "too many new accounts from here; try later")
		return
	}
	nw, err := s.E.Wallets.Create(accountID(addr))
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	short := addr[:6] + "…" + addr[len(addr)-4:]
	s.E.Store.PutProfile(store.WalletProfile{ID: nw.ID, Owner: short, Payout: addr, LossLimitPct: 20})
	_ = s.E.Store.Save()
	s.E.Feed.Publish(feed.Post{Kind: "follow", Text: fmt.Sprintf("A new trader (%s) opened an arena account", short)})
	s.Log.Info("trading account created", "id", nw.ID, "address", nw.Address(), "payout", addr)
	writeJSON(w, 201, s.meView(r.Context(), addr))
}

// meDepositPrepare builds the USDC authorization the person signs.
func (s *Server) meDepositPrepare(w http.ResponseWriter, r *http.Request, addr string) {
	wl, ok, err := s.myWallet(addr)
	if err != nil || !ok {
		writeErr(w, 409, "create your account first")
		return
	}
	var in struct {
		AmountUSD float64 `json:"amountUsd"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	amt := math.Floor(in.AmountUSD*100) / 100
	if amt < 1 || amt > s.E.Cfg.DepositMaxUSD {
		writeErr(w, 400, fmt.Sprintf("amount must be $1 to $%.0f", s.E.Cfg.DepositMaxUSD))
		return
	}
	if !allow("deposit", r, 10, time.Hour) {
		writeErr(w, 429, "too many deposits from here; try later")
		return
	}
	a := engine.DepositAuth{From: addr, To: wl.Address(), Value: new(big.Int).Mul(big.NewInt(int64(math.Round(amt*100))), big.NewInt(10_000)),
		ValidAfter: 0, ValidBefore: time.Now().Add(30 * time.Minute).Unix(), Nonce: "0x" + randHex(32)}
	users.Lock()
	for k, v := range users.deposits {
		if time.Now().After(v.exp) {
			delete(users.deposits, k)
		}
	}
	users.deposits[a.Nonce] = pendingDeposit{auth: a, walletID: wl.ID, exp: time.Unix(a.ValidBefore, 0)}
	users.Unlock()
	writeJSON(w, 200, map[string]any{"typedData": s.E.Exec.DepositTypedData(a), "nonce": a.Nonce, "amountUsd": amt,
		"to": wl.Address(), "expiresAt": time.Unix(a.ValidBefore, 0).UTC(), "mode": s.E.Cfg.ModeName()})
}

// meDeposit sends the signed authorization (the gas tank pays in live mode).
func (s *Server) meDeposit(w http.ResponseWriter, r *http.Request, addr string) {
	var in struct {
		Nonce     string `json:"nonce"`
		Signature string `json:"signature"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	users.Lock()
	p, ok := users.deposits[in.Nonce]
	delete(users.deposits, in.Nonce) // one use
	users.Unlock()
	switch {
	case !ok || time.Now().After(p.exp):
		writeErr(w, 410, "this deposit request expired or was used; start again")
		return
	case !strings.EqualFold(p.auth.From, addr):
		writeErr(w, 403, "this deposit belongs to another wallet")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	tx, err := s.E.Deposit(ctx, p.walletID, p.auth, in.Signature)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	usd, _ := new(big.Float).Quo(new(big.Float).SetInt(p.auth.Value), big.NewFloat(1e6)).Float64()
	tag := ""
	if s.E.Cfg.Paper {
		tag = "[paper] "
	}
	s.E.Feed.Publish(feed.Post{Kind: "follow", Text: fmt.Sprintf("%sA trader added $%.2f USDC to their account, no gas", tag, usd)})
	wl, _ := s.E.Wallets.Get(p.walletID)
	writeJSON(w, 200, map[string]any{"ok": true, "tx": tx, "amountUsd": usd, "account": s.E.Wallet(ctx, wl, false)})
}

// meInvest links the account to one agent (invest = trade on every signal,
// copy = mirror the agent's placed trades), with the person's limits.
func (s *Server) meInvest(w http.ResponseWriter, r *http.Request, addr string) {
	wl, ok, err := s.myWallet(addr)
	if err != nil || !ok {
		writeErr(w, 409, "create your account first")
		return
	}
	var in struct {
		AgentID        string  `json:"agentId"`
		Mode           string  `json:"mode"`
		AllocationPct  float64 `json:"allocationPct"`
		MaxPerTradeUSD float64 `json:"maxPerTradeUsd"`
		DailyCapUSD    float64 `json:"dailyCapUsd"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ag, ok := s.E.Agents[in.AgentID]
	if !ok {
		writeErr(w, 400, "unknown agent")
		return
	}
	if p, busy := s.activePlan(wl.ID); busy {
		writeErr(w, 409, "your account runs the plan \""+p.Name+"\"; stop it first (one job per account)")
		return
	}
	if in.Mode == "" {
		in.Mode = "invest"
	}
	if in.Mode != "invest" && in.Mode != "copy" {
		writeErr(w, 400, "mode must be invest or copy")
		return
	}
	f, err := s.followFrom(wl.ID, in.AgentID, in.Mode, in.AllocationPct, in.MaxPerTradeUSD, in.DailyCapUSD)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	for _, old := range s.E.Store.FollowsOf(wl.ID) { // one agent per account
		if old.AgentID != in.AgentID {
			s.E.Store.Unfollow(wl.ID, old.AgentID)
		}
	}
	s.E.Store.PutFollow(f)
	_ = s.E.Store.Save()
	verb := "is investing with"
	if in.Mode == "copy" {
		verb = "is copying"
	}
	p, _ := s.E.Store.Profile(wl.ID)
	s.E.Feed.Publish(feed.Post{Kind: "follow", AgentID: in.AgentID, Text: fmt.Sprintf("%s %s %s (max $%.2f a trade, $%.2f a day)", p.Owner, verb, ag.Name, f.MaxPerTradeUSD, f.DailyCapUSD)})
	writeJSON(w, 200, s.meView(r.Context(), addr))
}

// activePlan is the plan running on a wallet, if any.
func (s *Server) activePlan(walletID string) (store.Plan, bool) {
	for _, p := range s.E.Store.Plans() {
		if p.WalletID == walletID && (p.Status == "active" || p.Status == "paused") {
			return p, true
		}
	}
	return store.Plan{}, false
}

// mePlan starts the person's own auto-trader on their account: same fields
// as the plan builder; owner, payout and wallet are forced to the caller.
func (s *Server) mePlan(w http.ResponseWriter, r *http.Request, addr string) {
	wl, ok, err := s.myWallet(addr)
	if err != nil || !ok {
		writeErr(w, 409, "create your account first")
		return
	}
	if len(s.E.Store.FollowsOf(wl.ID)) > 0 {
		writeErr(w, 409, "your account is linked to an agent; stop that first (one job per account)")
		return
	}
	var in map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
		writeErr(w, 400, "bad plan")
		return
	}
	in["owner"] = addr[:6] + "…" + addr[len(addr)-4:]
	in["payout"] = addr
	in["walletId"] = wl.ID
	b, _ := json.Marshal(in)
	r2 := r.Clone(r.Context())
	r2.Body, r2.ContentLength = io.NopCloser(bytes.NewReader(b)), int64(len(b))
	s.planCreate(w, r2)
}

// meStop unlinks the account from its agent. Open trades keep their TP / SL.
func (s *Server) meStop(w http.ResponseWriter, r *http.Request, addr string) {
	wl, ok, err := s.myWallet(addr)
	if err != nil || !ok {
		writeErr(w, 409, "create your account first")
		return
	}
	for _, f := range s.E.Store.FollowsOf(wl.ID) {
		s.E.Store.Unfollow(wl.ID, f.AgentID)
	}
	_ = s.E.Store.Save()
	writeJSON(w, 200, s.meView(r.Context(), addr))
}

// mePause pauses or resumes the account (no new trades while paused).
func (s *Server) mePause(w http.ResponseWriter, r *http.Request, addr string) {
	wl, ok, err := s.myWallet(addr)
	if err != nil || !ok {
		writeErr(w, 409, "create your account first")
		return
	}
	pause := strings.HasSuffix(r.URL.Path, "/pause")
	s.E.Store.UpdateProfile(wl.ID, func(p *store.WalletProfile) {
		p.Paused, p.PausedReason = pause, ""
		if pause {
			p.PausedReason = "paused by the owner"
		}
	})
	_ = s.E.Store.Save()
	writeJSON(w, 200, s.meView(r.Context(), addr))
}

// meWithdraw sends the account's free USDC (or WETH) to the person's own
// address, the only place it can go. The gas tank pays the gas.
func (s *Server) meWithdraw(w http.ResponseWriter, r *http.Request, addr string) {
	wl, ok, err := s.myWallet(addr)
	if err != nil || !ok {
		writeErr(w, 409, "create your account first")
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.Token == "" {
		in.Token = "USDC"
	}
	if !allow("withdraw", r, 10, time.Hour) {
		writeErr(w, 429, "too many withdrawals from here; try later")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	tx, amt, err := s.E.Withdraw(ctx, wl.ID, in.Token, 0)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "tx": tx, "amount": amt, "token": in.Token, "to": addr, "account": s.E.Wallet(ctx, wl, false)})
}
