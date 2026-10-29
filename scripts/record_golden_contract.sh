#!/usr/bin/env bash
# Rigenera i golden contract dal working tree corrente.
# Uso: bash scripts/record_golden_contract.sh
set -euo pipefail

qui="$(cd "$(dirname "$0")/.." && pwd)"
(cd "$qui" && UPDATE_GOLDEN=1 go test -run TestGoldenContract -count=1 .)
echo "Golden rigenerati in testdata/golden_contract/."
