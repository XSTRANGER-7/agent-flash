package store

import (
	"testing"
	"time"
)

func TestPerfEquityIgnoresDepositsAndWithdrawals(t *testing.T) {
	st, _ := Open(t.TempDir())
	st.AddEquity("w", 25)
	time.Sleep(2 * time.Millisecond)
	st.AddFlow("w", 75) // owner tops up
	st.AddEquity("w", 100)
	time.Sleep(2 * time.Millisecond)
	st.AddEquity("w", 102) // +$2 from trading
	time.Sleep(2 * time.Millisecond)
	st.AddFlow("w", -50) // owner withdraws
	st.AddEquity("w", 52)
	p := st.PerfEquity("w")
	want := []float64{100, 100, 102, 102} // capital $100, only the $2 trade P&L moves it
	for i, w := range want {
		if p[i].USD != w {
			t.Fatalf("point %d = %v, want %v (all %v)", i, p[i].USD, w, p)
		}
	}
}
