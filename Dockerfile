# --- Stage 1: build ---
# NB: go-libsql è un binding CGO che linka una libreria nativa Rust
# precompilata per linux/amd64 e linux/arm64 (github.com/tursodatabase/go-libsql/lib).
# Quella libreria è compilata contro glibc: build e runtime DEVONO essere
# glibc-based (Debian/Ubuntu). Alpine (musl) causa un mismatch del linker
# dinamico e il binario non parte ("invalid ELF header" / crash al primo
# uso del DB). Per questo la migrazione sposta anche l'immagine base.
FROM golang:1.22-bookworm AS builder

WORKDIR /src

# gcc/libc6-dev: necessari per compilare la parte CGO del driver libSQL.
RUN apt-get update -qq && apt-get install -y --no-install-recommends \
    gcc \
    libc6-dev \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum* ./
RUN go mod download

COPY . .

# CGO abilitato: go-libsql (github.com/tursodatabase/go-libsql) richiede CGO
# per collegarsi alla libreria nativa libSQL. GOOS/GOARCH sono impliciti
# (build nativa nel container); per cross-compilare verso un'altra arch è
# necessario anche il toolchain gcc corrispondente (es. gcc-aarch64-linux-gnu).
ENV CGO_ENABLED=1
RUN go build -ldflags="-s -w" -o /out/notes-server .

# --- Stage 2: runtime ---
# debian-slim (glibc) invece di alpine: vedi nota sopra sul binding nativo.
FROM debian:bookworm-slim

RUN apt-get update -qq && apt-get install -y --no-install-recommends \
    ca-certificates \
    tzdata \
    wget \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -r app && useradd -r -g app app

WORKDIR /app
COPY --from=builder /out/notes-server /app/notes-server

# Directory dati persistente: contiene solo il file .db (cartelle e note
# vivono interamente nel database, non più come file separati su disco).
RUN mkdir -p /data && chown -R app:app /data /app

USER app

ENV DB_PATH=/data/app.db
ENV PORT=8080
# Opzionali, per abilitare la modalità embedded replica verso Turso/libSQL
# server: se TURSO_SYNC_URL è vuoto il server resta in modalità file locale.
ENV TURSO_SYNC_URL=""
ENV TURSO_AUTH_TOKEN=""
ENV TURSO_SYNC_INTERVAL=""

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/app/notes-server"]
