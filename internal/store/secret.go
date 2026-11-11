package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// OAuthToken is the secret.oauth_token row of one source. Its fields are never logged.
type OAuthToken struct {
	Source       string
	AccessToken  string
	RefreshToken string
	IssuedAt     time.Time // when the access token was issued; the backfill deadline counts from it
	ExpiresAt    time.Time
	UpdatedAt    time.Time
}

// GetOAuthToken returns the token row of source, or ErrNotFound.
func GetOAuthToken(ctx context.Context, q DBTX, source string) (OAuthToken, error) {
	var t OAuthToken
	err := q.QueryRow(ctx, `SELECT source, access_token, refresh_token, issued_at, expires_at, updated_at
		FROM secret.oauth_token WHERE source = $1`, source).
		Scan(&t.Source, &t.AccessToken, &t.RefreshToken, &t.IssuedAt, &t.ExpiresAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OAuthToken{}, ErrNotFound
	}
	if err != nil {
		return OAuthToken{}, fmt.Errorf("store: get oauth token: %w", err)
	}
	return t, nil
}

// PutOAuthToken replaces the token row of t.Source in one statement. Run on the pool (Store.DB)
// it is committed, and so durable, when it returns; run inside a transaction it is durable only
// once that commits, so a refreshed pair must not be used before then.
func PutOAuthToken(ctx context.Context, q DBTX, t OAuthToken) error {
	_, err := q.Exec(ctx, `INSERT INTO secret.oauth_token (source, access_token, refresh_token, issued_at, expires_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (source) DO UPDATE SET access_token = EXCLUDED.access_token,
		  refresh_token = EXCLUDED.refresh_token, issued_at = EXCLUDED.issued_at,
		  expires_at = EXCLUDED.expires_at, updated_at = EXCLUDED.updated_at`,
		t.Source, t.AccessToken, t.RefreshToken, t.IssuedAt, t.ExpiresAt, t.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store: put oauth token: %w", err)
	}
	return nil
}
