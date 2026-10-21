package handlers

import (
	"testing"

	"notes-server/internal/models"
)

func fdto(id string, parent string) models.FolderDTO {
	f := models.FolderDTO{ID: id, Name: id}
	if parent != "" {
		p := parent
		f.ParentID = &p
	}
	return f
}

func ids(fs []models.FolderDTO) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.ID)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestOrderDeferredFoldersParentsBeforeChildren(t *testing.T) {
	// c→b→a con a fuori dalle differite (già applicato): b non dipende da
	// nessuno, c dipende da b.
	ordered, cyclic := orderDeferredFolders([]models.FolderDTO{fdto("c", "b"), fdto("b", "a")})
	if len(cyclic) != 0 || !equalStrings(ids(ordered), []string{"b", "c"}) {
		t.Fatalf("ordine inatteso: %v, cicli %v", ids(ordered), ids(cyclic))
	}
}

func TestOrderDeferredFoldersIsStableForIndependentItems(t *testing.T) {
	ordered, cyclic := orderDeferredFolders([]models.FolderDTO{fdto("x", "p1"), fdto("y", "p2"), fdto("z", "p3")})
	if len(cyclic) != 0 || !equalStrings(ids(ordered), []string{"x", "y", "z"}) {
		t.Fatalf("l'ordine di arrivo non è preservato: %v", ids(ordered))
	}
}

func TestOrderDeferredFoldersDetectsCycles(t *testing.T) {
	// a↔b è un ciclo; d dipende dal ciclo; e è libero.
	ordered, cyclic := orderDeferredFolders([]models.FolderDTO{fdto("a", "b"), fdto("b", "a"), fdto("d", "a"), fdto("e", "fuori")})
	if !equalStrings(ids(ordered), []string{"e"}) {
		t.Fatalf("ordinate inattese: %v", ids(ordered))
	}
	if !equalStrings(ids(cyclic), []string{"a", "b", "d"}) {
		t.Fatalf("cicli inattesi: %v", ids(cyclic))
	}
}

func TestOrderDeferredFoldersEmptyAndLongChain(t *testing.T) {
	if o, c := orderDeferredFolders(nil); o != nil || c != nil {
		t.Fatalf("attesi nil, trovati %v %v", o, c)
	}
	// Catena molto profonda in ordine inverso: nessuna ricorsione, tempo lineare.
	const n = 50000
	in := make([]models.FolderDTO, 0, n)
	for i := n; i >= 2; i-- {
		in = append(in, fdto(idOf(i), idOf(i-1)))
	}
	ordered, cyclic := orderDeferredFolders(in)
	if len(cyclic) != 0 || len(ordered) != n-1 {
		t.Fatalf("catena: ordinate %d, cicli %d", len(ordered), len(cyclic))
	}
	if ordered[0].ID != idOf(2) || ordered[len(ordered)-1].ID != idOf(n) {
		t.Fatalf("estremi inattesi: %s … %s", ordered[0].ID, ordered[len(ordered)-1].ID)
	}
}

func idOf(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "f0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{digits[i%10]}, b...)
	}
	return "f" + string(b)
}
