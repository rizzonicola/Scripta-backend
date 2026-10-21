package main

// Y-01: cartelle del batch con il figlio PRIMA del genitore.
//
// Prima della correzione un batch [figlio(parent=P nuovo), P] finiva in 422
// (il trigger di ownership non trova P) e il client «riparava» spostando il
// figlio in radice. Ora il server differisce il figlio e lo applica dopo P.

import (
	"encoding/json"
	"testing"
	"time"
)

func y01Folder(id string, parent any) map[string]any {
	return map[string]any{"id": id, "name": "Cartella " + id, "parent_id": parent, "updated_at": time.Now().Add(-time.Hour).UnixMilli(), "deleted_at": nil}
}

type y01Pull struct {
	Folders []struct {
		ID       string  `json:"id"`
		ParentID *string `json:"parent_id"`
	} `json:"folders"`
	Rejected []struct {
		Kind   string `json:"kind"`
		ID     string `json:"id"`
		Reason string `json:"reason"`
	} `json:"rejected"`
	ServerTime int64 `json:"server_time"`
}

// y01Parents restituisce id→parent_id ("" = radice) dello stato completo del
// server (pull con cursore 0, nessun push).
func y01Parents(t *testing.T, e *testEnv, tok string) map[string]string {
	t.Helper()
	code, _, body := e.sync(tok, 0, nil, nil)
	if code != 200 {
		t.Fatalf("pull completa: %d %s", code, body)
	}
	var r y01Pull
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range r.Folders {
		if f.ParentID == nil {
			out[f.ID] = ""
		} else {
			out[f.ID] = *f.ParentID
		}
	}
	return out
}

func TestY01ChildBeforeNewParentIsAccepted(t *testing.T) {
	e := newEnv(t)
	tok := e.login("mario")

	code, _, body := e.sync(tok, 0, []map[string]any{y01Folder("figlio", "padre"), y01Folder("padre", nil)}, nil)
	if code != 200 {
		t.Fatalf("atteso 200, ottenuto %d (%s)", code, body)
	}
	got := y01Parents(t, e, tok)
	if got["figlio"] != "padre" || got["padre"] != "" || len(got) != 2 {
		t.Fatalf("gerarchia non preservata: %+v", got)
	}
}

func TestY01ReverseChainOfThree(t *testing.T) {
	e := newEnv(t)
	tok := e.login("mario")

	// Ordine peggiore: dal più profondo al radice.
	code, _, body := e.sync(tok, 0, []map[string]any{y01Folder("c", "b"), y01Folder("b", "a"), y01Folder("a", nil)}, nil)
	if code != 200 {
		t.Fatalf("atteso 200, ottenuto %d (%s)", code, body)
	}
	got := y01Parents(t, e, tok)
	if got["c"] != "b" || got["b"] != "a" || got["a"] != "" || len(got) != 3 {
		t.Fatalf("gerarchia non preservata: %+v", got)
	}
}

// Ordine «giusto» e ordine «sbagliato» devono portare allo stesso stato
// finale; e rinviare lo stesso batch è idempotente (stato invariato).
func TestY01OrderDoesNotChangeFinalStateAndResendIsIdempotent(t *testing.T) {
	parentsFirst := []map[string]any{y01Folder("p", nil), y01Folder("q", "p"), y01Folder("r", "q")}
	childrenFirst := []map[string]any{y01Folder("r", "q"), y01Folder("q", "p"), y01Folder("p", nil)}

	e1 := newEnv(t)
	tok1 := e1.login("mario")
	if code, _, b := e1.sync(tok1, 0, parentsFirst, nil); code != 200 {
		t.Fatalf("ordine corretto: %d %s", code, b)
	}
	want := y01Parents(t, e1, tok1)

	e2 := newEnv(t)
	tok2 := e2.login("mario")
	if code, _, b := e2.sync(tok2, 0, childrenFirst, nil); code != 200 {
		t.Fatalf("ordine inverso: %d %s", code, b)
	}
	got := y01Parents(t, e2, tok2)
	if len(got) != len(want) {
		t.Fatalf("stati diversi: %+v contro %+v", got, want)
	}
	for id, p := range want {
		if got[id] != p {
			t.Fatalf("stati diversi su %s: %q contro %q", id, got[id], p)
		}
	}

	// Rinvio identico (rete caduta dopo il commit): nessun errore, stato uguale.
	if code, _, b := e2.sync(tok2, 0, childrenFirst, nil); code != 200 {
		t.Fatalf("rinvio: %d %s", code, b)
	}
	again := y01Parents(t, e2, tok2)
	for id, p := range want {
		if again[id] != p {
			t.Fatalf("il rinvio ha cambiato %s: %q contro %q", id, again[id], p)
		}
	}
}

