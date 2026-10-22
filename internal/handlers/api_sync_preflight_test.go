package handlers

import (
	"encoding/json"
	"testing"
)

func TestCountJSONArray(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
		bad  bool
	}{
		{"vuoto", `[]`, 0, false},
		{"null", `null`, 0, false},
		{"assente", ``, 0, false},
		{"scalari", `[1,2,3]`, 3, false},
		{"annidati", `[{"a":[1,2]},{"b":{"c":3}}]`, 2, false},
		{"non array", `{}`, 0, true},
		{"non chiuso", `[1,2`, 0, true},
		{"extra", `[] []`, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := countJSONArray(json.RawMessage(c.raw))
			if c.bad {
				if err == nil {
					t.Fatal("errore atteso")
				}
				return
			}
			if err != nil {
				t.Fatalf("errore inatteso: %v", err)
			}
			if got != c.want {
				t.Fatalf("count = %d, atteso %d", got, c.want)
			}
		})
	}
}
