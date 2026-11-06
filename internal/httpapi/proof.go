package httpapi

// GET /v1/proof: the real-money record. Only trades that went to the real
// Flash API with real funds are listed, each with what anyone needs to check
// it: the Flash order ids, the funding wallet and the Base transaction hashes
// (entry fill, exit fill, approve). Paper and dry-run trades are counted, never
// listed, so the page cannot pass simulated trades off as real ones.

import (
	"net/http"
	"strconv"
	"time"
)

type proofRow struct {
	ID             string    `json:"id"`
	At             time.Time `json:"at"`
	Who            string    `json:"who"`
	Kind           string    `json:"kind"`
	Token          string    `json:"token"`
	Style          string    `json:"style,omitempty"`
	SizeUSD        float64   `json:"sizeUsd"`
	Status         string    `json:"status"`
	Wallet         string    `json:"wallet,omitempty"` // funding address on Base
	OrderID        string    `json:"orderId,omitempty"`
	BracketOrderID string    `json:"bracketOrderId,omitempty"`
	TPOrderID      string    `json:"tpOrderId,omitempty"`
	ExitOrderID    string    `json:"exitOrderId,omitempty"`
	ApproveTx      string    `json:"approveTx,omitempty"`
	EntryTx        string    `json:"entryTx,omitempty"`
	ExitTx         string    `json:"exitTx,omitempty"`
	USDCSpent      float64   `json:"usdcSpent,omitempty"`
	USDCReceived   float64   `json:"usdcReceived,omitempty"`
	CostUSD        float64   `json:"costUsd,omitempty"` // entry all-in cost from the quote
	PnLUSD         *float64  `json:"pnlUsd,omitempty"`
}

func (s *Server) proof(w http.ResponseWriter, r *http.Request) {
	feeBps, _ := strconv.ParseFloat(s.E.Cfg.FeeBps, 64)
	rows := []proofRow{}
	var paper, dry, filled, closed, onchainTx int
	var volume, pnl, cost float64
	for _, t := range s.E.Store.Trades("", 0) {
		switch {
		case t.DryRun:
			dry++
			continue
		case t.Paper:
			paper++
			continue
		}
		row := proofRow{ID: t.ID, At: t.CreatedAt, Who: s.name(t), Kind: t.Kind, Token: tokenOr(t.Token), Style: t.Style,
			SizeUSD: t.SizeUSD, Status: t.Status, OrderID: t.OrderID, BracketOrderID: t.BracketOrderID, TPOrderID: t.TPOrderID,
			ExitOrderID: t.ExitOrderID, ApproveTx: t.ApproveTx, EntryTx: t.EntryTx, ExitTx: t.ExitTx,
			USDCSpent: t.USDCSpent, USDCReceived: t.USDCReceived, CostUSD: t.EntryCostUSD, PnLUSD: t.PnLUSD}
		if t.Kind == "self" {
			row.Who = "Own wallet"
		}
		if wl, ok := s.E.Wallets.Get(t.WalletID); ok {
			row.Wallet = wl.Address()
		}
		rows = append(rows, row)
		volume += t.USDCSpent + t.USDCReceived
		cost += t.EntryCostUSD
		if t.EntryTx != "" || t.USDCSpent > 0 {
			filled++
		}
		if t.Status == "closed" {
			closed++
		}
		if t.PnLUSD != nil {
			pnl += *t.PnLUSD
		}
		for _, tx := range []string{t.ApproveTx, t.EntryTx, t.ExitTx} {
			if tx != "" {
				onchainTx++
			}
		}
	}
	if len(rows) > 200 {
		rows = rows[:200] // newest first
	}
	writeJSON(w, 200, map[string]any{
		"mode": s.E.Cfg.ModeName(), "live": s.E.Cfg.Live(), "liveAgents": s.E.Cfg.LiveAgents,
		"liveBudgetUsd": s.E.Cfg.LiveBudgetUSD, "liveSpentUsd": s.E.LiveSpent(),
		"explorer": "https://basescan.org", "settlement": s.settlementAddr(), "vault": s.vaultAddr(),
		"totals": map[string]any{"orders": len(rows), "filled": filled, "closed": closed, "onchainTxs": onchainTx,
			"volumeUsd": round2(volume), "entryCostUsd": round2(cost), "integratorFeeUsd": round2(volume * feeBps / 10000),
			"pnlUsd": round2(pnl), "feeBps": feeBps},
		"notListed": map[string]int{"paper": paper, "dryRun": dry},
		"trades":    rows,
	})
}

// settlementAddr / vaultAddr are the pinned Flash contracts (public, onchain).
func (s *Server) settlementAddr() string {
	if l := s.E.Cfg.SettlementAllowlist; len(l) > 0 {
		return l[0]
	}
	return "0x5d00000873b6BF41539e6f5365B0Ff7d3c368f78"
}

func (s *Server) vaultAddr() string {
	if l := s.E.Cfg.VaultAllowlist; len(l) > 0 {
		return l[0]
	}
	return ""
}
