package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Duration wraps time.Duration with text (un)marshalling so the same field can
// be populated from a TOML string ("30s") and from an environment variable.
// Both BurntSushi/toml and caarlos0/env dispatch through encoding.TextUnmarshaler.
type Duration time.Duration

func (d Duration) String() string { return time.Duration(d).String() }

// Std returns the wrapped standard-library duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", text, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// byteSizeUnits is ordered longest-suffix-first so that "MiB" is matched before
// "B" and "MB" before "M".
var byteSizeUnits = []struct {
	suffix string
	scale  int64
}{
	{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30}, {"TIB", 1 << 40},
	{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000}, {"TB", 1000 * 1000 * 1000 * 1000},
	{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40},
	{"B", 1},
}

// ByteSize is a byte quantity that accepts either a bare integer ("1024") or a
// human-readable suffix ("4MiB", "10GB"). IEC suffixes are powers of 1024, SI
// suffixes are powers of 1000.
type ByteSize int64

func (b ByteSize) Int64() int64 { return int64(b) }

func (b ByteSize) String() string {
	switch {
	case b >= 1<<30 && b%(1<<30) == 0:
		return strconv.FormatInt(int64(b)/(1<<30), 10) + "GiB"
	case b >= 1<<20 && b%(1<<20) == 0:
		return strconv.FormatInt(int64(b)/(1<<20), 10) + "MiB"
	case b >= 1<<10 && b%(1<<10) == 0:
		return strconv.FormatInt(int64(b)/(1<<10), 10) + "KiB"
	default:
		return strconv.FormatInt(int64(b), 10)
	}
}

func (b *ByteSize) UnmarshalText(text []byte) error {
	raw := strings.TrimSpace(string(text))
	if raw == "" {
		return fmt.Errorf("invalid byte size: empty")
	}
	upper := strings.ToUpper(raw)
	for _, unit := range byteSizeUnits {
		if !strings.HasSuffix(upper, unit.suffix) {
			continue
		}
		numPart := strings.TrimSpace(upper[:len(upper)-len(unit.suffix)])
		if numPart == "" {
			continue // bare "B"/"M" with no number is not a size
		}
		value, err := strconv.ParseFloat(numPart, 64)
		if err != nil {
			return fmt.Errorf("invalid byte size %q: %w", raw, err)
		}
		if value < 0 {
			return fmt.Errorf("invalid byte size %q: must not be negative", raw)
		}
		*b = ByteSize(int64(value * float64(unit.scale)))
		return nil
	}
	value, err := strconv.ParseInt(upper, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid byte size %q: want an integer or a suffixed size like 4MiB", raw)
	}
	if value < 0 {
		return fmt.Errorf("invalid byte size %q: must not be negative", raw)
	}
	*b = ByteSize(value)
	return nil
}

func (b ByteSize) MarshalText() ([]byte, error) { return []byte(b.String()), nil }
