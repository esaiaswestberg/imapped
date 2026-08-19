package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
)

// ExampleMasterKey is the placeholder shipped in example config. Production
// refuses to start with it, because an encryption key everyone can read from a
// public repository protects nothing.
const ExampleMasterKey = "change-me-32-bytes-minimum-secret"

// MinMasterKeyLen is the shortest accepted encryption master key.
const MinMasterKeyLen = 32

// Validate checks the whole configuration and returns every problem at once.
// Reporting one error per run turns configuring a fresh deployment into a
// guess-fix-restart loop, so the errors are joined instead.
func (c Config) Validate() error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	switch c.AppEnv {
	case "development", "staging", "production":
	default:
		add("app_env (APP_ENV) is %q: want development, staging or production", c.AppEnv)
	}

	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		add("log_level (LOG_LEVEL) is %q: want debug, info, warn or error", c.LogLevel)
	}
	switch strings.ToLower(c.LogFormat) {
	case "json", "text":
	default:
		add("log_format (LOG_FORMAT) is %q: want json or text", c.LogFormat)
	}

	if c.DB.URL == "" {
		problems = append(problems, errNoDatabase)
	}
	if c.DB.MaxConns < 1 {
		add("db.max_conns must be at least 1, got %d", c.DB.MaxConns)
	}
	if c.DB.MinConns < 0 || c.DB.MinConns > c.DB.MaxConns {
		add("db.min_conns must be between 0 and db.max_conns (%d), got %d", c.DB.MaxConns, c.DB.MinConns)
	}

	// The master key protects upstream IMAP credentials at rest. A short or
	// placeholder key is tolerated in development so `go test` and a local
	// `docker compose up` work without ceremony, but never in production.
	switch {
	case c.EncryptionMasterKey == "" && c.IsProduction():
		add("encryption_master_key (ENCRYPTION_MASTER_KEY) is required in production")
	case c.EncryptionMasterKey == "":
		// Development: a derived ephemeral key is used; warned about at startup.
	case len(c.EncryptionMasterKey) < MinMasterKeyLen:
		add("encryption_master_key must be at least %d bytes, got %d", MinMasterKeyLen, len(c.EncryptionMasterKey))
	case c.EncryptionMasterKey == ExampleMasterKey && c.IsProduction():
		add("encryption_master_key is still the example value; generate one with `openssl rand -hex 32`")
	}

	problems = append(problems, validateBind("imap.plaintext_bind", c.IMAP.PlaintextBind)...)
	problems = append(problems, validateBind("imap.tls_bind", c.IMAP.TLSBind)...)
	problems = append(problems, validateBind("http.bind", c.HTTP.Bind)...)
	problems = append(problems, validateBind("http.metrics_bind", c.HTTP.MetricsBind)...)

	if c.IMAP.PlaintextBind == "" && c.IMAP.TLSBind == "" && c.HTTP.Bind == "" {
		add("no listeners configured: set at least one of imap.plaintext_bind, imap.tls_bind or http.bind")
	}

	// A TLS bind with no key material would silently degrade to "port closed".
	if c.IMAP.TLSBind != "" {
		if c.IMAP.TLSCertPath == "" || c.IMAP.TLSKeyPath == "" {
			add("imap.tls_bind is set but imap.tls_cert_path and imap.tls_key_path are not both configured")
		} else {
			for label, path := range map[string]string{
				"imap.tls_cert_path": c.IMAP.TLSCertPath,
				"imap.tls_key_path":  c.IMAP.TLSKeyPath,
			} {
				if _, err := os.Stat(path); err != nil {
					add("%s: %v", label, err)
				}
			}
		}
	}

	// Storage: a half-configured S3 backend must not silently fall back to local
	// disk, which is how blobs end up on a container volume nobody backs up.
	s3Fields := map[string]string{
		"storage.s3_endpoint":          c.Storage.S3Endpoint,
		"storage.s3_bucket":            c.Storage.S3Bucket,
		"storage.s3_access_key_id":     c.Storage.S3AccessKeyID,
		"storage.s3_secret_access_key": c.Storage.S3SecretKey,
	}
	var s3Set, s3Missing []string
	for name, value := range s3Fields {
		if value == "" {
			s3Missing = append(s3Missing, name)
		} else {
			s3Set = append(s3Set, name)
		}
	}
	if len(s3Set) > 0 && len(s3Missing) > 0 {
		add("storage is partially configured for S3: set %s or clear %s to use local disk",
			strings.Join(sorted(s3Missing), ", "), strings.Join(sorted(s3Set), ", "))
	}
	if len(s3Set) == 0 && c.Storage.Path == "" {
		add("storage.path (OBJECT_STORE_PATH) is required when S3 is not configured")
	}

	if c.Upstream.InsecureSkipVerify && c.IsProduction() {
		add("upstream.insecure_skip_verify must not be enabled in production")
	}

	// Timeouts: a zero value here means "wait forever", which is the exact bug
	// this rewrite exists to eliminate. Reject it explicitly.
	for name, d := range map[string]Duration{
		"upstream.dial_timeout":           c.Upstream.DialTimeout,
		"upstream.tls_handshake_timeout":  c.Upstream.TLSHandshakeTimeout,
		"upstream.greeting_timeout":       c.Upstream.GreetingTimeout,
		"upstream.io_idle_timeout":        c.Upstream.IOIdleTimeout,
		"upstream.command_timeout":        c.Upstream.CommandTimeout,
		"upstream.fetch_metadata_timeout": c.Upstream.FetchMetadataTimeout,
		"upstream.fetch_body_timeout":     c.Upstream.FetchBodyTimeout,
		"sync.max_run_duration":           c.Sync.MaxRunDuration,
		"sync.claim_reap_after":           c.Sync.ClaimReapAfter,
		"sync.heartbeat_interval":         c.Sync.HeartbeatInterval,
		"db.connect_timeout":              c.DB.ConnectTimeout,
	} {
		if d <= 0 {
			add("%s must be greater than zero (a zero timeout means wait forever)", name)
		}
	}

	if c.Sync.AccountsConcurrent < 1 {
		add("sync.accounts_concurrent must be at least 1, got %d", c.Sync.AccountsConcurrent)
	}
	// Most IMAP servers cap concurrent connections per user well below this
	// (iCloud ~5, Dovecot defaults to 10); going higher gets us throttled or banned.
	if c.Sync.ConnectionsPerAccount < 1 || c.Sync.ConnectionsPerAccount > 16 {
		add("sync.connections_per_account must be between 1 and 16, got %d", c.Sync.ConnectionsPerAccount)
	}
	if c.Sync.MetadataBatchMessages < 1 {
		add("sync.metadata_batch_messages must be at least 1, got %d", c.Sync.MetadataBatchMessages)
	}
	if c.Sync.BodyBatchMaxMsgs < 1 {
		add("sync.body_batch_max_msgs must be at least 1, got %d", c.Sync.BodyBatchMaxMsgs)
	}
	if c.Sync.BodyBatchBytes < 1 {
		add("sync.body_batch_bytes must be greater than zero")
	}
	if c.Sync.BodyMaxAttempts < 1 {
		add("sync.body_max_attempts must be at least 1, got %d", c.Sync.BodyMaxAttempts)
	}
	if c.Sync.DeletionScanEvery < 1 {
		add("sync.deletion_scan_every must be at least 1, got %d", c.Sync.DeletionScanEvery)
	}
	if c.Sync.Enabled && c.Sync.Interval <= 0 {
		add("sync.interval must be greater than zero when sync is enabled")
	}
	// Heartbeats exist to detect a wedged run; one slower than the run limit
	// could never fire in time to matter.
	if c.Sync.HeartbeatInterval > 0 && c.Sync.MaxRunDuration > 0 &&
		c.Sync.HeartbeatInterval >= c.Sync.MaxRunDuration {
		add("sync.heartbeat_interval (%s) must be shorter than sync.max_run_duration (%s)",
			c.Sync.HeartbeatInterval, c.Sync.MaxRunDuration)
	}

	if c.Upstream.RetryMaxAttempts < 1 {
		add("upstream.retry_max_attempts must be at least 1, got %d", c.Upstream.RetryMaxAttempts)
	}
	if c.Upstream.RetryBaseDelay > c.Upstream.RetryMaxDelay {
		add("upstream.retry_base_delay (%s) must not exceed upstream.retry_max_delay (%s)",
			c.Upstream.RetryBaseDelay, c.Upstream.RetryMaxDelay)
	}

	if c.Limits.MaxMessageSize < 1 {
		add("limits.max_message_size must be greater than zero")
	}
	// A message larger than the batch ceiling would never be fetched at all.
	if c.Sync.BodyMaxInlineBytes > c.Limits.MaxMessageSize {
		add("sync.body_max_inline_bytes (%s) must not exceed limits.max_message_size (%s)",
			c.Sync.BodyMaxInlineBytes, c.Limits.MaxMessageSize)
	}

	switch c.Search.Backend {
	case "postgres":
	default:
		add("search.backend is %q: only \"postgres\" is implemented", c.Search.Backend)
	}
	if c.Search.MaxIndexedBodyBytes < 1 {
		add("search.max_indexed_body_bytes must be greater than zero")
	}

	if c.Web.Enabled && c.HTTP.Bind == "" {
		add("web.enabled is true but http.bind is empty")
	}
	if c.Web.SessionTTL <= 0 {
		add("web.session_ttl must be greater than zero")
	}
	if c.Bootstrap.Email != "" && c.Bootstrap.Password == "" {
		add("bootstrap.email is set but bootstrap.password is empty")
	}
	if c.Bootstrap.Password != "" && c.Bootstrap.Email == "" {
		add("bootstrap.password is set but bootstrap.email is empty")
	}

	if len(problems) == 0 {
		return nil
	}
	return &ValidationError{Problems: problems}
}

