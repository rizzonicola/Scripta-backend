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
		{"assente = default 1h", "", 1 * time.Hour},
		{"720h", "720h", 720 * time.Hour},
		{"12h", "12h", 12 * time.Hour},
		{"non valido = default", "tanto", 1 * time.Hour},
		{"negativo = default", "-5h", 1 * time.Hour},
		{"zero = default", "0s", 1 * time.Hour},
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

func TestRefreshTTLFromEnvironment(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"assente = default 90g", "", 90 * 24 * time.Hour},
		{"2160h", "2160h", 2160 * time.Hour},
		{"non valido = default", "tanto", 90 * 24 * time.Hour},
		{"negativo = default", "-5h", 90 * 24 * time.Hour},
		{"zero = default", "0s", 90 * 24 * time.Hour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			validEnv(t)
			t.Setenv("REFRESH_TOKEN_TTL", c.env)
			cfg, err := loadConfig()
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.RefreshTTL != c.want {
				t.Fatalf("RefreshTTL = %s, atteso %s", cfg.RefreshTTL, c.want)
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
	if !strings.Contains(string(raw), `REFRESH_TOKEN_TTL: "${REFRESH_TOKEN_TTL:-}"`) {
		t.Fatal("docker-compose.yml non inoltra REFRESH_TOKEN_TTL al container")
	}
	env, err := os.ReadFile(".env.example")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "JWT_TTL") {
		t.Fatal(".env.example non documenta JWT_TTL")
	}
	if !strings.Contains(string(env), "REFRESH_TOKEN_TTL") {
		t.Fatal(".env.example non documenta REFRESH_TOKEN_TTL")
	}
}
