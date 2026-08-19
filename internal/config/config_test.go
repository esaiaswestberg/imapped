package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig writes a TOML file into a temp dir and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "imapped.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func TestDefaultConfigIsValid(t *testing.T) {
	cfg := Default()
	// A database URL is the one thing with no sensible default.
	cfg.DB.URL = "postgres://localhost/imapped"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config should validate, got:\n%v", err)
	}
}

func TestDefaultsApplyWithNoFileOrEnv(t *testing.T) {
	res, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := res.Config.Sync.ConnectionsPerAccount, 4; got != want {
		t.Errorf("ConnectionsPerAccount = %d, want %d", got, want)
	}
	if got, want := res.Config.Upstream.IOIdleTimeout.Std(), 60*time.Second; got != want {
		t.Errorf("IOIdleTimeout = %s, want %s", got, want)
	}
}

func TestPrecedenceEnvOverFileOverDefault(t *testing.T) {
	path := writeConfig(t, `
log_level = "warn"

[sync]
accounts_concurrent = 7
connections_per_account = 6
`)
	t.Setenv("SYNC_CONNECTIONS_PER_ACCOUNT", "2")

	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// default → file → env, one of each.
	if got, want := res.Config.Sync.BodyBatchMaxMsgs, 50; got != want {
		t.Errorf("default lost: BodyBatchMaxMsgs = %d, want %d", got, want)
	}
	if got, want := res.Config.Sync.AccountsConcurrent, 7; got != want {
		t.Errorf("file value lost: AccountsConcurrent = %d, want %d", got, want)
	}
	if got, want := res.Config.Sync.ConnectionsPerAccount, 2; got != want {
		t.Errorf("env did not override file: ConnectionsPerAccount = %d, want %d", got, want)
	}
	if got, want := res.Config.LogLevel, "warn"; got != want {
		t.Errorf("LogLevel = %q, want %q", got, want)
	}
}

func TestProvenanceReportsWhereValuesCameFrom(t *testing.T) {
	path := writeConfig(t, `
[sync]
accounts_concurrent = 7
connections_per_account = 6
`)
	t.Setenv("SYNC_CONNECTIONS_PER_ACCOUNT", "2")

	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	byPath := make(map[string]Field, len(res.Fields))
	for _, f := range res.Fields {
		byPath[f.TOMLPath] = f
	}

	for _, tc := range []struct {
		path string
		want Source
	}{
		{"sync.body_batch_max_msgs", SourceDefault},
		{"sync.accounts_concurrent", SourceFile},
		{"sync.connections_per_account", SourceEnv},
	} {
		f, ok := byPath[tc.path]
		if !ok {
			t.Errorf("no provenance recorded for %s", tc.path)
			continue
		}
		if f.Source != tc.want {
			t.Errorf("%s came from %q, want %q", tc.path, f.Source, tc.want)
		}
	}

	if got := byPath["sync.connections_per_account"].EnvVar; got != "SYNC_CONNECTIONS_PER_ACCOUNT" {
		t.Errorf("EnvVar = %q, want SYNC_CONNECTIONS_PER_ACCOUNT", got)
	}
}

