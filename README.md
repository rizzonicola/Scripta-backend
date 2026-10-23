# Notes Server — Backend Go per app di note Markdown

Backend in Go 1.26+, SQLite/libSQL (WAL, driver `go-libsql`, CGO),
con dashboard di amministrazione web integrata e API REST per app mobile.

## Struttura del progetto

```
notes-server/
├── main.go                          # entrypoint, routing, configurazione, manutenzione
├── go.mod / go.sum
├── Dockerfile                       # multi-stage Debian bookworm, CGO_ENABLED=1
├── docker-compose.yml
├── internal/
│   ├── auth/
│   │   ├── admin_session.go         # cookie di sessione firmato per /admin
│   │   ├── jwt.go                   # JWT utente (HS256) + revoche
│   │   ├── password.go              # bcrypt hash/verify
│   │   └── username.go              # normalizzazione/validazione username
│   ├── db/
│   │   ├── db.go                    # apertura SQLite (WAL) + migrazioni + trigger
│   │   ├── users_repo.go            # repository utenti
│   │   ├── folders_repo.go          # repository cartelle
│   │   ├── notes_repo.go            # repository note (LWW, tombstone)
│   │   ├── settings_repo.go         # preferenze utente
│   │   ├── sync_state_repo.go       # cursore/high-water mark della sync
│   │   ├── revocations_repo.go      # revoche JWT persistenti
│   │   └── maintenance.go           # incremental vacuum / checkpoint WAL
│   ├── handlers/
│   │   ├── admin.go                 # dashboard admin (html/template)
│   │   ├── api_auth.go              # login/logout
│   │   ├── api_sync.go              # POST /api/v1/sync (batch, transazione esplicita)
│   │   ├── api_notes.go             # GET  /api/v1/notes/download
│   │   ├── settings.go              # GET/PUT /api/v1/user/settings
│   │   ├── helpers.go               # encoding/decoding JSON comune
│   │   └── proto.go                 # versione del contratto
│   ├── middleware/                  # JWT, rate limit, CSRF, gzip, header, timing, IP
│   └── models/
│       └── models.go
├── scripts/record_golden_n1.sh      # rigenera i fixture golden
├── testdata/                        # fixture di contratto e golden
└── web/templates/
    ├── admin_login.html             # login dashboard
    └── users.html                   # UI admin (utenti, reset password, eliminazione)
```

Dati persistenti (montati come volume Docker su `/data`):

```
/data/app.db                         # SQLite, modalità WAL: utenti, cartelle, note
```

Tutto vive nel database, compreso il contenuto Markdown (colonna `notes.content`):
non esistono file `.md` su disco né percorsi testuali (`relative_path`).

## Avvio locale (senza Docker)

```bash
go build -o notes-server .
JWT_SECRET="una-stringa-segreta-lunga-almeno-32-caratteri" \
ADMIN_USER=admin ADMIN_PASS=supersegreta \
./notes-server
```

Il server parte su `:8080`. Dashboard admin: `http://localhost:8080/admin`
(protetta da login form-based con cookie di sessione; credenziali da `ADMIN_USER` / `ADMIN_PASS`).

> Il cookie admin è `Secure`: in produzione la dashboard va servita via HTTPS
> (Cloudflare Tunnel o reverse proxy). I browser considerano `localhost` un
> contesto sicuro, quindi lo sviluppo locale su `http://localhost` funziona;
> un accesso via IP/LAN in HTTP semplice invece non conserva il cookie.

## Avvio con Docker

```bash
docker compose up -d --build
```

