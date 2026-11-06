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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vikram/agent-arena/internal/arena"
	"github.com/vikram/agent-arena/internal/atomicfile"
	"github.com/vikram/agent-arena/internal/engine"
	"github.com/vikram/agent-arena/internal/evm"
	"github.com/vikram/agent-arena/internal/feed"
	"github.com/vikram/agent-arena/internal/store"
	"github.com/vikram/agent-arena/internal/wallet"
)

type userSession struct {
	Addr string    `json:"addr"`
	Exp  time.Time `json:"exp"`
}

// Sessions last a week and survive restarts: they are saved to
// DATA_DIR/sessions.json, keyed by the SHA-256 of the token, so the file never
// holds a token anyone could use.
const sessionTTL = 7 * 24 * time.Hour

func sessKey(tok string) string { h := sha256.Sum256([]byte(tok)); return hex.EncodeToString(h[:]) }

// loadSessions reads saved sessions (expired ones are dropped).
func (s *Server) loadSessions() {
	b, _, err := atomicfile.Read(s.sessionsPath())
	if err != nil {
		return
	}
	var m map[string]userSession
	if json.Unmarshal(b, &m) != nil {
		return
	}
	users.Lock()
	defer users.Unlock()
	for k, v := range m {
		if time.Now().Before(v.Exp) {
			users.sessions[k] = v
		}
	}
}

// saveSessions writes the open sessions; call with users locked.
func (s *Server) saveSessionsLocked() {
	b, err := json.Marshal(users.sessions)
	if err == nil {
		err = atomicfile.Write(s.sessionsPath(), b, 0o600)
	}
	if err != nil && s.Log != nil {
		s.Log.Warn("sessions not saved", "err", err)
	}
}

func (s *Server) sessionsPath() string { return filepath.Join(s.E.Cfg.DataDir, "sessions.json") }

// meLogout ends the caller's session.
func (s *Server) meLogout(w http.ResponseWriter, r *http.Request, _ string) {
	users.Lock()
	delete(users.sessions, sessKey(r.Header.Get(sessionHeader)))
	s.saveSessionsLocked()
	users.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true})
}

type pendingDeposit struct {
	auth     engine.DepositAuth
	walletID string
	exp      time.Time
}

var users = struct {
	sync.Mutex
	nonces   map[string]userNonce // nonce -> sign-in message for one address
	sessions map[string]userSession
	deposits map[string]pendingDeposit // authorization nonce -> request
	hits     map[string][]time.Time    // bucket|ip -> request times
}{nonces: map[string]userNonce{}, sessions: map[string]userSession{}, deposits: map[string]pendingDeposit{}, hits: map[string][]time.Time{}}

type userNonce struct {
	addr string // lower case
	msg  string
	exp  time.Time
}

// Pending sign-in messages and rate-limit buckets are swept so a flood of
// requests cannot grow memory without bound.
const (
	maxPendingNonces = 20000
	maxHitBuckets    = 10000
)

// jobMu serialises the "one job per account" checks (agent, invest/copy,
// plan) and the unique agent name check with the change they guard, so two
// requests at once cannot both pass.
var jobMu sync.Mutex

// nonceRe pulls the nonce out of a sign-in message the client sends back.
var nonceRe = regexp.MustCompile(`(?m)^Nonce: ([0-9a-f]{16})$`)

const sessionHeader = "X-Arena-Session"

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the OS random source is gone; nothing safe to do
	}
	return hex.EncodeToString(b)
}

func clientIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	// Trust Cloudflare's header only from a local proxy (cloudflared runs on
	// this machine); anyone talking to the server directly could fake it.
	if f := strings.TrimSpace(r.Header.Get("Cf-Connecting-Ip")); f != "" {
		if a := net.ParseIP(ip); a != nil && a.IsLoopback() {
			return f
		}
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
	if len(users.hits) > maxHitBuckets {
		sweepHitsLocked()
	}
	return true
}

// sweepHitsLocked drops buckets with no request in the last hour (the
// longest window any limit uses).
func sweepHitsLocked() {
	for k, ts := range users.hits {
		if len(ts) == 0 || time.Since(ts[len(ts)-1]) > time.Hour {
			delete(users.hits, k)
		}
	}
}

