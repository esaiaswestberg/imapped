package config

import "time"

// Config is the complete runtime configuration. Every field carries both a
// `toml` and an `env` tag: the TOML file and the environment are two views onto
// the same struct, so a knob can never exist in one and not the other.
type Config struct {
	AppEnv              string `toml:"app_env" env:"APP_ENV"`
	AppBaseURL          string `toml:"app_base_url" env:"APP_BASE_URL"`
	LogLevel            string `toml:"log_level" env:"LOG_LEVEL"`
	LogFormat           string `toml:"log_format" env:"LOG_FORMAT"`
	EncryptionMasterKey string `toml:"encryption_master_key" env:"ENCRYPTION_MASTER_KEY"`

	IMAP      IMAP      `toml:"imap"`
	HTTP      HTTP      `toml:"http"`
	DB        DB        `toml:"db"`
	Storage   Storage   `toml:"storage"`
	Upstream  Upstream  `toml:"upstream"`
	Sync      Sync      `toml:"sync"`
	Web       Web       `toml:"web"`
	Search    Search    `toml:"search"`
	Limits    Limits    `toml:"limits"`
	Bootstrap Bootstrap `toml:"bootstrap"`
}

// IMAP configures the downstream listeners that mail clients connect to.
type IMAP struct {
	PlaintextBind  string   `toml:"plaintext_bind" env:"IMAP_PLAINTEXT_BIND"`
	TLSBind        string   `toml:"tls_bind" env:"IMAP_TLS_BIND"`
	TLSCertPath    string   `toml:"tls_cert_path" env:"IMAP_TLS_CERT_PATH"`
	TLSKeyPath     string   `toml:"tls_key_path" env:"IMAP_TLS_KEY_PATH"`
	IdleTimeout    Duration `toml:"idle_timeout" env:"IMAP_IDLE_TIMEOUT"`
	CommandTimeout Duration `toml:"command_timeout" env:"IMAP_COMMAND_TIMEOUT"`
	// MaxLiteralSize caps an inbound APPEND literal from a mail client.
	MaxLiteralSize ByteSize `toml:"max_literal_size" env:"IMAP_MAX_LITERAL_SIZE"`
}

// HTTP configures the web UI listener and the optional separate metrics listener.
type HTTP struct {
	Bind        string   `toml:"bind" env:"HTTP_BIND"`
	MetricsBind string   `toml:"metrics_bind" env:"HTTP_METRICS_BIND"`
	ReadTimeout Duration `toml:"read_timeout" env:"HTTP_READ_TIMEOUT"`
	// WriteTimeout must stay generous (or zero) because SSE responses are long-lived.
	WriteTimeout    Duration `toml:"write_timeout" env:"HTTP_WRITE_TIMEOUT"`
	ShutdownTimeout Duration `toml:"shutdown_timeout" env:"HTTP_SHUTDOWN_TIMEOUT"`
}

// DB configures the Postgres connection pool.
type DB struct {
	URL              string   `toml:"url" env:"DATABASE_URL"`
	MaxConns         int32    `toml:"max_conns" env:"DB_MAX_CONNS"`
	MinConns         int32    `toml:"min_conns" env:"DB_MIN_CONNS"`
	ConnMaxLifetime  Duration `toml:"conn_max_lifetime" env:"DB_CONN_MAX_LIFETIME"`
	ConnectTimeout   Duration `toml:"connect_timeout" env:"DB_CONNECT_TIMEOUT"`
	StatementTimeout Duration `toml:"statement_timeout" env:"DB_STATEMENT_TIMEOUT"`
	// AutoMigrate applies pending migrations on startup.
	AutoMigrate bool `toml:"auto_migrate" env:"DB_AUTO_MIGRATE"`
}

