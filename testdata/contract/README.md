# Fixture di contratto della sync

Richieste e risposte di `POST /api/v1/sync`, **condivise** fra server e app:

- il server le confronta con le risposte reali (`contract_fixtures_test.go`);
- l'app deve copiarle in `test/fixtures/contract/` e usarle per `SyncResponse.fromJson`
  e per la gestione di 422 e 503 (fase app di P6).

Se un lato cambia un campo, i test falliscono. Le fixture sono ricavate dal codice
(`models.go`, `api_sync.go`), non da una spec. `sync_response_503.json` non è prodotta
da un test del server (richiede una contesa reale su SQLite): la sua forma e l'header
`Retry-After: 1` (`sync_response_503.headers.json`) vengono da `process` in `api_sync.go`.

Il valore di `server_time` (`1700000000999`) è un segnaposto: i test lo ignorano.
`sync_request.json` contiene una nota cancellata (`n-2`) che la prima sync (cursore 0)
non restituisce: dopo un full resync o con cursore 0 i tombstone non vengono inviati.
