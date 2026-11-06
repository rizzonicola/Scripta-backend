// Package httpx contiene le primitive HTTP condivise da handler e middleware:
// un solo posto in cui vivono lo schema JSON di errore, il content type e la
// decodifica dei body con limite di dimensione.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// JSONContentType dichiara esplicitamente UTF-8. RFC 8259 impone già UTF-8 per
// il JSON, ma alcuni client HTTP (es. package:http di Dart) decodificano un
// «application/json» senza charset con la codifica di default dello stack:
// l'header esplicito toglie ogni ambiguità ai testi con accenti ed emoji.
const JSONContentType = "application/json; charset=utf-8"

// ErrorBody è lo schema comune delle risposte di errore. Code è opzionale e
// serve ai client che devono distinguere casi diversi con lo stesso status.
type ErrorBody struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// WriteJSON scrive payload come JSON con lo status indicato.
func WriteJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", JSONContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// WriteError scrive una risposta di errore JSON senza codice.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, ErrorBody{Error: msg})
}

// WriteErrorCode accompagna l'errore con un codice stabile.
func WriteErrorCode(w http.ResponseWriter, status int, msg, code string) {
	WriteJSON(w, status, ErrorBody{Error: msg, Code: code})
}

// DecodeJSONBody decodifica il body JSON di una richiesta in dst, applicando
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
func DecodeJSONBody(w http.ResponseWriter, r *http.Request, dst interface{}, maxBytes int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		writeDecodeError(w, err)
		return false
	}
	// Un solo valore JSON per richiesta: senza questo controllo un body come
	// {"username":"a"}{"password":"b"} verrebbe accettato leggendo solo il
	// primo oggetto, lasciando al client l'illusione di aver inviato altro.
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		WriteError(w, http.StatusBadRequest, "body JSON non valido")
		return false
	}
	return true
}

// DecodeOptionalJSONBody è come DecodeJSONBody ma accetta anche un body
// vuoto. Il logout può essere inviato senza body; se il client conosce il
// refresh token lo include per revocare anche la famiglia.
func DecodeOptionalJSONBody(w http.ResponseWriter, r *http.Request, dst interface{}, maxBytes int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		writeDecodeError(w, err)
		return false
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		WriteError(w, http.StatusBadRequest, "body JSON non valido")
		return false
	}
	return true
}

func writeDecodeError(w http.ResponseWriter, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		WriteError(w, http.StatusRequestEntityTooLarge, "payload troppo grande")
		return
	}
	WriteError(w, http.StatusBadRequest, "body JSON non valido")
}