Il volume Docker `notes-data` persiste il database SQLite (utenti, cartelle e note, contenuto compreso). `JWT_SECRET`, `ADMIN_USER` e `ADMIN_PASS` sono **obbligatori** (il server non parte senza, né con valori d'esempio): impostarli in un file `.env` accanto a `docker-compose.yml` (vedi `.env.example`) prima di andare in produzione.

## Variabili d'ambiente

| Variabile        | Default                | Descrizione                                   |
|-------------------|-------------------------|------------------------------------------------|
| `DB_PATH`         | `data/app.db`           | Percorso file SQLite (contiene TUTTI i dati: utenti, cartelle, note) |
| `JWT_SECRET`      | **obbligatorio** (≥32 car.) | Chiave HMAC per firma JWT. Nessun default: valori mancanti/d'esempio bloccano l'avvio |
| `JWT_TTL`         | `24h`                   | Durata di validità dei JWT utente (formato Go `time.Duration`, es. `12h`, `720h`). Massimo `720h` (30 giorni): oltre, l'avvio viene rifiutato perché supererebbe la retention delle revoche. Vuota o non valida = default |
| `SYNC_MAX_BODY_BYTES` | `67108864` (64 MiB) | Tetto sul corpo di `POST /api/v1/sync`; oltre risponde `413` |
| `ADMIN_SESSION_TTL` | `8h`                  | Durata del cookie di sessione admin (`time.Duration`) |
| `ADMIN_USER`      | **obbligatorio**        | Username login dashboard `/admin`             |
| `ADMIN_PASS`      | **obbligatorio** (≥12 car.) | Password login dashboard `/admin` (nessun default) |
| `TRUSTED_PROXIES` | *(vuoto)*               | IP/CIDR dei reverse proxy fidati, separati da virgola. Solo da questi peer sono creduti `Cf-Connecting-IP` / `X-Forwarded-For` (rate limit per IP reale). Vuoto = si usa sempre l'IP del peer TCP |
| `TURSO_SYNC_URL`  | *(vuoto)*               | Primario libSQL/Turso per la modalità embedded replica (`libsql://`, `https://`, `wss://`; `http://`/`ws://` solo loopback). Vuoto = database locale puro |
| `TURSO_AUTH_TOKEN`| *(vuoto)*               | Token di autenticazione per `TURSO_SYNC_URL` |
| `TURSO_SYNC_INTERVAL` | `0`                 | Intervallo di sync automatica della replica (`time.Duration`); `0` = sync solo all'apertura |
| `PORT`            | `8080`                  | Porta HTTP                                    |

---

## Specifica API REST (v1)

Base URL: `http://<host>:8080/api/v1`

Tutte le risposte sono in JSON (`Content-Type: application/json; charset=utf-8`), tranne il
download delle note (`text/markdown; charset=utf-8`). Timestamp di sincronizzazione
(`updated_at`, `deleted_at`, `last_synced_at`, `server_time`): **millisecondi** Unix.

> **La fonte di verità è il codice** (`internal/handlers/api_sync.go`, `internal/models/models.go`),
> non questo documento: se divergono, vale il codice.

### 1. `POST /api/v1/auth/login`

Autentica un utente con username/password e rilascia un JWT.

**Request body**
```json
{
  "username": "mario.rossi",
  "password": "password-in-chiaro"
}
```

**Response 200**
```json
{
  "token": "eyJhbGciOiJIUzI1NiIs...",
  "expires_at": 1767225600,
  "user_id": "b3f1c2...",
  "username": "mario.rossi"
}
```

`expires_at` è in **secondi** Unix (a differenza dei timestamp di sync, in millisecondi).

**Errori**: `400` (campi mancanti), `401` (credenziali non valide).

Il token va incluso in tutte le chiamate successive come:
```
Authorization: Bearer <token>
```

---

### 2. `POST /api/v1/sync`

*(richiede JWT)*

L'unico endpoint di sincronizzazione. Protocollo **delta sync** local-first, interamente
**basato su ID** (nessun percorso testuale, nessun file): il client invia le cartelle e le note
modificate localmente (flag «dirty») e riceve ciò che è cambiato sul server. I conflitti si
risolvono **riga per riga** con **Last-Write-Wins** su `updated_at`.

**Request body**
```json
{
  "last_synced_at": 1756900000000,
  "folders": [
    { "id": "f-1", "name": "Lavoro", "parent_id": null, "updated_at": 1756900000000, "deleted_at": null }
  ],
  "notes": [
    {
      "id": "n-1", "title": "Riunione", "content": "# Riunione\n\n...",
      "folder_id": "f-1", "is_favorite": false, "is_pinned": false, "order_index": 0,
      "updated_at": 1756900000000, "deleted_at": null
    }
  ]
}
```

- `last_synced_at`: il cursore del client = il `server_time` dell'ultimo giro riuscito (`0` alla prima sync).
- `folders` / `notes`: solo le entità **sporche**. `deleted_at` non nullo = cancellazione (tombstone).
- Limiti: `id` ≤ 64 caratteri senza caratteri di controllo; `name` ≤ 255 caratteri (vuoto → «Senza nome»);
  `title` ≤ 1024 caratteri; al massimo 100000 elementi per richiesta.

**Cosa fa il server** (in un'unica transazione):
1. **Push**: per ogni elemento, upsert condizionato: ha effetto solo se `updated_at` del client è
   **≥** di quello memorizzato. Un `updated_at` oltre «adesso + 5 minuti» viene riportato all'ora del server.
2. **Cascata**: se una cartella risulta cancellata, la cancellazione si propaga a sottocartelle e note ancora attive.
3. **Pull**: restituisce le entità dell'utente con `synced_at` in `(last_synced_at, server_time]`,
   calcolate **dopo** il push. Comprende quindi anche l'**eco** di ciò che il client ha appena inviato e,
   se il client ha perso il confronto LWW, la versione vincente del server.
   `synced_at` è lo stamp **del server** (monotono, e non scende nemmeno dopo un riavvio con l'orologio
   indietro), distinto dall'`updated_at` del client, che serve solo al LWW.

**Ordine delle cartelle**: il genitore deve esistere quando si scrive il figlio. Il server accetta anche
un batch con il figlio **prima** del genitore, se il genitore è nello stesso batch (lo applica dopo). Un
genitore che non esiste né nel batch né sul server, o un ciclo fra cartelle nuove, è rifiutato (`422`).

**Response 200**
```json
{
  "server_time": 1756900001234,
  "full_resync": false,
  "folders": [ { "id": "f-1", "name": "Lavoro", "parent_id": null, "updated_at": 1756900000000, "deleted_at": null } ],
  "notes": [
    {
      "id": "n-1", "title": "Riunione", "content": "# Riunione\n\n...",
      "folder_id": "f-1", "is_favorite": false, "is_pinned": false, "order_index": 0,
      "updated_at": 1756900000000, "deleted_at": null
    }
  ],
  "caps": { "proto": 2 }
}
```
- `server_time`: il nuovo cursore. Il client lo salva **solo** dopo aver applicato la risposta.
- `full_resync`: `true` se il cursore è negativo, nel futuro o più vecchio di 30 giorni (retention dei
  tombstone). La risposta contiene allora lo stato **completo** (solo righe attive) e il client elimina i
  residui locali «puliti» che non compaiono.
- `caps` (additivo): capability del server. Oggi solo `proto` (versione del contratto, `2`). Una chiave
  assente vale «non supportato»; un client che non la conosce la ignora. Lo stesso valore è nell'header
  `X-Scripta-Proto` (anche su `GET /healthz`).
- **Non esistono** `accepted`, `server_wins`, `relative_path` né `deleted` (contratto precedente).
- Le cancellazioni viaggiano come tombstone (`deleted_at` valorizzato) per 30 giorni dall'ultimo `synced_at`.

**Header opzionale della richiesta**: `X-Scripta-Client: scripta/<versione>`. Serve solo ai log del
server (compare nel riepilogo dei `422`); non cambia nessun comportamento.

**Errori**

| Codice | Quando | Corpo |
|---|---|---|
| `400` | corpo non JSON | `{"error":"body JSON non valido"}` |
| `401` | token mancante, scaduto o revocato | `{"error":"..."}` |
| `405` | metodo diverso da `POST` | `{"error":"metodo non consentito"}` |
| `413` | corpo oltre `SYNC_MAX_BODY_BYTES` o più di 100000 elementi | `{"error":"payload troppo grande"}` |
| `422` | almeno un elemento rifiutato: **nessuna modifica applicata** (rollback dell'intero batch), nessun `server_time` | `{"error":"...","rejected":[{"kind":"folder\|note","id":"...","reason":"invalid_id\|invalid_parent_id\|invalid_folder_id"}]}` |
| `500` | errore interno | `{"error":"..."}` |
| `503` | database occupato; riprovare (header `Retry-After: 1`) | `{"error":"database temporaneamente occupato, riprovare"}` |

La sync è **idempotente**: rinviare lo stesso corpo (per esempio dopo una rete caduta) dà lo stesso stato finale.

---

### 3. `GET /api/v1/user/settings`

*(richiede JWT)*

Restituisce le preferenze dell'utente autenticato (tema, schema colore, lingua,
font, dimensione, interlinea, layout). Se l'utente non ha mai salvato nulla,
restituisce i valori di default senza scrivere nel DB.

**Response 200**
```json
{
  "theme": "dark",
  "color_scheme": "solarized",
  "language": "it",
  "font_family": "Inter",
  "font_size": 16,
  "line_spacing": 1.4,
  "layout": "split",
  "updated_at": 1756950000000
}
```

---

### 4. `PUT /api/v1/user/settings`

*(richiede JWT)*

Sostituisce integralmente le preferenze dell'utente (il client invia sempre lo
stato completo, non una patch parziale). Applica validazioni minime (`font_size`
tra 8 e 48, `line_spacing` tra 0.8 e 3.0) e valori di fallback ai campi vuoti.

**Request body**: stesso schema della response di `GET /api/v1/user/settings`
(il campo `updated_at` viene ignorato in input e ricalcolato dal server).

**Response 200**: le impostazioni salvate, con `updated_at` aggiornato.

**Errori**: `400` (JSON non valido o valori fuori range), `401`, `500`.

---

### 5. `GET /api/v1/notes/download`

*(richiede JWT)*

Scarica il contenuto Markdown di **una nota** dell'utente autenticato. Non fa parte del protocollo di
sync (l'app non lo usa per sincronizzare); serve a un download diretto o a un client minimale.

**Query params**
- `id` (obbligatorio): l'`id` della nota (lo stesso che compare nella sync).

**Esempio**
```
GET /api/v1/notes/download?id=n-1
Authorization: Bearer <token>
```

**Response 200**: corpo = il contenuto (`content`) della nota, con gli header
```
Content-Type: text/markdown; charset=utf-8
Content-Disposition: attachment; filename="Riunione.md"
```
Il nome del file deriva dal titolo, ripulito dai caratteri non validi nei nomi file (`/ \ : * ? " < > |`);
se resta vuoto vale `nota`.

**Errori**: `400` (`id` mancante), `401`, `404` (la nota non esiste, è di un altro utente o è cancellata:
tombstone), `405` (metodo diverso da `GET`), `500`.

> Non esiste alcun file su disco da risolvere: il contenuto è una colonna del database, quindi non c'è
> nessun rischio di *path traversal*.

---

## Dashboard Admin (`/admin`)

Protetta da un login form-based (`ADMIN_USER` / `ADMIN_PASS`) con cookie di sessione HttpOnly/Secure/SameSite=Strict, invece del popup nativo Basic Auth: i campi sono riconosciuti dai password manager e il logout (`/admin/logout`) è esplicito.

- **`GET /admin`** — elenco utenti registrati (username, ID, data creazione) + form
  di creazione nuovo utente.
- **`POST /admin/users/create`** — crea un nuovo utente. La password arriva in chiaro
  dal form HTML mostrato nel browser dell'amministratore, viene **immediatamente
  cifrata con bcrypt** (`internal/auth/password.go`) e solo l'hash viene salvato nel
  DB. La password in chiaro non viene mai loggata né mostrata in nessuna pagina.
- **`POST /admin/users/reset-password`** — imposta una nuova password per un utente
  esistente. Non è **mai** possibile visualizzare la password precedente: viene
  semplicemente sovrascritto l'hash bcrypt e vengono revocati i token già emessi.
- **`POST /admin/users/delete`** — elimina definitivamente l'utente e, tramite
  `ON DELETE CASCADE`, tutte le sue cartelle, note e impostazioni. I token
  dell'utente vengono revocati.

---

## Note implementative

- **SQLite WAL**: PRAGMA (`journal_mode(WAL)`, `busy_timeout(5000)`,
  `synchronous(NORMAL)`, `foreign_keys(ON)`) applicati su ogni connessione
  fisica del pool. In modalità locale pura il pool è limitato a 1 connessione;
  con embedded replica Turso sono ammesse più connessioni in lettura.
- **Atomicità della sync**: l'intero push (cascate comprese) avviene in una sola transazione SQLite;
  un rifiuto (`422`) o un errore annulla tutto il batch.
- **Nessun file su disco**: il contenuto delle note è nella colonna `notes.content`; non ci sono
  percorsi testuali, quindi nemmeno il rischio di path traversal.
- **JWT**: firma HMAC-SHA256, scadenza di default 24 ore (`JWT_TTL`), claims con
  `uid` (ID utente), `username` e `iat_ms` (istante di emissione al millisecondo,
  usato per il confronto con le revoche).


## Note di sicurezza e sincronizzazione (v2.1)

- **Cursore di pull server-side**: ogni riga ha `synced_at`, assegnato dal server con uno stamp monotono. Il massimo stamp emesso è persistito (tabella `sync_state`, con un margine di 60 s, scrittura al massimo una volta al minuto): dopo un riavvio con l'orologio indietro i nuovi stamp restano comunque maggiori di ogni cursore già consegnato. Il pull filtra solo su `synced_at`; `updated_at` (orologio del client) serve unicamente alla risoluzione LWW. Timestamp del client oltre *ora + 5 min* vengono riportati all'ora del server.
- **Rifiuti espliciti**: un `folder_id`/`parent_id` non valido produce `422` con l'elenco `rejected` e rollback dell'intero batch; il client non riceve `server_time` e non avanza il cursore.
- **Tombstone**: purge dopo 30 giorni dall'accettazione sul server (`synced_at`). Un client con cursore più vecchio riceve `full_resync: true` (stato completo) e ripulisce i residui locali.
- **Revoca token persistente** (`user_token_revocations`, `revoked_tokens`) + `POST /api/v1/auth/logout`.
- **Rate limit** basato sull'IP del peer; header di inoltro creduti solo da `TRUSTED_PROXIES`.
- **Header di sicurezza** (CSP con nonce sulle pagine admin, HSTS, X-Frame-Options, ecc.); i messaggi della dashboard viaggiano come codici whitelisted, non come testo in query string.
- **Migrazione**: al primo avvio vengono aggiunte le colonne mancanti (es. `synced_at`, con backfill = ora) e creati gli indici; le DDL sono idempotenti. `go.sum` è versionato: eseguire `go mod tidy` solo dopo aver modificato le dipendenze.
