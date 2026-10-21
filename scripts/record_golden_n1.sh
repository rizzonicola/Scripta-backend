#!/usr/bin/env bash
# Registra i golden N-1 dal server 2.1.0 NON modificato.
# Uso: scripts/record_golden_n1.sh /percorso/dello/zip-originale.zip
# Poi committare testdata/golden_n1/*.json PRIMA di applicare la patch di P6.
set -euo pipefail

zip_originale="${1:?Uso: $0 <Scripta-backend-main-p5.zip originale>}"
qui="$(cd "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

unzip -q "$zip_originale" -d "$tmp"
radice="$(find "$tmp" -maxdepth 1 -mindepth 1 -type d | head -n 1)"
cp "$qui/golden_n1_test.go" "$radice/golden_n1_test.go"
(cd "$radice" && UPDATE_GOLDEN=1 go test -run TestGoldenN1 -count=1 .)
mkdir -p "$qui/testdata/golden_n1"
cp "$radice"/testdata/golden_n1/*.json "$qui/testdata/golden_n1/"
echo "Golden registrati in testdata/golden_n1/ dal server originale."