// A silently-ignored typo is the failure mode this check exists to prevent.
func TestUnknownFileKeyIsAnError(t *testing.T) {
	path := writeConfig(t, `
[sync]
connections_per_acount = 6
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for an unknown key, got nil")
	}
	if !strings.Contains(err.Error(), "connections_per_acount") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

func TestMissingExplicitFileIsAnError(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.toml")); err == nil {
		t.Fatal("expected an error for a missing explicit config path, got nil")
	}
}

func TestSecretsAreRedactedInProvenance(t *testing.T) {
	t.Setenv("ENCRYPTION_MASTER_KEY", "super-secret-master-key-of-at-least-32-bytes")

	res, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, f := range res.Fields {
		if f.TOMLPath != "encryption_master_key" {
			continue
		}
		if !f.Secret {
			t.Error("encryption_master_key should be marked secret")
		}
		if strings.Contains(f.Value, "super-secret") {
			t.Errorf("secret leaked into provenance: %q", f.Value)
		}
		if !strings.HasPrefix(f.Value, "sha256:") {
			t.Errorf("secret should render as a fingerprint, got %q", f.Value)
		}
		return
	}
	t.Fatal("encryption_master_key missing from provenance")
}

// Validate must surface every problem in one pass, not just the first.
func TestValidateReportsAllProblemsAtOnce(t *testing.T) {
	cfg := Default()
	cfg.DB.URL = ""
	cfg.LogLevel = "chatty"
	cfg.Sync.ConnectionsPerAccount = 99
	cfg.Search.Backend = "elasticsearch"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation to fail")
	}
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if len(verr.Problems) < 4 {
		t.Errorf("expected at least 4 problems, got %d:\n%v", len(verr.Problems), err)
	}
	for _, want := range []string{"db.url", "log_level", "connections_per_account", "search.backend"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got:\n%v", want, err)
		}
	}
	if !IsMissingDatabase(err) {
		t.Error("IsMissingDatabase should recognise the missing db.url problem")
	}
}

// Zero timeouts mean "wait forever" — the exact bug this rewrite exists to fix.
func TestValidateRejectsZeroTimeouts(t *testing.T) {
	cfg := Default()
	cfg.DB.URL = "postgres://localhost/imapped"
	cfg.Upstream.IOIdleTimeout = 0

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected a zero io_idle_timeout to be rejected")
	}
	if !strings.Contains(err.Error(), "upstream.io_idle_timeout") {
		t.Errorf("error should name the zero timeout, got:\n%v", err)
	}
}

func TestValidateProductionHardening(t *testing.T) {
	base := func() Config {
		cfg := Default()
		cfg.AppEnv = "production"
		cfg.DB.URL = "postgres://localhost/imapped"
		cfg.EncryptionMasterKey = strings.Repeat("k", 40)
		return cfg
	}

	t.Run("rejects example master key", func(t *testing.T) {
		cfg := base()
		cfg.EncryptionMasterKey = ExampleMasterKey
		if err := cfg.Validate(); err == nil ||
			!strings.Contains(err.Error(), "example value") {
			t.Errorf("expected the example key to be rejected, got: %v", err)
		}
	})

	t.Run("rejects insecure upstream TLS", func(t *testing.T) {
		cfg := base()
		cfg.Upstream.InsecureSkipVerify = true
		if err := cfg.Validate(); err == nil ||
			!strings.Contains(err.Error(), "insecure_skip_verify") {
			t.Errorf("expected insecure_skip_verify to be rejected, got: %v", err)
		}
	})

	t.Run("allows a well-formed production config", func(t *testing.T) {
		if err := base().Validate(); err != nil {
			t.Errorf("expected a valid production config, got:\n%v", err)
		}
	})
}

// A half-configured S3 backend must not silently write blobs to local disk.
func TestValidateRejectsPartialS3Config(t *testing.T) {
	cfg := Default()
	cfg.DB.URL = "postgres://localhost/imapped"
	cfg.Storage.S3Bucket = "mail"
	cfg.Storage.S3Endpoint = "https://s3.example.com"
	// access key and secret deliberately absent

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected partial S3 configuration to be rejected")
	}
	if !strings.Contains(err.Error(), "partially configured for S3") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateTLSBindRequiresKeyMaterial(t *testing.T) {
	cfg := Default()
	cfg.DB.URL = "postgres://localhost/imapped"
	cfg.IMAP.TLSBind = "0.0.0.0:1993"

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "tls_cert_path") {
		t.Errorf("expected a TLS bind without cert paths to be rejected, got: %v", err)
	}
}

func TestDurationParsing(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "30s", want: 30 * time.Second},
		{in: "2h", want: 2 * time.Hour},
		{in: "1h30m", want: 90 * time.Minute},
		{in: "", wantErr: true},
		{in: "soon", wantErr: true},
		{in: "30", wantErr: true}, // unitless is ambiguous; reject it
	} {
		var d Duration
		err := d.UnmarshalText([]byte(tc.in))
		if tc.wantErr {
			if err == nil {
				t.Errorf("Duration(%q): expected an error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("Duration(%q): %v", tc.in, err)
			continue
		}
		if d.Std() != tc.want {
			t.Errorf("Duration(%q) = %s, want %s", tc.in, d, tc.want)
		}
	}
}

func TestByteSizeParsing(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "1024", want: 1024},
		{in: "4MiB", want: 4 << 20},
		{in: "4mib", want: 4 << 20},
		{in: "32MiB", want: 32 << 20},
		{in: "10GiB", want: 10 << 30},
		{in: "1MB", want: 1_000_000},
		{in: "512KiB", want: 512 << 10},
		{in: "900000", want: 900_000},
		{in: "", wantErr: true},
		{in: "big", wantErr: true},
		{in: "-1", wantErr: true},
	} {
		var b ByteSize
		err := b.UnmarshalText([]byte(tc.in))
		if tc.wantErr {
			if err == nil {
				t.Errorf("ByteSize(%q): expected an error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ByteSize(%q): %v", tc.in, err)
			continue
		}
		if b.Int64() != tc.want {
			t.Errorf("ByteSize(%q) = %d, want %d", tc.in, b.Int64(), tc.want)
		}
	}
}

func TestByteSizeRoundTrip(t *testing.T) {
	for _, in := range []string{"4MiB", "32MiB", "10GiB", "512KiB"} {
		var b ByteSize
		if err := b.UnmarshalText([]byte(in)); err != nil {
			t.Fatalf("ByteSize(%q): %v", in, err)
		}
		if got := b.String(); got != in {
			t.Errorf("round trip of %q produced %q", in, got)
		}
	}
}

// Sizes and durations must survive both the TOML and the env path identically.
func TestScalarsParseFromFileAndEnv(t *testing.T) {
	path := writeConfig(t, `
[sync]
body_batch_bytes = "8MiB"

[upstream]
io_idle_timeout = "45s"
`)
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := res.Config.Sync.BodyBatchBytes.Int64(), int64(8<<20); got != want {
		t.Errorf("file ByteSize = %d, want %d", got, want)
	}
	if got, want := res.Config.Upstream.IOIdleTimeout.Std(), 45*time.Second; got != want {
		t.Errorf("file Duration = %s, want %s", got, want)
	}

	t.Setenv("SYNC_BODY_BATCH_BYTES", "16MiB")
	t.Setenv("UPSTREAM_IO_IDLE_TIMEOUT", "90s")
	res, err = Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := res.Config.Sync.BodyBatchBytes.Int64(), int64(16<<20); got != want {
		t.Errorf("env ByteSize = %d, want %d", got, want)
	}
	if got, want := res.Config.Upstream.IOIdleTimeout.Std(), 90*time.Second; got != want {
		t.Errorf("env Duration = %s, want %s", got, want)
	}
}

// The R2_* names are kept as aliases so existing deployments keep working.
func TestLegacyR2EnvAliases(t *testing.T) {
	t.Setenv("R2_BUCKET", "legacy-bucket")
	t.Setenv("R2_ENDPOINT", "https://r2.example.com")

	res, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := res.Config.Storage.S3Bucket; got != "legacy-bucket" {
		t.Errorf("R2_BUCKET alias not honoured: got %q", got)
	}
	if !res.Config.UseS3() {
		t.Error("UseS3 should be true when bucket and endpoint are set")
	}
}

func TestEveryFieldHasProvenance(t *testing.T) {
	res, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Guards against a new config section being added without a toml tag, which
	// would make it invisible to both the file loader and the settings page.
	if len(res.Fields) < 50 {
		t.Errorf("only %d fields have provenance; a section is probably untagged", len(res.Fields))
	}
	seen := make(map[string]bool)
	for _, f := range res.Fields {
		if seen[f.TOMLPath] {
			t.Errorf("duplicate provenance path %q", f.TOMLPath)
		}
		seen[f.TOMLPath] = true
		if f.TOMLPath == "" {
			t.Error("field with an empty TOML path")
		}
	}
	for _, want := range []string{
		"log_level", "db.url", "sync.connections_per_account",
		"upstream.io_idle_timeout", "web.session_ttl", "search.backend",
	} {
		if !seen[want] {
			t.Errorf("expected provenance for %q", want)
		}
	}
}
