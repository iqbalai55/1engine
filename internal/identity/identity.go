package identity

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/smtp"
	"net/url"
	"strings"
	"time"

	"billing-backend/internal/domain"
	"billing-backend/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/argon2"
)

type Mailer interface {
	SendVerification(ctx context.Context, email, token string) error
}

// LogMailer writes the verification link to the service log instead of sending it.
// Use it for development only: anyone who can read the logs can verify an address.
type LogMailer struct{ PublicBaseURL string }

func (m LogMailer) SendVerification(ctx context.Context, email, token string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	link := strings.TrimRight(m.PublicBaseURL, "/") + "/verify-email?token=" + url.QueryEscape(token)
	log.Printf("verification link for %s: %s", email, link)
	return nil
}

type SMTPMailer struct {
	Host          string
	Port          string
	Username      string
	Password      string
	From          string
	PublicBaseURL string
}

func (m SMTPMailer) SendVerification(ctx context.Context, email, token string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	link := strings.TrimRight(m.PublicBaseURL, "/") + "/verify-email?token=" + url.QueryEscape(token)
	body := []byte("To: " + email + "\r\nSubject: Verify your account\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nOpen this link to verify your account:\r\n" + link + "\r\n")
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", net.JoinHostPort(m.Host, m.Port), &tls.Config{ServerName: m.Host, MinVersion: tls.VersionTLS12})
	if err != nil {
		return err
	}
	defer conn.Close()
	client, err := smtp.NewClient(conn, m.Host)
	if err != nil {
		return err
	}
	defer client.Close()
	if err = client.Auth(smtp.PlainAuth("", m.Username, m.Password, m.Host)); err != nil {
		return err
	}
	if err = client.Mail(m.From); err != nil {
		return err
	}
	if err = client.Rcpt(email); err != nil {
		return err
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err = writer.Write(body); err != nil {
		return err
	}
	if err = writer.Close(); err != nil {
		return err
	}
	if err = client.Quit(); err != nil {
		return fmt.Errorf("SMTP delivery completion failed: %w", err)
	}
	return nil
}

type Service struct {
	DB            *pgxpool.Pool
	Mailer        Mailer
	Tenants       TenantRegistrar
	Accounts      CreditAccountOpener
	SessionTTL    time.Duration
	SessionSecret []byte
}

type TenantRegistrar interface {
	CreateForOwner(context.Context, store.Querier, domain.UserID, string, string) (domain.TenantID, error)
}

type CreditAccountOpener interface {
	OpenAccount(context.Context, store.Querier, domain.TenantID) error
}
type Session struct {
	Token     string
	UserID    string
	ExpiresAt time.Time
}

func NewToken() (string, []byte, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	h := sha256.Sum256([]byte(token))
	return token, h[:], nil
}

func hashPassword(password string) (string, error) {
	if len(password) < 12 || len(password) > 1024 {
		return "", errors.New("password must contain 12 to 1024 bytes")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return "$argon2id$v=19$m=65536,t=3,p=2$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(hash), nil
}

// timingDummyHash makes logins for unknown or unverified accounts run the same argon2 work as real ones.
var timingDummyHash, _ = hashPassword("timing-equalizer-password")

func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	salt, e1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, e2 := base64.RawStdEncoding.DecodeString(parts[5])
	if e1 != nil || e2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, uint32(len(want)))
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

func (s *Service) SignUp(ctx context.Context, email, password string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if !validEmail(email) {
		return errors.New("invalid email")
	}
	if s.Mailer == nil {
		return errors.New("verification email delivery is unavailable")
	}
	passwordHash, err := hashPassword(password)
	if err != nil {
		return err
	}
	token, tokenHash, err := NewToken()
	if err != nil {
		return err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var userID string
	err = tx.QueryRow(ctx, `INSERT INTO users(email,password_hash) VALUES($1,$2) ON CONFLICT(email) DO UPDATE SET password_hash=EXCLUDED.password_hash WHERE users.email_verified_at IS NULL RETURNING id`, email, passwordHash).Scan(&userID)
	if err != nil {
		return errors.New("email already registered")
	}
	_, err = tx.Exec(ctx, `INSERT INTO verification_challenges(user_id,token_hash,expires_at) VALUES($1,$2,$3) ON CONFLICT(user_id) DO UPDATE SET token_hash=EXCLUDED.token_hash,expires_at=EXCLUDED.expires_at,consumed_at=NULL`, userID, tokenHash, time.Now().Add(30*time.Minute))
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	return s.Mailer.SendVerification(ctx, email, token)
}

func validEmail(email string) bool {
	if len(email) > 320 {
		return false
	}
	parts := strings.Split(email, "@")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || !strings.Contains(parts[1], ".") {
		return false
	}
	for _, r := range email {
		if r < 33 || r > 126 {
			return false
		}
	}
	return true
}

func (s *Service) VerifyEmail(ctx context.Context, token string) (string, error) {
	h := sha256.Sum256([]byte(token))
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var userID string
	err = tx.QueryRow(ctx, `SELECT user_id FROM verification_challenges WHERE token_hash=$1 AND consumed_at IS NULL AND expires_at>now() FOR UPDATE`, h[:]).Scan(&userID)
	if err != nil {
		return "", errors.New("verification token is invalid or expired")
	}
	if _, err = tx.Exec(ctx, `UPDATE verification_challenges SET consumed_at=now() WHERE user_id=$1`, userID); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `UPDATE users SET email_verified_at=COALESCE(email_verified_at,now()) WHERE id=$1`, userID); err != nil {
		return "", err
	}
	var nonce [8]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return "", err
	}
	suffix := base64.RawURLEncoding.EncodeToString(nonce[:])
	tenantID, err := s.Tenants.CreateForOwner(ctx, tx, domain.UserID(userID), "tenant-"+suffix, "project-"+suffix)
	if err != nil {
		return "", err
	}
	if tenantID == "" {
		return "", errors.New("tenant activation did not return a tenant")
	}
	if err = s.Accounts.OpenAccount(ctx, tx, tenantID); err != nil {
		return "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	return userID, nil
}

func (s *Service) Login(ctx context.Context, email, password string) (Session, error) {
	if len(s.SessionSecret) < 32 {
		return Session{}, errors.New("portal session signing secret is unavailable")
	}
	var userID, encoded string
	var verified *time.Time
	err := s.DB.QueryRow(ctx, `SELECT id,password_hash,email_verified_at FROM users WHERE email=$1`, strings.ToLower(strings.TrimSpace(email))).Scan(&userID, &encoded, &verified)
	if err != nil {
		encoded = timingDummyHash
	}
	passwordMatches := verifyPassword(encoded, password)
	if err != nil || verified == nil || !passwordMatches {
		return Session{}, errors.New("invalid email or password")
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
