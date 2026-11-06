package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vikram/agent-arena/internal/referee"
	"github.com/vikram/agent-arena/internal/store"
)

type intentT = referee.Intent

func steps(tl []store.Step) string {
	var s []string
	for _, x := range tl {
		s = append(s, x.Step)
	}
	return strings.Join(s, ",")
}

func TestTWAPEntryFillsSliceBySliceWithBracket(t *testing.T) {
	e, _, v, _ := paperSetup(t)
	ctx := context.Background()
	in := intent()
	in.SizeUSD, in.Style, in.TwapMinutes = 5, "twap", 10
	res := e.Propose(ctx, in)
	if res.Trade == nil || res.Trade.OrderType != "twap+bracket" {
		t.Fatalf("twap propose: %+v", res)
	}
	if !strings.Contains(steps(res.Trade.Timeline), "submitted") || !strings.Contains(res.Trade.Timeline[len(res.Trade.Timeline)-1].Detail, "10 slices") {
		t.Fatalf("timeline: %+v", res.Trade.Timeline)
	}
	e.RefreshTrade(ctx, res.Trade.ID)
	tr, _ := e.Store.Trade(res.Trade.ID)
	if tr.Status != "placed" || tr.BracketOrderID == "" {
		t.Fatalf("after slice 1: status %s bracket %q (bracket must be active from the first slice)", tr.Status, tr.BracketOrderID)
	}
	v.Advance(11 * time.Minute)
	v.Tick(ctx)
	e.RefreshTrade(ctx, tr.ID)
	tr, _ = e.Store.Trade(tr.ID)
	if tr.Status != "filled" || tr.USDCSpent < 4.99 || tr.WETHAmount <= 0 {
		t.Fatalf("after all slices: %s spent %v weth %v", tr.Status, tr.USDCSpent, tr.WETHAmount)
	}
}

func TestStopBuyTriggersThenProtects(t *testing.T) {
	e, _, v, px := paperSetup(t)
	ctx := context.Background()
	in := intent()
	in.SizeUSD, in.Style, in.StopPrice = 5, "stop", 3030
	in.TakeProfit, in.StopLoss = 3300, 2990
	res := e.Propose(ctx, in)
	if res.Trade == nil || res.Trade.OrderType != "stop+protect" {
		t.Fatalf("stop propose: %+v", res)
	}
	v.Tick(ctx) // price 3000: below the trigger
	e.RefreshTrade(ctx, res.Trade.ID)
	if tr, _ := e.Store.Trade(res.Trade.ID); tr.Status != "placed" {
		t.Fatalf("stop filled before its trigger: %s", tr.Status)
	}
	px.Store(3031.0)
	v.Tick(ctx)
	e.RefreshTrade(ctx, res.Trade.ID)
	tr, _ := e.Store.Trade(res.Trade.ID)
	if tr.Status != "filled" || tr.BracketOrderID == "" || tr.Protection != "active" || !strings.Contains(steps(tr.Timeline), "protected") {
		t.Fatalf("after trigger: %s bracket %q protection %s steps %s", tr.Status, tr.BracketOrderID, tr.Protection, steps(tr.Timeline))
	}
	px.Store(3305.0)
	v.Tick(ctx)
	e.RefreshTrade(ctx, tr.ID)
	if tr, _ = e.Store.Trade(tr.ID); tr.Status != "closed" || tr.CloseReason != "take profit hit" || tr.PnLUSD == nil || *tr.PnLUSD <= 0 {
		t.Fatalf("after TP: %s %q %v", tr.Status, tr.CloseReason, tr.PnLUSD)
	}
}

func TestStopBuyWithTrailingStop(t *testing.T) {
	e, _, v, px := paperSetup(t)
	ctx := context.Background()
	in := intent()
	in.SizeUSD, in.Style, in.StopPrice, in.ProtectMode, in.TrailPct = 5, "stop", 3030, "trailing", 2
	in.TakeProfit, in.StopLoss = 3120, 2990
	res := e.Propose(ctx, in)
	if res.Trade == nil {
		t.Fatalf("propose: %+v", res)
	}
	px.Store(3031.0)
	v.Tick(ctx)
	e.RefreshTrade(ctx, res.Trade.ID)
	px.Store(3400.0) // new high: the trailing stop follows it up
	v.Tick(ctx)
	e.RefreshTrade(ctx, res.Trade.ID)
	if tr, _ := e.Store.Trade(res.Trade.ID); tr.Status != "filled" {
		t.Fatalf("closed too early: %s", tr.Status)
	}
	px.Store(3330.0) // 2.1% under the 3400 high
	v.Tick(ctx)
	e.RefreshTrade(ctx, res.Trade.ID)
	tr, _ := e.Store.Trade(res.Trade.ID)
	if tr.Status != "closed" || tr.CloseReason != "trailing stop hit" || tr.PnLUSD == nil || *tr.PnLUSD <= 0 {
		t.Fatalf("trailing: %s %q %v", tr.Status, tr.CloseReason, tr.PnLUSD)
	}
}

func TestStyleRules(t *testing.T) {
	e, _ := setup(t)
	cases := []struct {
		name string
		set  func(in *intentT)
	}{
		{"limit above the price", func(in *intentT) { in.Style, in.EntryLimit = "limit", 3100 }},
		{"twap shorter than 5 min", func(in *intentT) { in.Style, in.TwapMinutes = "twap", 2 }},
		{"stop below the price", func(in *intentT) { in.Style, in.StopPrice = "stop", 2900 }},
		{"trailing on a market entry", func(in *intentT) { in.ProtectMode, in.TrailPct = "trailing", 2 }},
		{"unknown style", func(in *intentT) { in.Style = "yolo" }},
	}
	for _, c := range cases {
		in := intent()
		c.set(&in)
		if res := e.Propose(context.Background(), in); res.Trade != nil || res.Decision.Code != "ORDER_TYPE" {
			t.Fatalf("%s: expected ORDER_TYPE, got %+v", c.name, res.Decision)
		}
	}
}