func checksum(addr string) (string, error) {
	b, err := evm.HexToAddressBytes(strings.TrimSpace(addr))
	if err != nil || len(b) != 20 {
		return "", errors.New("not a 0x address")
	}
	return evm.ChecksumAddress(b), nil
}

// accountID is the trading account's wallet id for an address: "u-" and
// 30 hex characters (120 bits, which nobody can grind to take someone
// else's id). Accounts made before this keep their short 10-character id,
// used only while it pays out to the same address.
func (s *Server) accountID(addr string) string {
	a := strings.ToLower(strings.TrimPrefix(addr, "0x"))
	legacy := "u-" + a[:10]
	if _, ok := s.E.Wallets.Get(legacy); ok {
		if p, _ := s.E.Store.Profile(legacy); strings.EqualFold(p.Payout, addr) {
			return legacy
		}
	}
	return "u-" + a[:30]
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
	nonce := randHex(8)
	msg := fmt.Sprintf("Agent Arena sign-in\n\nAddress: %s\nNonce: %s\nIssued At: %s\n\nThis signature is free. It does not move funds or approve anything.",
		addr, nonce, time.Now().UTC().Format(time.RFC3339))
	users.Lock()
	if len(users.nonces) >= maxPendingNonces/2 {
		for k, v := range users.nonces {
			if time.Now().After(v.exp) {
				delete(users.nonces, k)
			}
		}
	}
	full := len(users.nonces) >= maxPendingNonces
	if !full {
		// Keyed by the nonce, not the address: asking for a message for
		// someone else's address never replaces theirs.
		users.nonces[nonce] = userNonce{addr: strings.ToLower(addr), msg: msg, exp: time.Now().Add(10 * time.Minute)}
	}
	users.Unlock()
	if full {
		writeErr(w, 429, "sign-in is busy; try again in a minute")
		return
	}
	writeJSON(w, 200, map[string]any{"message": msg, "nonce": nonce})
}

// meLogin checks the signed message (EIP-191) and opens a 7-day session.
func (s *Server) meLogin(w http.ResponseWriter, r *http.Request) {
	if !allow("auth", r, 20, time.Minute) {
		writeErr(w, 429, "too many sign-in attempts; wait a minute")
		return
	}
	var in struct {
		Address   string `json:"address"`
		Signature string `json:"signature"`
		Nonce     string `json:"nonce"`
		Message   string `json:"message"`
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
	nonce := strings.ToLower(strings.TrimSpace(in.Nonce))
	if m := nonceRe.FindStringSubmatch(in.Message); nonce == "" && m != nil {
		nonce = m[1]
	}
	sig, err := evm.DecodeHex(in.Signature)
	if err != nil {
		writeErr(w, 400, "bad signature")
		return
	}
	// The messages this signature may be for: the one named by its nonce,
	// or (an older app that sends no nonce) the newest few for the address.
	type cand struct {
		nonce string
		n     userNonce
	}
	var cands []cand
	users.Lock()
	for k, v := range users.nonces {
		if v.addr == strings.ToLower(addr) && time.Now().Before(v.exp) && (nonce == "" || k == nonce) {
			cands = append(cands, cand{k, v})
		}
	}
	users.Unlock()
	if len(cands) == 0 {
		writeErr(w, 401, "sign-in expired; try again")
		return
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].n.exp.After(cands[j].n.exp) })
	used := ""
	for _, c := range cands[:min(len(cands), 10)] {
		if who, err := evm.RecoverAddress(evm.PersonalMessageHash([]byte(c.n.msg)), sig); err == nil && strings.EqualFold(who, addr) {
			used = c.nonce
			break
		}
	}
	if used == "" {
		// A wrong signature uses nothing up, so a stranger's bad try can
		// never cancel the real owner's sign-in.
		writeErr(w, 401, "the signature is not from "+addr)
		return
	}
	users.Lock()
	_, fresh := users.nonces[used]
	delete(users.nonces, used) // a message signs in once
	users.Unlock()
	if !fresh { // two logins raced with the same signature
		writeErr(w, 401, "sign-in expired; try again")
		return
	}
	tok := "u_" + randHex(32)
	exp := time.Now().Add(sessionTTL)
	users.Lock()
	for k, v := range users.sessions {
		if time.Now().After(v.Exp) {
			delete(users.sessions, k)
		}
	}
	users.sessions[sessKey(tok)] = userSession{Addr: addr, Exp: exp}
	s.saveSessionsLocked()
	users.Unlock()
	writeJSON(w, 200, map[string]any{"token": tok, "address": addr, "expiresAt": exp.UTC()})
}

