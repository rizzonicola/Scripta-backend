# Notes Server — Backend Go per app di note Markdown

Backend in Go 1.22+, SQLite (WAL, driver puro `modernc.org/sqlite`, senza CGO),
con dashboard di amministrazione web integrata e API REST per app mobile.

## Struttura del progetto

```
notes-server/
├── main.go                          # entrypoint, routing, wiring
├── go.mod
├── Dockerfile                       # multi-stage, Alpine, CGO_ENABLED=0
├── docker-compose.yml
├── internal/
│   ├── auth/
│   │   ├── password.go              # bcrypt hash/verify
│   │   └── jwt.go                   # generazione/validazione JWT (HS256)
│   ├── db/
│   │   ├── db.go                    # apertura SQLite (WAL, synchronous=NORMAL) + migrazioni
│   │   ├── users_repo.go            # repository utenti
│   │   ├── notes_repo.go            # repository metadati note (per LWW, con supporto tx)
│   │   └── settings_repo.go         # repository preferenze utente
│   ├── handlers/
│   │   ├── admin.go                 # dashboard admin (html/template)
│   │   ├── api_auth.go              # POST /api/v1/auth/login
│   │   ├── api_sync.go              # POST /api/v1/sync (batch, transazione esplicita)
│   │   ├── api_notes.go             # GET  /api/v1/notes/download
│   │   ├── settings.go              # GET/PUT /api/v1/user/settings
│   │   └── helpers.go
│   ├── middleware/
│   │   └── auth.go                  # middleware JWT + Basic Auth admin
│   ├── models/
│   │   └── models.go
│   └── storage/
│       └── files.go                 # path sanitization, lock per-path, scrittura atomica (.tmp + rename)
└── web/templates/
    └── users.html                   # UI admin (elenco utenti, creazione, reset password)
```

Dati persistenti (montati come volume Docker su `/data`):

```
/data/app.db                         # SQLite, modalità WAL
/data/users/{user_id}/notes/{relative_path}   # file .md dell'utente
```

## Avvio locale (senza Docker)

```bash
go mod tidy      # scarica le dipendenze e genera go.sum (richiede rete)
go build -o notes-server .
JWT_SECRET="una-stringa-segreta-lunga" \
ADMIN_USER=admin ADMIN_PASS=supersegreta \
./notes-server
```

Il server parte su `:8080`. Dashboard admin: `http://localhost:8080/admin`
(protetta con HTTP Basic Auth, credenziali da `ADMIN_USER` / `ADMIN_PASS`).

> Nota: in questo ambiente di generazione del codice non è stato possibile eseguire
> `go mod tidy` / `go build` perché la rete è disabilitata (impossibile scaricare
> `modernc.org/sqlite`, `golang-jwt`, `google/uuid`, `golang.org/x/crypto`).
> Il codice è scritto e revisionato con cura ma va compilato con `go mod tidy && go build`
> in un ambiente con accesso a internet prima del primo deploy.

## Avvio con Docker

```bash
docker compose up -d --build
```

Il volume Docker `notes-data` persiste sia il database SQLite sia i file `.md` di tutti
gli utenti. Ricordarsi di **cambiare** `JWT_SECRET`, `ADMIN_USER` e `ADMIN_PASS` nel
`docker-compose.yml` prima di andare in produzione.

## Variabili d'ambiente

| Variabile        | Default                | Descrizione                                   |
|-------------------|-------------------------|------------------------------------------------|
| `DB_PATH`         | `data/app.db`           | Percorso file SQLite (contiene TUTTI i dati: utenti, cartelle, note) |
| `JWT_SECRET`      | *(insicuro, da cambiare)* | Chiave HMAC per firma JWT                   |
| `ADMIN_USER`      | `admin`                 | Username Basic Auth dashboard `/admin`        |
| `ADMIN_PASS`      | `admin`                 | Password Basic Auth dashboard `/admin`        |
| `PORT`            | `8080`                  | Porta HTTP                                    |

---

## Specifica API REST (v1)

Base URL: `http://<host>:8080/api/v1`

Tutte le risposte sono in JSON (`Content-Type: application/json`), tranne il download
delle note (`text/markdown`).

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

**Errori**: `400` (campi mancanti), `401` (credenziali non valide).

Il token va incluso in tutte le chiamate successive come:
```
Authorization: Bearer <token>
```

---

### 2. `POST /api/v1/sync`

