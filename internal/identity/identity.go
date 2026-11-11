package identity

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"billing-backend/internal/domain"
	"billing-backend/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	DB            *pgxpool.Pool
	Google        GoogleOAuth
	Tenants       TenantRegistrar
	Accounts      CreditAccountOpener
	SessionTTL    time.Duration
	SessionSecret []byte
}

type TenantRegistrar interface {
	EnsureForOwner(context.Context, store.Querier, domain.UserID, string, string) (domain.TenantID, error)
}

type CreditAccountOpener interface {
	OpenAccount(context.Context, store.Querier, domain.TenantID) error
}

type Session struct {
	Token     string
	UserID    string
	ExpiresAt time.Time
}

const googleStateTTL = 10 * time.Minute

func NewToken() (string, []byte, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	h := sha256.Sum256([]byte(token))
	return token, h[:], nil
}

// StartGoogle returns the Google authorization URL and the signed state that
// binds the browser to the callback.
func (s *Service) StartGoogle() (string, string, error) {
	if s.Google == nil {
		return "", "", errors.New("google sign-in is unavailable")
	}
	state, err := s.signState()
	if err != nil {
		return "", "", err
	}
	return s.Google.AuthURL(state), state, nil
}

func (s *Service) CompleteGoogle(ctx context.Context, code, state string) (Session, error) {
	if s.Google == nil {
		return Session{}, errors.New("google sign-in is unavailable")
	}
	if err := s.verifyState(state); err != nil {
		return Session{}, err
	}
	identity, err := s.Google.Exchange(ctx, code)
	if err != nil {
		return Session{}, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback(ctx)
	userID, err := linkGoogleUser(ctx, tx, identity)
	if err != nil {
		return Session{}, err
	}
	suffix, err := randomSuffix()
	if err != nil {
		return Session{}, err
	}
	tenantID, err := s.Tenants.EnsureForOwner(ctx, tx, domain.UserID(userID), "tenant-"+suffix, "project-"+suffix)
	if err != nil {
		return Session{}, err
	}
	if tenantID == "" {
		return Session{}, errors.New("tenant activation did not return a tenant")
	}
	if err := s.Accounts.OpenAccount(ctx, tx, tenantID); err != nil {
		return Session{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Session{}, err
	}
	return s.issueSession(ctx, userID)
}

func (s *Service) signState() (string, error) {
	if len(s.SessionSecret) < 32 {
		return "", errors.New("portal session signing secret is unavailable")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(nonce) + "." + strconv.FormatInt(time.Now().UTC().Unix(), 10)
	mac := hmac.New(sha256.New, s.SessionSecret)
	_, _ = mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *Service) verifyState(state string) error {
	if len(s.SessionSecret) < 32 {
		return errors.New("portal session signing secret is unavailable")
	}
	parts := strings.Split(state, ".")
	if len(parts) != 3 {
		return errors.New("google sign-in state is invalid")
	}
	payload := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, s.SessionSecret)
	_, _ = mac.Write([]byte(payload))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(parts[2])) {
		return errors.New("google sign-in state is invalid")
	}
	issued, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return errors.New("google sign-in state is invalid")
	}
	age := time.Since(time.Unix(issued, 0))
	if age < -time.Minute || age > googleStateTTL {
		return errors.New("google sign-in state has expired")
	}
	return nil
}

func linkGoogleUser(ctx context.Context, tx store.Querier, google GoogleIdentity) (string, error) {
	var userID string
	err := tx.QueryRow(ctx, `SELECT id FROM users WHERE google_subject=$1 FOR UPDATE`, google.Subject).Scan(&userID)
	if err == nil {
		if _, err := tx.Exec(ctx, `UPDATE users SET email_verified_at=COALESCE(email_verified_at,now()) WHERE id=$1`, userID); err != nil {
			return "", err
		}
		return userID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	err = tx.QueryRow(ctx, `SELECT id FROM users WHERE email=$1 FOR UPDATE`, google.Email).Scan(&userID)
	if err == nil {
		tag, err := tx.Exec(ctx, `UPDATE users SET google_subject=$2, email_verified_at=COALESCE(email_verified_at,now()) WHERE id=$1 AND google_subject IS NULL`, userID, google.Subject)
		if err != nil {
			return "", err
		}
		if tag.RowsAffected() == 0 {
			return "", errors.New("this email is already linked to another Google account")
		}
		return userID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	err = tx.QueryRow(ctx, `INSERT INTO users(email,google_subject,email_verified_at) VALUES($1,$2,now()) RETURNING id`, google.Email, google.Subject).Scan(&userID)
	return userID, err
}

func randomSuffix() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func (s *Service) issueSession(ctx context.Context, userID string) (Session, error) {
	if len(s.SessionSecret) < 32 {
		return Session{}, errors.New("portal session signing secret is unavailable")
	}
	token, tokenHash, err := NewToken()
	if err != nil {
		return Session{}, err
	}
	mac := hmac.New(sha256.New, s.SessionSecret)
	_, _ = mac.Write(tokenHash)
	tokenHash = mac.Sum(nil)
	ttl := s.SessionTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	expires := time.Now().Add(ttl)
	_, err = s.DB.Exec(ctx, `INSERT INTO portal_sessions(user_id,token_hash,expires_at) VALUES($1,$2,$3)`, userID, tokenHash, expires)
	return Session{Token: token, UserID: userID, ExpiresAt: expires}, err
}

func (s *Service) Authenticate(ctx context.Context, token string) (string, error) {
	if len(s.SessionSecret) < 32 {
		return "", errors.New("portal session signing secret is unavailable")
	}
	h := sha256.Sum256([]byte(token))
	mac := hmac.New(sha256.New, s.SessionSecret)
	_, _ = mac.Write(h[:])
	storedHash := mac.Sum(nil)
	var userID string
	err := s.DB.QueryRow(ctx, `SELECT user_id FROM portal_sessions WHERE token_hash=$1 AND revoked_at IS NULL AND expires_at>now()`, storedHash).Scan(&userID)
	return userID, err
}

func (s *Service) RevokeSession(ctx context.Context, token string) error {
	h := sha256.Sum256([]byte(token))
	mac := hmac.New(sha256.New, s.SessionSecret)
	_, _ = mac.Write(h[:])
	_, err := s.DB.Exec(ctx, `UPDATE portal_sessions SET revoked_at=now() WHERE token_hash=$1 AND revoked_at IS NULL`, mac.Sum(nil))
	return err
}

func (s *Service) Email(ctx context.Context, userID string) (string, error) {
	var email string
	err := s.DB.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, userID).Scan(&email)
	return email, err
}

func (s *Service) IsProvider(ctx context.Context, userID string) (bool, error) {
	var allowed bool
	err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM provider_admins WHERE user_id=$1)`, userID).Scan(&allowed)
	return allowed, err
}

func BootstrapProvider(ctx context.Context, db *pgxpool.Pool, email string) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var id string
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE lower(email)=lower($1) AND email_verified_at IS NOT NULL FOR UPDATE`, email).Scan(&id); err != nil {
		return errors.New("provider user must have a verified portal account")
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM provider_admins`).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return errors.New("provider admin has already been bootstrapped")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO provider_admins(slot,user_id) VALUES(1,$1)`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