// sessionAddr is the signed-in address of a request, or "".
func sessionAddr(r *http.Request) string {
	tok := r.Header.Get(sessionHeader)
	if tok == "" {
		return ""
	}
	users.Lock()
	sess, ok := users.sessions[sessKey(tok)]
	users.Unlock()
	if !ok || time.Now().After(sess.Exp) {
		return ""
	}
	return sess.Addr
}

// me wraps handlers that need a signed-in wallet.
func (s *Server) me(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get(sessionHeader)
		users.Lock()
		sess, ok := users.sessions[sessKey(tok)]
		users.Unlock()
		if tok == "" || !ok || time.Now().After(sess.Exp) {
			writeErr(w, 401, "sign in with your wallet first")
			return
		}
		h(w, r, sess.Addr)
	}
}

// myWallet is the caller's account, checked against its locked payout.
func (s *Server) myWallet(addr string) (*wallet.Wallet, bool, error) {
	wl, ok := s.E.Wallets.Get(s.accountID(addr))
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
		if ua, isAgent := s.E.Store.UserAgent(wl.ID); isAgent {
			out["agent"] = ua // the account is an agent people can follow
		}
		if p, busy := s.activePlan(wl.ID); busy {
			out["plan"] = map[string]any{"id": p.ID, "name": p.Name, "status": p.Status}
		}
	}
	subs := []engine.WalletView{}
	for _, w := range s.mySubWallets(addr) {
		subs = append(subs, s.E.Wallet(ctx, w, false))
	}
	out["wallets"] = subs
	return out
}

var agentNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.'-]{1,22}[A-Za-z0-9]$`)

// meAgent makes the caller's account an agent: it trades its own signals
// from that account, shows on the leaderboard, and anyone can watch, copy or
// invest with it. Every arena rule still applies (stop loss within 5%, size
// and cost guards, live budget, circuit breaker).
func (s *Server) meAgent(w http.ResponseWriter, r *http.Request, addr string) {
	jobMu.Lock()
	defer jobMu.Unlock()
	wl, ok, err := s.myWallet(addr)
	if err != nil || !ok {
		writeErr(w, 409, "create your account first")
		return
	}
	if _, exists := s.E.Store.UserAgent(wl.ID); exists {
		writeErr(w, 409, "your account is already an agent")
		return
	}
	if len(s.E.Store.FollowsOf(wl.ID)) > 0 {
		writeErr(w, 409, "your account follows an agent; stop that first (one job per account)")
		return
	}
	if p, busy := s.activePlan(wl.ID); busy {
		writeErr(w, 409, "your account runs the plan \""+p.Name+"\"; stop it first (one job per account)")
		return
	}
	ua, code, err := s.agentRules(r, wl.ID, addr, nil)
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	if !allow("agent", r, 3, time.Hour) {
		writeErr(w, 429, "too many new agents from here; try later")
		return
	}
	prof, err := engine.UserAgentProfile(ua)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	name, params := ua.Name, ua
	s.E.Store.PutUserAgent(ua)
	_ = s.E.Store.Save()
	s.E.AddAgent(prof)
	s.E.Feed.Publish(feed.Post{Kind: "follow", AgentID: wl.ID, Text: fmt.Sprintf("New agent \"%s\" by %s: %s strategy, take profit %.1f%%, stop loss %.1f%%. Watch, copy or invest with it.",
		name, addr[:6]+"…"+addr[len(addr)-4:], params.Kind, params.TPPct, params.SLPct)})
	s.Log.Info("user agent created", "id", wl.ID, "name", name, "kind", params.Kind, "token", params.Token, "owner", addr)
	writeJSON(w, 201, s.meView(r.Context(), addr))
}

