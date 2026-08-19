package config_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/esaiaswestberg/imapped/internal/config"
)

// repoFile resolves a path relative to the repository root, independent of the
// working directory the test happens to run from.
func repoFile(t *testing.T, name string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine test file location")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", name)
}

// The shipped example is the first thing a new operator copies. If it drifts
// out of step with the config struct — a renamed key, a removed section — the
// loader's own strictness turns it into a startup failure, so it has to be
// checked in CI rather than by hand.
func TestExampleConfigIsValid(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/imapped")

	res, err := config.Load(repoFile(t, "imapped.example.toml"))
	if err != nil {
		t.Fatalf("example config failed to load: %v", err)
	}
	if !res.FileLoaded {
		t.Fatal("example config was not loaded")
	}
	if err := res.Config.Validate(); err != nil {
		t.Fatalf("example config failed validation:\n%v", err)
	}
}

// Every value in the example should match the built-in default, so that reading
// the file tells you what the software actually does when left alone.
func TestExampleConfigMatchesDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/imapped")

	res, err := config.Load(repoFile(t, "imapped.example.toml"))
	if err != nil {
		t.Fatalf("loading example config: %v", err)
	}

	got := res.Config
	want := config.Default()
	// The database URL has no default and is supplied above.
	got.DB.URL, want.DB.URL = "", ""

	if got != want {
		t.Errorf("example config diverges from built-in defaults.\n"+
			"Either update imapped.example.toml or change Default().\n got: %+v\nwant: %+v", got, want)
	}
}

// A documented setting that no longer exists is worse than an undocumented one:
// the loader rejects unknown keys, so a stale example bricks startup.
func TestExampleConfigDocumentsEveryEnvVar(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/imapped")

	res, err := config.Load(repoFile(t, "imapped.example.toml"))
	if err != nil {
		t.Fatalf("loading example config: %v", err)
	}

	data, err := readFile(repoFile(t, "imapped.example.toml"))
	if err != nil {
		t.Fatalf("reading example config: %v", err)
	}

	var missing []string
	for _, field := range res.Fields {
		if field.EnvVar == "" {
			continue
		}
		// The file nests keys under section headers, so look for the leaf key
		// (possibly commented out, as secrets are) or the env var name.
		leaf := field.TOMLPath
		if idx := strings.LastIndex(leaf, "."); idx >= 0 {
			leaf = leaf[idx+1:]
		}
		if !strings.Contains(data, leaf) && !strings.Contains(data, field.EnvVar) {
			missing = append(missing, field.TOMLPath+" ("+field.EnvVar+")")
		}
	}
	if len(missing) > 0 {
		t.Errorf("these settings are undocumented in imapped.example.toml:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

func readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	return string(data), err
}