// Storage configures the blob store holding RFC822 bodies and MIME parts.
// If S3Bucket and S3Endpoint are both set the S3 backend is used; otherwise the
// filesystem backend at Path is used.
type Storage struct {
	Path             string `toml:"path" env:"OBJECT_STORE_PATH"`
	S3Endpoint       string `toml:"s3_endpoint" env:"S3_ENDPOINT"`
	S3Bucket         string `toml:"s3_bucket" env:"S3_BUCKET"`
	S3AccessKeyID    string `toml:"s3_access_key_id" env:"S3_ACCESS_KEY_ID"`
	S3SecretKey      string `toml:"s3_secret_access_key" env:"S3_SECRET_ACCESS_KEY"`
	S3Region         string `toml:"s3_region" env:"S3_REGION"`
	S3ForcePathStyle bool   `toml:"s3_force_path_style" env:"S3_FORCE_PATH_STYLE"`
}

// Upstream configures how we talk to the remote IMAP server we are mirroring.
// Every timeout here exists because the Rust implementation had none and a
// silently-dropped TCP connection hung the sync indefinitely.
type Upstream struct {
	DialTimeout         Duration `toml:"dial_timeout" env:"UPSTREAM_DIAL_TIMEOUT"`
	TLSHandshakeTimeout Duration `toml:"tls_handshake_timeout" env:"UPSTREAM_TLS_HANDSHAKE_TIMEOUT"`
	GreetingTimeout     Duration `toml:"greeting_timeout" env:"UPSTREAM_GREETING_TIMEOUT"`
	// IOIdleTimeout is an inactivity deadline reset on every read/write, so a
	// legitimately slow large body is fine but a silent connection dies fast.
	IOIdleTimeout        Duration `toml:"io_idle_timeout" env:"UPSTREAM_IO_IDLE_TIMEOUT"`
	CommandTimeout       Duration `toml:"command_timeout" env:"UPSTREAM_COMMAND_TIMEOUT"`
	FetchMetadataTimeout Duration `toml:"fetch_metadata_timeout" env:"UPSTREAM_FETCH_METADATA_TIMEOUT"`
	FetchBodyTimeout     Duration `toml:"fetch_body_timeout" env:"UPSTREAM_FETCH_BODY_TIMEOUT"`
	// TCPUserTimeout bounds how long the kernel retransmits unacknowledged data
	// before declaring the connection dead. Linux defaults to ~11 minutes.
	TCPUserTimeout Duration `toml:"tcp_user_timeout" env:"UPSTREAM_TCP_USER_TIMEOUT"`
	TCPKeepAlive   Duration `toml:"tcp_keepalive" env:"UPSTREAM_TCP_KEEPALIVE"`

	RetryMaxAttempts int      `toml:"retry_max_attempts" env:"UPSTREAM_RETRY_MAX_ATTEMPTS"`
	RetryBaseDelay   Duration `toml:"retry_base_delay" env:"UPSTREAM_RETRY_BASE_DELAY"`
	RetryMaxDelay    Duration `toml:"retry_max_delay" env:"UPSTREAM_RETRY_MAX_DELAY"`

	PreferQResync   bool `toml:"prefer_qresync" env:"UPSTREAM_PREFER_QRESYNC"`
	PreferCondStore bool `toml:"prefer_condstore" env:"UPSTREAM_PREFER_CONDSTORE"`
	EnableCompress  bool `toml:"enable_compress" env:"UPSTREAM_ENABLE_COMPRESS"`
	// InsecureSkipVerify disables upstream TLS certificate verification. Only for
	// self-signed test servers; Validate refuses it when app_env is production.
	InsecureSkipVerify bool `toml:"insecure_skip_verify" env:"UPSTREAM_INSECURE_SKIP_VERIFY"`
}