// nameKey is an agent name reduced to lower-case letters and numbers.
func nameKey(n string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		}
		return -1
	}, n)
}

// hiddenRune is a control character or an invisible direction / width mark
// that could make public text read differently from what it says.
func hiddenRune(r rune) bool {
	return unicode.IsControl(r) || (r >= 0x200B && r <= 0x200F) || (r >= 0x202A && r <= 0x202E) ||
		(r >= 0x2066 && r <= 0x2069) || r == 0xFEFF
}

// agentRules reads and checks an agent's settings from the request. old is
// the agent being edited (nil when making a new one): it keeps its name
// unless a new one is given, and its own name does not count as taken.
func (s *Server) agentRules(r *http.Request, id, owner string, old *store.UserAgent) (store.UserAgent, int, error) {
	var in struct {
		Name          string            `json:"name"`
		Tagline       string            `json:"tagline"`
		Strategy      string            `json:"strategy"`
		Token         string            `json:"token"`
		Fast          int               `json:"fast"`
		Slow          int               `json:"slow"`
		Window        int               `json:"window"`
		DipPct        float64           `json:"dipPct"`
		Streak        int               `json:"streak"`
		Trend         int               `json:"trend"`
		TakeProfitPct float64           `json:"takeProfitPct"`
		StopLossPct   float64           `json:"stopLossPct"`
		SizePct       float64           `json:"sizePct"`
		Limits        store.AgentLimits `json:"limits"`
	}
	if err := decode(r, &in); err != nil {
		return store.UserAgent{}, 400, err
	}
	limits, err := engine.CheckAgentLimits(s.E.Cfg, in.Limits)
	if err != nil {
		return store.UserAgent{}, 400, err
	}
	name, tagline := strings.TrimSpace(in.Name), strings.TrimSpace(in.Tagline)
	if old != nil && name == "" {
		name = old.Name
	}
	switch {
	case !agentNameRe.MatchString(name):
		return store.UserAgent{}, 400, errors.New("name: 3-24 letters, numbers, spaces, - _ . '")
	case !utf8.ValidString(tagline) || utf8.RuneCountInString(tagline) > 80:
		return store.UserAgent{}, 400, errors.New("tagline: up to 80 characters")
	case strings.IndexFunc(tagline, hiddenRune) >= 0:
		return store.UserAgent{}, 400, errors.New("tagline: no control or hidden direction characters")
	}
	// "Trend Hunter" or "trend-hunter" would pass for TrendHunter, so names
	// are compared on letters and numbers only.
	for pid, p := range s.E.AgentMap() {
		if pid != id && (nameKey(p.Name) == nameKey(name) || nameKey(pid) == nameKey(name)) {
			return store.UserAgent{}, 409, errors.New("that name is taken (or too close to an existing agent's)")
		}
	}
	token := strings.ToUpper(strings.TrimSpace(in.Token))
	if token == "" {
		token = s.E.Cfg.TradeAsset
	}
	tk, ok := s.E.Cfg.TokenBySymbol(token)
	if !ok {
		return store.UserAgent{}, 400, fmt.Errorf("token: one of %s", strings.Join(s.E.Cfg.TokenSymbols(), ", "))
	}
	token = tk.Symbol // canonical spelling (cbBTC, not CBBTC)
	if mp := s.E.Cfg.MaxPositionPct; mp > 0 && in.SizePct > mp {
		return store.UserAgent{}, 400, fmt.Errorf("size: at most %.0f%% of the account per trade (the arena's limit)", mp)
	}
	_, p, err := arena.BuildStrategy(arena.StrategyParams{Kind: in.Strategy, Fast: in.Fast, Slow: in.Slow, Window: in.Window,
		DipPct: in.DipPct, Streak: in.Streak, Trend: in.Trend, TPPct: in.TakeProfitPct, SLPct: in.StopLossPct, SizePct: in.SizePct})
	if err != nil {
		return store.UserAgent{}, 400, err
	}
	ua := store.UserAgent{ID: id, Name: name, Tagline: tagline, Owner: owner, Kind: p.Kind, Token: token, Fast: p.Fast, Slow: p.Slow,
		Window: p.Window, DipPct: p.DipPct, Streak: p.Streak, Trend: p.Trend, TPPct: p.TPPct, SLPct: p.SLPct, SizePct: p.SizePct,
		Limits: limits, CreatedAt: time.Now().UTC()}
	if old != nil {
		ua.CreatedAt, ua.Paused = old.CreatedAt, old.Paused
	}
	return ua, 0, nil
}