// ValidationError collects every configuration problem found in one pass.
type ValidationError struct{ Problems []error }

func (e *ValidationError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "invalid configuration (%d %s):",
		len(e.Problems), plural(len(e.Problems), "problem", "problems"))
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p.Error())
	}
	return b.String()
}

func (e *ValidationError) Unwrap() []error { return e.Problems }

func validateBind(name, value string) []error {
	if value == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return []error{fmt.Errorf("%s is %q: want host:port, e.g. 0.0.0.0:1143", name, value)}
	}
	if port == "" {
		return []error{fmt.Errorf("%s is %q: port is missing", name, value)}
	}
	if host != "" && net.ParseIP(host) == nil {
		// A hostname is legal for a bind address, so only reject obvious garbage.
		if strings.ContainsAny(host, " \t/") {
			return []error{fmt.Errorf("%s has an invalid host %q", name, host)}
		}
	}
	return nil
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Fingerprint renders a short, stable digest of a secret so operators can tell
// two deployments apart without the value ever appearing in logs or the UI.
func Fingerprint(secret string) string {
	if secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(secret))
	return "sha256:" + hex.EncodeToString(sum[:4])
}

// IsMissingDatabase reports whether err is the "no database configured" problem.
func IsMissingDatabase(err error) bool { return errors.Is(err, errNoDatabase) }
