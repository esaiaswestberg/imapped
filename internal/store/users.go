package store

import (
	"context"
	"fmt"
	"time"
)

// User is a person who can sign in.
type User struct {
	ID           int64
	Email        string
	PasswordHash string
	IsAdmin      bool
	DisabledAt   *time.Time
	CreatedAt    time.Time
}

// Active reports whether the user may sign in.
func (u User) Active() bool { return u.DisabledAt == nil }

// CreateUser adds a user.
func (s *Store) CreateUser(ctx context.Context, email, passwordHash string, isAdmin bool) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, is_admin) VALUES ($1, $2, $3)
		 RETURNING id, email, password_hash, is_admin, disabled_at, created_at`,
		email, passwordHash, isAdmin).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.IsAdmin, &u.DisabledAt, &u.CreatedAt)
	if err != nil {
		return User{}, fmt.Errorf("creating user %s: %w", email, err)
	}
	return u, nil
}

// UserByEmail looks a user up for sign-in. Matching is case-insensitive,
// because addresses are in practice.
func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, is_admin, disabled_at, created_at
		 FROM users WHERE lower(email) = lower($1)`, email).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.IsAdmin, &u.DisabledAt, &u.CreatedAt)
	if err != nil {
		return User{}, notFound(err)
	}
	return u, nil
}

// UserByID looks a user up by id.
func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, is_admin, disabled_at, created_at
		 FROM users WHERE id = $1`, id).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.IsAdmin, &u.DisabledAt, &u.CreatedAt)
	if err != nil {
		return User{}, notFound(err)
	}
	return u, nil
}

// CountUsers reports how many users exist, for the bootstrap check.
func (s *Store) CountUsers(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

// SetUserPassword replaces a user's password hash.
func (s *Store) SetUserPassword(ctx context.Context, userID int64, hash string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1`, userID, hash)
	return err
}

// CreateSession records a signed-in session.
//
// Only a hash of the token is stored, so a database leak does not hand over
// live sessions the way storing the token itself would.
func (s *Store) CreateSession(ctx context.Context, userID int64, tokenHash []byte, kind string,
	userAgent, remoteAddr string, expiresAt time.Time) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sessions (user_id, token_hash, kind, user_agent, remote_addr, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		userID, tokenHash, kind, userAgent, remoteAddr, expiresAt)
	if err != nil {
		return fmt.Errorf("creating session: %w", err)
	}
	return nil
}

// UserForSession resolves a session token hash to its user, refreshing the
// last-seen timestamp.
func (s *Store) UserForSession(ctx context.Context, tokenHash []byte) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`UPDATE sessions SET last_seen_at = now()
		 WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()
		 RETURNING (SELECT id FROM users WHERE id = sessions.user_id),
		           (SELECT email FROM users WHERE id = sessions.user_id),
		           (SELECT password_hash FROM users WHERE id = sessions.user_id),
		           (SELECT is_admin FROM users WHERE id = sessions.user_id),
		           (SELECT disabled_at FROM users WHERE id = sessions.user_id),
		           (SELECT created_at FROM users WHERE id = sessions.user_id)`, tokenHash).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.IsAdmin, &u.DisabledAt, &u.CreatedAt)
	if err != nil {
		return User{}, notFound(err)
	}
	return u, nil
}

// RevokeSession signs a session out.
func (s *Store) RevokeSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE token_hash = $1`, tokenHash)
	return err
}

// DeleteExpiredSessions removes sessions that can no longer be used.
func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM sessions WHERE expires_at < now() - interval '7 days'`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ListAccountsForUser returns the accounts a user owns.
func (s *Store) ListAccountsForUser(ctx context.Context, userID int64) ([]Account, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+accountColumns+` FROM mail_accounts WHERE user_id = $1 ORDER BY email_address`,
		userID)
	if err != nil {
		return nil, fmt.Errorf("listing accounts: %w", err)
	}
	defer rows.Close()

	var out []Account
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, account)
	}
	return out, rows.Err()
}

// CreateAccountParams describes a new mail account.
type CreateAccountParams struct {
	UserID            int64
	DisplayName       string
	EmailAddress      string
	UpstreamHost      string
	UpstreamPort      int
	UpstreamTLSMode   string
	EncryptedUsername []byte
	EncryptedSecret   []byte
}

// CreateAccount adds a mail account to mirror.
func (s *Store) CreateAccount(ctx context.Context, p CreateAccountParams) (Account, error) {
	row := s.pool.QueryRow(ctx,
		`INSERT INTO mail_accounts
		   (user_id, display_name, email_address, upstream_host, upstream_port,
		    upstream_tls_mode, encrypted_username, encrypted_secret)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		 RETURNING `+accountColumns,
		p.UserID, p.DisplayName, p.EmailAddress, p.UpstreamHost, p.UpstreamPort,
		p.UpstreamTLSMode, p.EncryptedUsername, p.EncryptedSecret)

	account, err := scanAccount(row)
	if err != nil {
		return Account{}, fmt.Errorf("creating account %s: %w", p.EmailAddress, err)
	}
	return account, nil
}

// DeleteAccount removes an account and everything belonging to it.
func (s *Store) DeleteAccount(ctx context.Context, accountID int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM mail_accounts WHERE id = $1`, accountID)
	return err
}