// meAgentEdit changes the rules of the caller's agent. The new rules apply
// from its next decision; open trades keep the take profit and stop loss
// they were placed with.
func (s *Server) meAgentEdit(w http.ResponseWriter, r *http.Request, addr string, wl *wallet.Wallet) {
	jobMu.Lock()
	defer jobMu.Unlock()
	old, ok := s.E.Store.UserAgent(wl.ID)
	if !ok {
		writeErr(w, 404, "your account is not an agent")
		return
	}
	ua, code, err := s.agentRules(r, wl.ID, addr, &old)
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	prof, err := engine.UserAgentProfile(ua)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.E.Store.PutUserAgent(ua)
	_ = s.E.Store.Save()
	s.E.AddAgent(prof) // same id: replaces the rules in place
	s.E.Feed.Publish(feed.Post{Kind: "follow", AgentID: wl.ID, Text: fmt.Sprintf("Agent \"%s\" has new rules: %s on %s, take profit %.1f%%, stop loss %.1f%%, %.0f%% a trade.",
		ua.Name, ua.Kind, ua.Token, ua.TPPct, ua.SLPct, ua.SizePct)})
	s.Log.Info("user agent edited", "id", wl.ID, "kind", ua.Kind, "token", ua.Token)
	writeJSON(w, 200, s.meView(r.Context(), addr))
}