// Sync configures the mirroring engine.
type Sync struct {
	Enabled            bool     `toml:"enabled" env:"SYNC_ENABLED"`
	Interval           Duration `toml:"interval" env:"SYNC_INTERVAL"`
	AccountsConcurrent int      `toml:"accounts_concurrent" env:"SYNC_ACCOUNTS_CONCURRENT"`
	// ConnectionsPerAccount is the body-fetch worker pool size per account.
	ConnectionsPerAccount int `toml:"connections_per_account" env:"SYNC_CONNECTIONS_PER_ACCOUNT"`
	// MetadataBatchUIDs chunks the pass-1 UID range so a failure loses one chunk.
	MetadataBatchUIDs int      `toml:"metadata_batch_uids" env:"SYNC_METADATA_BATCH_UIDS"`
	BodyBatchBytes    ByteSize `toml:"body_batch_bytes" env:"SYNC_BODY_BATCH_BYTES"`
	BodyBatchMaxMsgs  int      `toml:"body_batch_max_msgs" env:"SYNC_BODY_BATCH_MAX_MSGS"`
	// BodyMaxInlineBytes: messages above this get a batch to themselves.
	BodyMaxInlineBytes ByteSize `toml:"body_max_inline_bytes" env:"SYNC_BODY_MAX_INLINE_BYTES"`
	BodyMaxAttempts    int      `toml:"body_max_attempts" env:"SYNC_BODY_MAX_ATTEMPTS"`
	// DeletionScanEvery bounds how often the expensive full-UID deletion scan runs
	// on servers without QRESYNC.
	DeletionScanEvery int      `toml:"deletion_scan_every" env:"SYNC_DELETION_SCAN_EVERY"`
	MaxRunDuration    Duration `toml:"max_run_duration" env:"SYNC_MAX_RUN_DURATION"`
	// ClaimReapAfter returns messages stuck in body_state='fetching' to 'pending'
	// after a hard crash.
	ClaimReapAfter    Duration `toml:"claim_reap_after" env:"SYNC_CLAIM_REAP_AFTER"`
	HeartbeatInterval Duration `toml:"heartbeat_interval" env:"SYNC_HEARTBEAT_INTERVAL"`
}

// Web configures the browser-facing UI.
type Web struct {
	Enabled    bool     `toml:"enabled" env:"WEB_ENABLED"`
	SessionTTL Duration `toml:"session_ttl" env:"WEB_SESSION_TTL"`
	// SecureCookies forces the Secure flag. Turn off only for plaintext localhost.
	SecureCookies bool `toml:"secure_cookies" env:"WEB_SECURE_COOKIES"`
	PProf         bool `toml:"pprof" env:"WEB_PPROF"`
}

// Search configures full-text search.
type Search struct {
	Backend  string `toml:"backend" env:"SEARCH_BACKEND"`
	Language string `toml:"language" env:"SEARCH_LANGUAGE"`
	// MaxIndexedBodyBytes truncates body text before to_tsvector, which errors
	// above roughly 1MB of lexemes.
	MaxIndexedBodyBytes ByteSize `toml:"max_indexed_body_bytes" env:"SEARCH_MAX_INDEXED_BODY_BYTES"`
}

// Limits configures per-message and per-account ceilings.
type Limits struct {
	MaxMessageSize         ByteSize `toml:"max_message_size" env:"MAX_MESSAGE_SIZE"`
	DefaultAccountQuota    ByteSize `toml:"default_account_quota" env:"DEFAULT_ACCOUNT_QUOTA"`
	LoginRateLimitFailures int      `toml:"login_rate_limit_failures" env:"LOGIN_RATE_LIMIT_FAILURES"`
	LoginRateLimitLockout  Duration `toml:"login_rate_limit_lockout" env:"LOGIN_RATE_LIMIT_LOCKOUT"`
}

// Bootstrap creates a first admin user on startup when no users exist, so a
// fresh deployment can reach the web UI without shell access.
type Bootstrap struct {
	Email    string `toml:"email" env:"BOOTSTRAP_EMAIL"`
	Password string `toml:"password" env:"BOOTSTRAP_PASSWORD"`
}