// Un genitore realmente mancante resta un 422, come prima (stesso formato).
func TestY01MissingParentIsStillRejected(t *testing.T) {
	e := newEnv(t)
	tok := e.login("mario")

	code, _, body := e.sync(tok, 0, []map[string]any{y01Folder("figlio", "genitore-che-non-esiste")}, nil)
	if code != 422 {
		t.Fatalf("atteso 422, ottenuto %d (%s)", code, body)
	}
	var r y01Pull
	_ = json.Unmarshal(body, &r)
	if len(r.Rejected) != 1 || r.Rejected[0].ID != "figlio" || r.Rejected[0].Kind != "folder" || r.Rejected[0].Reason != "invalid_parent_id" {
		t.Fatalf("rejected inatteso: %s", body)
	}
	if r.ServerTime != 0 {
		t.Fatal("un batch rifiutato non deve restituire server_time")
	}
	if got := y01Parents(t, e, tok); len(got) != 0 {
		t.Fatalf("nessuna cartella doveva essere salvata: %+v", got)
	}
}

// Ciclo fra cartelle NUOVE (A→B→A): esito deciso = 422 con entrambe le
// cartelle rifiutate e rollback completo; nessun ciclo, nessun loop.
func TestY01NewCycleIsRejectedAndRolledBack(t *testing.T) {
	e := newEnv(t)
	tok := e.login("mario")

	done := make(chan struct{})
	var code int
	var body []byte
	go func() {
		defer close(done)
		code, _, body = e.sync(tok, 0, []map[string]any{y01Folder("a", "b"), y01Folder("b", "a")}, nil)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("la sync con un ciclo non termina")
	}
	if code != 422 {
		t.Fatalf("atteso 422, ottenuto %d (%s)", code, body)
	}
	var r y01Pull
	_ = json.Unmarshal(body, &r)
	ids := map[string]bool{}
	for _, it := range r.Rejected {
		if it.Reason != "invalid_parent_id" || it.Kind != "folder" {
			t.Fatalf("rifiuto inatteso: %+v", it)
		}
		ids[it.ID] = true
	}
	if !ids["a"] || !ids["b"] || len(ids) != 2 {
		t.Fatalf("attese a e b rifiutate, trovato %v (%s)", ids, body)
	}
	if got := y01Parents(t, e, tok); len(got) != 0 {
		t.Fatalf("rollback incompleto: %+v", got)
	}
}

// Il genitore è nel batch ma è a sua volta invalido: nessun salvataggio
// parziale, entrambe le cartelle risultano rifiutate.
func TestY01ParentInBatchButItselfInvalidIsRejected(t *testing.T) {
	e := newEnv(t)
	tok := e.login("mario")

	code, _, body := e.sync(tok, 0, []map[string]any{y01Folder("figlio", "padre"), y01Folder("padre", "inesistente")}, nil)
	if code != 422 {
		t.Fatalf("atteso 422, ottenuto %d (%s)", code, body)
	}
	var r y01Pull
	_ = json.Unmarshal(body, &r)
	ids := map[string]bool{}
	for _, it := range r.Rejected {
		ids[it.ID] = true
	}
	if !ids["figlio"] || !ids["padre"] || len(ids) != 2 {
		t.Fatalf("attese figlio e padre rifiutate: %s", body)
	}
	if got := y01Parents(t, e, tok); len(got) != 0 {
		t.Fatalf("salvataggio parziale: %+v", got)
	}
}

// Una nota in una cartella nuova inviata in coda al figlio-prima-del-padre.
func TestY01NoteInDeferredFolderIsAccepted(t *testing.T) {
	e := newEnv(t)
	tok := e.login("mario")

	code, _, body := e.sync(tok, 0,
		[]map[string]any{y01Folder("figlio", "padre"), y01Folder("padre", nil)},
		[]map[string]any{note("n1", "dentro", time.Now().Add(-time.Hour).UnixMilli(), "figlio")})
	if code != 200 {
		t.Fatalf("atteso 200, ottenuto %d (%s)", code, body)
	}
}
