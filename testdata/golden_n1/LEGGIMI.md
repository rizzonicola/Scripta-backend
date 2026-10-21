# Golden N-1 (da registrare)

Questa cartella deve contenere le richieste e risposte del server **2.1.0 non
modificato**, registrate con:

    scripts/record_golden_n1.sh /percorso/Scripta-backend-main-p5.zip

oppure, a mano, nello zip originale:

    UPDATE_GOLDEN=1 go test -run TestGoldenN1 -count=1 .

Vanno committate **prima** di applicare la patch di P6. Finché mancano, `TestGoldenN1`
fallisce con un messaggio esplicito: è voluto (un golden registrato dal server già
modificato non proverebbe nulla). Il confronto è «contiene»: campi nuovi sono ammessi,
campi tolti o cambiati no.
