// Package flagx provides startup-validated environment flags for migration
// modes (RFC-0021): enum flags whose value set is fixed at compile time and a
// bounded percent flag for shadow sampling.
//
// Validation is fail-fast by design: services call the Must* variants during
// startup so an invalid value stops the process before it serves traffic,
// instead of silently running in an unintended mode. The returned value is
// safe to use directly as a metric label — it is bounded by construction
// (one of the allowed values, or 0..100 for percents).
//
// Process rule (not enforced in code): every migration flag has an owner and
// a removal issue — flags do not outlive their migration.
package flagx

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

// Enum reads the environment variable name and validates it against allowed.
// An unset or empty variable yields def (which must itself be allowed). The
// active value is logged so operators can confirm the running mode from
// startup logs. An invalid value returns an error — callers must treat it as
// fatal at startup.
func Enum(name, def string, allowed ...string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		value = def
	}
	for _, a := range allowed {
		if value == a {
			log.Printf("flagx: %s=%s", name, value)
			return value, nil
		}
	}
	return "", fmt.Errorf("flagx: %s=%q invalid (allowed: %s)",
		name, value, strings.Join(allowed, "|"))
}

// MustEnum is Enum but exits the process on an invalid value. Use during
// service startup, before serving traffic.
func MustEnum(name, def string, allowed ...string) string {
	value, err := Enum(name, def, allowed...)
	if err != nil {
		log.Fatal(err)
	}
	return value
}

// Percent reads the environment variable name as an integer percentage in
// [0, 100]. An unset or empty variable yields def. Out-of-range or
// non-numeric values return an error — callers must treat it as fatal at
// startup.
func Percent(name string, def int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		if def < 0 || def > 100 {
			return 0, fmt.Errorf("flagx: %s default %d out of range 0..100", name, def)
		}
		log.Printf("flagx: %s=%d", name, def)
		return def, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 || value > 100 {
		return 0, fmt.Errorf("flagx: %s=%q invalid (integer 0..100)", name, raw)
	}
	log.Printf("flagx: %s=%d", name, value)
	return value, nil
}

// MustPercent is Percent but exits the process on an invalid value.
func MustPercent(name string, def int) int {
	value, err := Percent(name, def)
	if err != nil {
		log.Fatal(err)
	}
	return value
}
