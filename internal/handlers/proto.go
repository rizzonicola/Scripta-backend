package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"notes-server/internal/models"
)

// setProtoHeader aggiunge X-Scripta-Proto alla risposta (Y-14).
func setProtoHeader(w http.ResponseWriter) {
	w.Header().Set(models.ProtoHeader, strconv.Itoa(models.ServerCaps.Proto))
}

// maxClientTagLen limita quanto dell'header X-Scripta-Client finisce nei log.
const maxClientTagLen = 64

// clientTag legge X-Scripta-Client (es. "scripta/1.0.0") PER I LOG: il server
// non cambia comportamento in base a questo valore. Il valore arriva dal
// client e quindi non è fidato: si tengono solo i caratteri ASCII stampabili
// (niente a-capo né sequenze di escape, per evitare righe di log false) e si
// tronca. Vuoto diventa "-".
func clientTag(r *http.Request) string {
	raw := r.Header.Get("X-Scripta-Client")
	var b strings.Builder
	for _, c := range raw {
		if c >= 0x20 && c <= 0x7e {
			b.WriteRune(c)
			if b.Len() >= maxClientTagLen {
				break
			}
		}
	}
	if b.Len() == 0 {
		return "-"
	}
	return b.String()
}
