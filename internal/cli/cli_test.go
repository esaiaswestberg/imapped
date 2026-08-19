package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// execute runs the command tree with args and captures combined output.
func execute(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := NewRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "imapped.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func TestVersionCommand(t *testing.T) {
	out, err := execute(t, "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.Contains(out, "imapped") {
		t.Errorf("version output = %q", out)
	}
}

func TestConfigCheckRejectsInvalidConfig(t *testing.T) {
	path := writeConfig(t, `
[sync]
connections_per_account = 99
`)
	_, err := execute(t, "config", "check", "--config", path)
	if err == nil {
		t.Fatal("expected config check to fail")
	}
	// Both problems should appear: the bad knob and the missing database URL.
	for _, want := range []string{"connections_per_account", "db.url"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got:\n%v", want, err)
		}
	}
}

func TestConfigCheckAcceptsValidConfig(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/imapped")

	out, err := execute(t, "config", "check", "--config", writeConfig(t, ""))
	if err != nil {
		t.Fatalf("config check: %v", err)
	}
	if !strings.Contains(out, "configuration is valid") {
		t.Errorf("output = %q", out)
	}
}

func TestConfigCheckReportsUnknownKeys(t *testing.T) {
	path := writeConfig(t, `
[sync]
conections_per_account = 4
`)
	_, err := execute(t, "config", "check", "--config", path)
	if err == nil || !strings.Contains(err.Error(), "conections_per_account") {
		t.Errorf("expected the typo to be reported, got: %v", err)
	}
}

func TestConfigShowRedactsSecrets(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://user:hunter2@localhost/imapped")

	out, err := execute(t, "config", "show", "--config", writeConfig(t, ""))
	if err != nil {
		t.Fatalf("config show: %v", err)
	}
	if strings.Contains(out, "hunter2") {
		t.Error("database password leaked into `config show` output")
	}
	if !strings.Contains(out, "env:DATABASE_URL") {
		t.Errorf("output should attribute db.url to its env var, got:\n%s", out)
	}
}

func TestConfigShowOnlyListsOverridesByDefault(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/imapped")
	path := writeConfig(t, "")

	out, err := execute(t, "config", "show", "--config", path)
	if err != nil {
		t.Fatalf("config show: %v", err)
	}
	// A default-valued knob should be hidden unless --all is passed.
	if strings.Contains(out, "sync.body_batch_max_msgs") {
		t.Error("defaults should be hidden without --all")
	}

	out, err = execute(t, "config", "show", "--all", "--config", path)
	if err != nil {
		t.Fatalf("config show --all: %v", err)
	}
	if !strings.Contains(out, "sync.body_batch_max_msgs") {
		t.Error("--all should include defaulted knobs")
	}
}
