package wallet

import "testing"

// Someone could grind an address whose first 10 hex characters match a
// person's; the person still gets a wallet (a longer id), and a restart puts
// each address back under the id it was saved with.
func TestWatchIdClashFallsBackToLongID(t *testing.T) {
	r := &Registry{byID: map[string]*Wallet{}}
	squat, err := r.Watch("0x1234567890aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil || squat.ID != "self-1234567890" {
		t.Fatalf("first: %v %v", squat, err)
	}
	victim, err := r.Watch("0x1234567890bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err != nil || victim.ID != "self-1234567890bbbbbbbbbbbbbbbbb" || len(victim.ID) > 32 {
		t.Fatalf("clash: %v %v", victim, err)
	}
	if again, _ := r.Watch("0x1234567890bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); again != victim {
		t.Fatal("not idempotent")
	}
	// Restart in the other order: ids come from the saved list, not the order.
	r2 := &Registry{byID: map[string]*Wallet{}}
	if w, err := r2.WatchID(victim.ID, victim.Address()); err != nil || w.ID != victim.ID {
		t.Fatalf("restore victim: %v %v", w, err)
	}
	if w, err := r2.WatchID(squat.ID, squat.Address()); err != nil || w.ID != squat.ID {
		t.Fatalf("restore squatter: %v %v", w, err)
	}
	if _, err := r2.WatchID("self-1234567890", victim.Address()); err == nil {
		t.Fatal("an id was restored for an address it does not belong to")
	}
}
