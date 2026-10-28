package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// jsonContentType dichiara esplicitamente UTF-8. RFC 8259 impone già UTF-8 per
// il JSON, ma alcuni client HTTP (es. package:http di Dart) decodificano un
// «application/json» senza charset con la codifica di default dello stack:
// l'header esplicito toglie ogni ambiguità ai testi con accenti ed emoji.
const jsonContentType = "application/json; charset=utf-8"

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

// writeJSONErrorCode accompagna l'errore con un codice stabile, pensato per
// i client che devono distinguere casi diversi con lo stesso status HTTP.
func writeJSONErrorCode(w http.ResponseWriter, status int, msg, code string) {
	writeJSON(w, status, errorBody{Error: msg, Code: code})
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
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "payload troppo grande")
		} else {
			writeJSONError(w, http.StatusBadRequest, "body JSON non valido")
		}
		return false
	}
	// Un solo valore JSON per richiesta: senza questo controllo un body come
	// {"username":"a"}{"password":"b"} verrebbe accettato leggendo solo il
	// primo oggetto, lasciando al client l'illusione di aver inviato altro.
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "body JSON non valido")
		return false
	}
	return true
}

// decodeOptionalJSONBody è come decodeJSONBody ma accetta anche un body
// vuoto. Serve al logout: i client precedenti non inviano alcun body, quelli
// nuovi possono inviare il refresh token per revocare la famiglia.
func decodeOptionalJSONBody(w http.ResponseWriter, r *http.Request, dst interface{}, maxBytes int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "payload troppo grande")
		} else {
			writeJSONError(w, http.StatusBadRequest, "body JSON non valido")
		}
		return false
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "body JSON non valido")
		return false
	}
	return true
}
