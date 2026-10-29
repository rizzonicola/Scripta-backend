# Golden contract

Questa cartella contiene richieste e risposte del server, registrate con:

    bash scripts/record_golden_contract.sh

oppure, a mano:

    UPDATE_GOLDEN=1 go test -run TestGoldenContract -count=1 .

Vanno committate insieme al codice. Il confronto è «contiene»: campi nuovi sono
ammessi, campi tolti o cambiati no.
