package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/zalando/go-keyring"
)

// TestFactorySnapshot_JiraMineIsTheViewersAccount: a Jira ticket on the belt
// is the viewer's when it is assigned to the account id their identity on the
// org's site carries. A colleague who shares the viewer's display name is not
// the viewer, and an unassigned ticket is nobody's.
func TestFactorySnapshot_JiraMineIsTheViewersAccount(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)
	seedStockConfig(t, s) // the viewer is acc-1, display name "Tester"
	seedStockTicketIn(t, s, "SKY-1", "To Do", "Tester", "acc-1")
	seedStockTicketIn(t, s, "SKY-2", "To Do", "Tester", "acc-2")
	seedStockTicketIn(t, s, "SKY-3", "To Do", "", "")

	rec := doJSON(t, s, http.MethodGet, "/api/factory/snapshot", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var snap struct {
		Entities []struct {
			ID   string `json:"id"`
			Mine bool   `json:"mine"`
		} `json:"entities"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	mine := map[string]bool{}
	for _, e := range snap.Entities {
		mine[e.ID] = e.Mine
	}
	want := map[string]bool{"e_stock_SKY-1": true, "e_stock_SKY-2": false, "e_stock_SKY-3": false}
	for id, w := range want {
		got, ok := mine[id]
		if !ok {
			t.Errorf("%s is not on the belt: %v", id, mine)
			continue
		}
		if got != w {
			t.Errorf("%s mine = %v, want %v", id, got, w)
		}
	}
}