*(richiede JWT)*

Riceve dall'app mobile un elenco di note modificate localmente e applica la
risoluzione dei conflitti **Last-Write-Wins** basata sul campo `updated_at`
(timestamp Unix in millisecondi).

**Request body**
```json
{
  "notes": [
    {
      "relative_path": "lavoro/riunione-2026-09-01.md",
      "content": "# Riunione\n\nContenuto della nota...",
      "updated_at": 1756900000000,
      "deleted": false
    },
    {
      "relative_path": "personale/vecchia-nota.md",
      "content": "",
      "updated_at": 1700000000000,
      "deleted": true
    }
  ]
}
```

**Logica di risoluzione conflitti** (per ciascuna nota inviata):
- Se la nota non esiste ancora sul server, **oppure** `updated_at` del client è
  **maggiore o uguale** a quello registrato sul server → **vince il client**:
  il file `.md` viene scritto (o cancellato, se `deleted: true`) in modo **atomico**
  (`.tmp` + `os.Rename`) nella cartella dell'utente, e i metadati vengono aggiornati.
- Se il server possiede una versione con `updated_at` **maggiore** → **vince il server**:
  la nota (con relativo contenuto letto da disco) viene restituita al client nella
  lista `server_wins`, così il client può allineare la propria copia locale.

**Response 200**
```json
{
  "accepted": [
    { "relative_path": "lavoro/riunione-2026-09-01.md", "updated_at": 1756900000000, "deleted": false }
  ],
  "server_wins": [
    {
      "relative_path": "personale/vecchia-nota.md",
      "content": "# Contenuto più recente presente sul server",
      "updated_at": 1756950000000,
      "deleted": false
    }
  ]
}
```

**Errori**: `400` (body non valido), `401` (token mancante/non valido), `500` (errore I/O o DB).

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

Scarica il contenuto grezzo (raw) di un file `.md` dell'utente autenticato.

**Query params**
- `path` (obbligatorio): percorso relativo del file, es. `lavoro/riunione.md`

**Esempio**
```
GET /api/v1/notes/download?path=lavoro/riunione-2026-09-01.md
Authorization: Bearer <token>
```

**Response 200**: corpo = contenuto grezzo del file Markdown, header:
```
Content-Type: text/markdown; charset=utf-8
Content-Disposition: attachment; filename="riunione-2026-09-01.md"
```

**Errori**: `400` (path mancante o non valido / tentativo di path traversal),
`401` (non autenticato), `404` (file non trovato).

> **Sicurezza**: il parametro `path` viene sempre risolto e validato contro la
> cartella note dell'utente autenticato (`internal/storage/files.go → ResolvePath`),
> impedendo l'accesso a file esterni tramite sequenze `../`.

---

## Dashboard Admin (`/admin`)

Protetta da HTTP Basic Auth (`ADMIN_USER` / `ADMIN_PASS`).

- **`GET /admin`** — elenco utenti registrati (username, ID, data creazione) + form
  di creazione nuovo utente.
- **`POST /admin/users/create`** — crea un nuovo utente. La password arriva in chiaro
  dal form HTML mostrato nel browser dell'amministratore, viene **immediatamente
  cifrata con bcrypt** (`internal/auth/password.go`) e solo l'hash viene salvato nel
  DB. La password in chiaro non viene mai loggata né mostrata in nessuna pagina.
- **`POST /admin/users/reset-password`** — imposta una nuova password per un utente
  esistente. Non è **mai** possibile visualizzare la password precedente: viene
  semplicemente sovrascritto l'hash bcrypt.

---

## Note implementative

- **SQLite WAL**: aperto con `_pragma=journal_mode(WAL)` e `busy_timeout(5000)`;
  `SetMaxOpenConns(1)` evita "database is locked" con scritture concorrenti.
- **Scrittura atomica dei file**: `storage.Store.AtomicWrite` scrive su un file
  temporaneo (`.tmp-*`) nella stessa directory di destinazione, esegue `fsync`,
  poi `os.Rename` — operazione atomica a livello di filesystem (POSIX).
- **Path traversal**: ogni `relative_path` fornito da client/API viene ripulito con
  `filepath.Clean` e verificato con `filepath.Rel` per assicurarsi che resti
  contenuto nella cartella note dell'utente.
- **JWT**: firma HMAC-SHA256, scadenza di default 7 giorni (`main.go`), claims con
  `user_id` e `username`.
