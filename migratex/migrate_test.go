package migratex

import "testing"

func TestPgxURL(t *testing.T) {
	tests := map[string]string{
		"postgres://u:p@h:5432/db?sslmode=require":   "pgx5://u:p@h:5432/db?sslmode=require",
		"postgresql://u:p@h:5432/db?sslmode=disable": "pgx5://u:p@h:5432/db?sslmode=disable",
		"pgx5://u:p@h:5432/db":                       "pgx5://u:p@h:5432/db",
		"mysql://u:p@h:3306/db":                      "mysql://u:p@h:3306/db",
	}
	for in, want := range tests {
		if got := pgxURL(in); got != want {
			t.Errorf("pgxURL(%q) = %q, want %q", in, got, want)
		}
	}
}
