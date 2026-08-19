package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Account is a mail account being mirrored.
type Account struct {
	ID           int64
	UserID       int64
	DisplayName  string
	EmailAddress string

	UpstreamHost       string
	UpstreamPort       int
	UpstreamTLSMode    string
	UpstreamAuthMethod string

	EncryptedUsername []byte
	EncryptedSecret   []byte

	Capabilities []string
	SyncPaused   bool
	AuthState    string

	LastSyncAt    *time.Time
	LastSyncError *string
	DisabledAt    *time.Time
}

// Active reports whether the account should be synced.
func (a Account) Active() bool {
	return a.DisabledAt == nil && !a.SyncPaused && a.AuthState != "auth_failed"
}

const accountColumns = `
	id, user_id, display_name, email_address,
	upstream_host, upstream_port, upstream_tls_mode, upstream_auth_method,
	encrypted_username, encrypted_secret,
	upstream_capabilities, sync_paused, auth_state,
	last_sync_at, last_sync_error, disabled_at`

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var a Account
	var capsJSON []byte
	err := row.Scan(
		&a.ID, &a.UserID, &a.DisplayName, &a.EmailAddress,
		&a.UpstreamHost, &a.UpstreamPort, &a.UpstreamTLSMode, &a.UpstreamAuthMethod,
		&a.EncryptedUsername, &a.EncryptedSecret,
		&capsJSON, &a.SyncPaused, &a.AuthState,
		&a.LastSyncAt, &a.LastSyncError, &a.DisabledAt,
	)
	if err != nil {
		return Account{}, err
	}
	if len(capsJSON) > 0 {
		_ = json.Unmarshal(capsJSON, &a.Capabilities)
	}
	return a, nil
}

// GetAccount returns one account by id.
func (s *Store) GetAccount(ctx context.Context, id int64) (Account, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+accountColumns+` FROM mail_accounts WHERE id = $1`, id)
	account, err := scanAccount(row)
	if err != nil {
		return Account{}, fmt.Errorf("loading account %d: %w", id, notFound(err))
	}
	return account, nil
}

// ListSyncableAccounts returns accounts eligible for a background sync.
//
// Accounts in the auth_failed state are excluded: their credentials are known
// to be wrong, and repeatedly retrying them risks the provider locking the
// address. They return to the rotation when the credentials are edited.
func (s *Store) ListSyncableAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+accountColumns+` FROM mail_accounts
		 WHERE disabled_at IS NULL AND sync_paused = FALSE AND auth_state <> 'auth_failed'
		 ORDER BY COALESCE(last_sync_at, 'epoch'::timestamptz) ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("listing syncable accounts: %w", err)
	}
	defer rows.Close()

	var accounts []Account
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning account: %w", err)
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

// SetAccountCapabilities caches what the server advertised.
func (s *Store) SetAccountCapabilities(ctx context.Context, accountID int64, caps []string) error {
	payload, err := json.Marshal(caps)
	if err != nil {
		return fmt.Errorf("encoding capabilities: %w", err)
	}
	_, err = s.pool.Exec(ctx,
		`UPDATE mail_accounts SET upstream_capabilities = $2, updated_at = now() WHERE id = $1`,
		accountID, payload)
	return err
}

// SetAccountSyncResult records the outcome of a sync attempt.
//
// An authentication failure also flips auth_state, which removes the account
// from the sync rotation until someone corrects the credentials.
func (s *Store) SetAccountSyncResult(ctx context.Context, accountID int64, syncErr error, authFailed bool) error {
	var message *string
	if syncErr != nil {
		text := syncErr.Error()
		message = &text
	}
	authState := "ok"
	if authFailed {
		authState = "auth_failed"
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE mail_accounts
		 SET last_sync_at = now(), last_sync_error = $2, auth_state = $3, updated_at = now()
		 WHERE id = $1`, accountID, message, authState)
	return err
}

// SetAccountPaused pauses or resumes background syncing.
func (s *Store) SetAccountPaused(ctx context.Context, accountID int64, paused bool) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE mail_accounts SET sync_paused = $2, updated_at = now() WHERE id = $1`,
		accountID, paused)
	return err
}