// meAgentAction pauses or resumes the caller's agent (open trades keep their TP / SL).
func (s *Server) meAgentAction(w http.ResponseWriter, r *http.Request, addr string) {
	wl, ok, err := s.myWallet(addr)
	if err != nil || !ok {
		writeErr(w, 409, "create your account first")
		return
	}
	if r.PathValue("action") == "edit" {
		s.meAgentEdit(w, r, addr, wl)
		return
	}
	if r.PathValue("action") == "retire" {
		ua, ok := s.E.Store.UserAgent(wl.ID)
		if !ok {
			writeErr(w, 404, "your account is not an agent")
			return
		}
		n := s.E.RetireAgent(wl.ID)
		s.E.Feed.Publish(feed.Post{Kind: "follow", AgentID: wl.ID, Text: fmt.Sprintf("Agent \"%s\" was retired by its maker; %d follower(s) stopped following it. Open trades keep their take profit and stop loss.", ua.Name, n)})
		writeJSON(w, 200, s.meView(r.Context(), addr))
		return
	}
	pause := r.PathValue("action") == "pause"
	if !pause && r.PathValue("action") != "resume" {
		writeErr(w, 404, "unknown action")
		return
	}
	ua, ok := s.E.Store.UpdateUserAgent(wl.ID, func(a *store.UserAgent) { a.Paused = pause })
	if !ok {
		writeErr(w, 404, "your account is not an agent")
		return
	}
	_ = s.E.Store.Save()
	verb := "resumed"
	if pause {
		verb = "paused"
	}
	s.E.Feed.Publish(feed.Post{Kind: "follow", AgentID: wl.ID, Text: fmt.Sprintf("Agent \"%s\" was %s by its maker", ua.Name, verb)})
	writeJSON(w, 200, s.meView(r.Context(), addr))
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
	nw, err := s.E.Wallets.Create(s.accountID(addr))
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
	mine := ok && strings.EqualFold(p.auth.From, addr)
	if mine {
		delete(users.deposits, in.Nonce) // one use; someone else's try never spends it
	}
	users.Unlock()
	switch {
	case !ok || time.Now().After(p.exp):
		writeErr(w, 410, "this deposit request expired or was used; start again")
		return
	case !mine:
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
	jobMu.Lock()
	defer jobMu.Unlock()
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
	ag, ok := s.E.AgentMap()[in.AgentID]
	if !ok {
		writeErr(w, 400, "unknown agent")
		return
	}
	if _, isAgent := s.E.Store.UserAgent(wl.ID); isAgent {
		writeErr(w, 409, "your account is an agent: it trades its own signals (one job per account)")
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
	jobMu.Lock()
	defer jobMu.Unlock()
	wl, ok, err := s.myWallet(addr)
	if err != nil || !ok {
		writeErr(w, 409, "create your account first")
		return
	}
	if len(s.E.Store.FollowsOf(wl.ID)) > 0 {
		writeErr(w, 409, "your account is linked to an agent; stop that first (one job per account)")
		return
	}
	if _, isAgent := s.E.Store.UserAgent(wl.ID); isAgent {
		writeErr(w, 409, "your account is an agent: it trades its own signals (one job per account)")
		return
	}
	var in map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil || in == nil {
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

// mePlanAction lets the owner pause, resume, stop or check their own plan.
func (s *Server) mePlanAction(w http.ResponseWriter, r *http.Request, addr string) {
	wl, ok, err := s.myWallet(addr)
	if err != nil || !ok {
		writeErr(w, 409, "create your account first")
		return
	}
	p, found := s.E.Store.Plan(r.PathValue("id"))
	if !found {
		writeErr(w, 404, "plan not found")
		return
	}
	if p.WalletID != wl.ID {
		writeErr(w, 403, "this plan is not yours")
		return
	}
	s.planAction(w, r)
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

// mySubWallets are the arena wallets whose payout is locked to addr, other
// than the account itself (for example a copy wallet the operator made for
// this person). The signed-in owner sees and controls them without the admin
// token; money can still only ever go back to addr.
func (s *Server) mySubWallets(addr string) []*wallet.Wallet {
	acct, _, _ := s.myWallet(addr)
	var out []*wallet.Wallet
	for _, w := range s.E.Wallets.List("") {
		if acct != nil && w.ID == acct.ID {
			continue
		}
		if p, ok := s.E.Store.Profile(w.ID); ok && strings.EqualFold(p.Payout, addr) {
			out = append(out, w)
		}
	}
	return out
}

// meSubWallet runs pause | resume | stop | withdraw on one of the caller's
// sub-wallets. Withdrawals go to the payout address, which is the caller.
func (s *Server) meSubWallet(w http.ResponseWriter, r *http.Request, addr string) {
	var wl *wallet.Wallet
	for _, x := range s.mySubWallets(addr) {
		if x.ID == r.PathValue("id") {
			wl = x
		}
	}
	if wl == nil {
		writeErr(w, 404, "no such wallet of yours")
		return
	}
	switch act := r.PathValue("action"); act {
	case "resume":
		if p, ok := s.E.Store.Profile(wl.ID); ok && strings.Contains(strings.ToLower(p.PausedReason), "operator") {
			writeErr(w, 409, "the operator paused this wallet; ask them to resume it")
			return
		}
		fallthrough
	case "pause":
		s.E.Store.UpdateProfile(wl.ID, func(p *store.WalletProfile) {
			p.Paused, p.PausedReason = act == "pause", ""
			if act == "pause" {
				p.PausedReason = "paused by the owner"
			}
		})
	case "stop":
		for _, f := range s.E.Store.FollowsOf(wl.ID) {
			s.E.Store.Unfollow(wl.ID, f.AgentID)
		}
	case "withdraw":
		if !allow("withdraw", r, 10, time.Hour) {
			writeErr(w, 429, "too many withdrawals from here; try later")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		tx, amt, err := s.E.Withdraw(ctx, wl.ID, "USDC", 0)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		s.Log.Info("owner withdrew from a sub-wallet", "wallet", wl.ID, "amount", amt, "tx", tx)
	default:
		writeErr(w, 404, "unknown action")
		return
	}
	_ = s.E.Store.Save()
	writeJSON(w, 200, s.meView(r.Context(), addr))
}
