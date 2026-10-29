package main

import (
	"encoding/json"
	"testing"
)

// Un client precedente non conosce created_at: il suo push deve aggiornare la
// nota senza cancellare la data di creazione già nota al server.
func TestOldClientPushKeepsServerCreatedAt(t *testing.T) {
	e := newEnv(t)
	tok := e.login("mario")

	newClient := note("n-created", "titolo", 2000, nil)
	newClient["created_at"] = int64(1000)
	if code, _, body := e.sync(tok, 0, nil, []map[string]any{newClient}); code != 200 {
		t.Fatalf("push client nuovo: %d %s", code, body)
	}

	oldClient := note("n-created", "titolo aggiornato", 3000, nil)
	code, _, body := e.sync(tok, 0, nil, []map[string]any{oldClient})
	if code != 200 {
		t.Fatalf("push client vecchio: %d %s", code, body)
	}

	var resp struct {
		Notes []struct {
			ID        string `json:"id"`
			CreatedAt int64  `json:"created_at"`
		} `json:"notes"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("risposta non JSON: %v (%s)", err, body)
	}
	for _, n := range resp.Notes {
		if n.ID == "n-created" {
			if n.CreatedAt != 1000 {
				t.Fatalf("created_at = %d, atteso 1000", n.CreatedAt)
			}
			return
		}
	}
	t.Fatalf("nota n-created assente dalla risposta: %s", body)
}
