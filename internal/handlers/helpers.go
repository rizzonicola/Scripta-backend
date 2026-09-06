package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
)

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

type errorBody struct {
	Error string `json:"error"`
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

// decodeJSONBody decodifica il body JSON di una richiesta in dst, applicando
// un limite massimo di byte leggibili tramite http.MaxBytesReader.
//
// Perché serve: json.Decoder fa streaming dei *token* JSON, ma questo non
// impedisce a un singolo valore (es. una stringa enorme in un campo "content")
// di essere comunque interamente allocato in RAM in un colpo solo. Senza un
// tetto esplicito sulla dimensione del body, un client (malevolo o
// semplicemente bacato) può forzare allocazioni arbitrariamente grandi con
// una singola richiesta. http.MaxBytesReader interrompe la lettura non
// appena il limite viene superato, restituendo un *http.MaxBytesError che
// distinguiamo qui da un normale errore di parsing per rispondere con lo
// status HTTP corretto (413 invece di 400).
//
// Nota: modifica r.Body in place (come previsto dalla documentazione di
// http.MaxBytesReader), quindi va chiamata prima di qualunque altra lettura
// del body.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst interface{}, maxBytes int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "payload troppo grande")
		} else {
			writeJSONError(w, http.StatusBadRequest, "body JSON non valido")
		}
		return false
	}
	return true
}