// Default returns the built-in configuration. This struct literal is the single
// source of truth for defaults; the TOML file and environment only overlay it.
func Default() Config {
	return Config{
		AppEnv:     "development",
		AppBaseURL: "http://localhost:8080",
		LogLevel:   "info",
		LogFormat:  "json",

		IMAP: IMAP{
			PlaintextBind: "0.0.0.0:1143",
			TLSBind:       "",
			TLSCertPath:   "",
			TLSKeyPath:    "",
			// 29 minutes: RFC 2177 requires re-issuing IDLE at least every 29.
			IdleTimeout:    Duration(29 * time.Minute),
			CommandTimeout: Duration(5 * time.Minute),
			MaxLiteralSize: ByteSize(50 << 20),
		},
		HTTP: HTTP{
			Bind:        "0.0.0.0:8080",
			MetricsBind: "",
			ReadTimeout: Duration(30 * time.Second),
			// Zero: SSE streams must not be cut off by a write deadline.
			WriteTimeout:    Duration(0),
			ShutdownTimeout: Duration(20 * time.Second),
		},
		DB: DB{
			MaxConns:         20,
			MinConns:         2,
			ConnMaxLifetime:  Duration(time.Hour),
			ConnectTimeout:   Duration(10 * time.Second),
			StatementTimeout: Duration(30 * time.Second),
			AutoMigrate:      true,
		},
		Storage: Storage{
			Path:             "./data/blob",
			S3Region:         "auto",
			S3ForcePathStyle: true,
		},
		Upstream: Upstream{
			DialTimeout:          Duration(10 * time.Second),
			TLSHandshakeTimeout:  Duration(10 * time.Second),
			GreetingTimeout:      Duration(15 * time.Second),
			IOIdleTimeout:        Duration(60 * time.Second),
			CommandTimeout:       Duration(30 * time.Second),
			FetchMetadataTimeout: Duration(180 * time.Second),
			FetchBodyTimeout:     Duration(60 * time.Second),
			TCPUserTimeout:       Duration(60 * time.Second),
			TCPKeepAlive:         Duration(30 * time.Second),

			RetryMaxAttempts: 6,
			RetryBaseDelay:   Duration(time.Second),
			RetryMaxDelay:    Duration(5 * time.Minute),

			PreferQResync:   true,
			PreferCondStore: true,
			EnableCompress:  true,
		},
		Sync: Sync{
			Enabled:               true,
			Interval:              Duration(15 * time.Minute),
			AccountsConcurrent:    4,
			ConnectionsPerAccount: 4,
			MetadataBatchUIDs:     20000,
			BodyBatchBytes:        ByteSize(4 << 20),
			BodyBatchMaxMsgs:      50,
			BodyMaxInlineBytes:    ByteSize(32 << 20),
			BodyMaxAttempts:       5,
			DeletionScanEvery:     10,
			MaxRunDuration:        Duration(2 * time.Hour),
			ClaimReapAfter:        Duration(15 * time.Minute),
			HeartbeatInterval:     Duration(15 * time.Second),
		},
		Web: Web{
			Enabled:       true,
			SessionTTL:    Duration(30 * 24 * time.Hour),
			SecureCookies: true,
			PProf:         false,
		},
		Search: Search{
			Backend:             "postgres",
			Language:            "english",
			MaxIndexedBodyBytes: ByteSize(900_000),
		},
		Limits: Limits{
			MaxMessageSize:         ByteSize(100 << 20),
			DefaultAccountQuota:    ByteSize(10 << 30),
			LoginRateLimitFailures: 5,
			LoginRateLimitLockout:  Duration(time.Minute),
		},
	}
}

// IsProduction reports whether this process should refuse insecure defaults.
func (c Config) IsProduction() bool { return c.AppEnv == "production" }

// UseS3 reports whether the S3 blob backend is configured. Both the bucket and
// endpoint must be present; a partial configuration is a Validate error rather
// than a silent fallback to local disk (which is how the Rust version could
// quietly write blobs to a container-local volume nobody backed up).
func (c Config) UseS3() bool {
	return c.Storage.S3Bucket != "" && c.Storage.S3Endpoint != ""
}
