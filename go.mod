module notes-server

go 1.23

require (
	github.com/golang-jwt/jwt/v5 v5.2.2
	github.com/google/uuid v1.6.0
	github.com/tursodatabase/go-libsql v0.0.0-20260424063416-3051e37e6e04
	golang.org/x/crypto v0.31.0
)

require (
	github.com/antlr4-go/antlr/v4 v4.13.0 // indirect
	github.com/libsql/sqlite-antlr4-parser v0.0.0-20240327125255-dbf53b6cbf06 // indirect
	golang.org/x/exp v0.0.0-20230515195305-f3d0a9c9a5cc // indirect
)

// NB: le direttive `replace` che fissavano golang.org/x/crypto e
// golang.org/x/sys a v0.21.0/v0.18.0 (mirror GitHub) sono state rimosse: quelle
// versioni contengono vulnerabilità note (es. GO-2024-3321 in x/crypto).
// Dopo questa modifica eseguire UNA VOLTA `go mod tidy` per rigenerare go.sum.
replace golang.org/x/exp => github.com/golang/exp v0.0.0-20230515195305-f3d0a9c9a5cc

replace golang.org/x/sync => github.com/golang/sync v0.6.0

replace gotest.tools => github.com/gotestyourself/gotest.tools v2.2.0+incompatible
