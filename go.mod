module notes-server

go 1.22

require (
	github.com/golang-jwt/jwt/v5 v5.2.1
	github.com/google/uuid v1.6.0
	github.com/tursodatabase/go-libsql v0.0.0-20260424063416-3051e37e6e04
	golang.org/x/crypto v0.21.0
)

require (
	github.com/antlr4-go/antlr/v4 v4.13.0 // indirect
	github.com/libsql/sqlite-antlr4-parser v0.0.0-20240327125255-dbf53b6cbf06 // indirect
	golang.org/x/exp v0.0.0-20230515195305-f3d0a9c9a5cc // indirect
)

replace golang.org/x/crypto => github.com/golang/crypto v0.21.0

replace golang.org/x/sys => github.com/golang/sys v0.18.0

replace golang.org/x/exp => github.com/golang/exp v0.0.0-20230515195305-f3d0a9c9a5cc

replace golang.org/x/sync => github.com/golang/sync v0.6.0

replace gotest.tools => github.com/gotestyourself/gotest.tools v2.2.0+incompatible
