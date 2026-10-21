package main

// Y-13-A (lato server): JWT_TTL configurabile e passata al container.

import (
	"os"
	"strings"
	"testing"
	"time"
)

func validEnv(t *testing.T) {
	t.Helper()
	t.Setenv("JWT_SECRET", testSecret)
	t.Setenv("ADMIN_USER", "root")
	t.Setenv("ADMIN_PASS", "a-very-long-admin-pass")
}

func TestJWTTTLFromEnvironment(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"assente = default 24h", "", 24 * time.Hour},
		{"720h", "720h", 720 * time.Hour},
		{"12h", "12h", 12 * time.Hour},
		{"non valido = default", "tanto", 24 * time.Hour},
		{"negativo = default", "-5h", 24 * time.Hour},
		{"zero = default", "0s", 24 * time.Hour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			validEnv(t)
			t.Setenv("JWT_TTL", c.env)
			cfg, err := loadConfig()
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.JWTTTL != c.want {
				t.Fatalf("JWTTTL = %s, atteso %s", cfg.JWTTTL, c.want)
			}
		})
	}
}

// docker-compose.yml deve inoltrare JWT_TTL al container (con default vuoto),
// altrimenti chi usa compose non può cambiarla.
func TestComposePassesJWTTTL(t *testing.T) {
	raw, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `JWT_TTL: "${JWT_TTL:-}"`) {
		t.Fatal("docker-compose.yml non inoltra JWT_TTL al container")
	}
	env, err := os.ReadFile(".env.example")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "JWT_TTL") {
		t.Fatal(".env.example non documenta JWT_TTL")
	}
}
