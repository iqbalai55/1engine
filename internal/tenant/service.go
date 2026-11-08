package tenant

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"strings"

	"billing-backend/internal/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct{ DB *pgxpool.Pool }
type DStackAdmin struct {
	BaseURL string
	Token   string
	HTTP    Doer
}
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

func EncryptCredential(key []byte, plaintext string) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("dstack credential encryption key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

func DecryptCredential(key, ciphertext []byte) (string, error) {
	if len(key) != 32 {
		return "", errors.New("dstack credential encryption key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return "", errors.New("invalid encrypted dstack credential")
	}
	plain, err := gcm.Open(nil, ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():], nil)
	return string(plain), err
}

func (s *Service) SetMarkup(ctx context.Context, id domain.TenantID, bps domain.MarkupBPS) error {
	if bps > domain.MaxMarkupBPS {
		return errors.New("markup must be between 0 and 10000 basis points")
	}
	_, err := s.DB.Exec(ctx, `UPDATE tenants SET markup_bps=$2 WHERE id=$1`, string(id), int64(bps))
	return err
}

func (s *Service) List(ctx context.Context) ([]domain.Tenant, error) {
	rows, err := s.DB.Query(ctx, `SELECT id,owner_user_id,dstack_username,dstack_project,dstack_provision_state,markup_bps,state FROM tenants ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tenants []domain.Tenant
	for rows.Next() {
		var t domain.Tenant
		if err := rows.Scan(&t.ID, &t.Owner, &t.DStack.Username, &t.DStack.Project, &t.DStack.Provision, &t.Markup, &t.State); err != nil {
			return nil, err
		}
		tenants = append(tenants, t)
	}
	return tenants, rows.Err()
}

func (s *Service) Get(ctx context.Context, id domain.TenantID) (domain.Tenant, error) {
	var t domain.Tenant
	err := s.DB.QueryRow(ctx, `SELECT id,owner_user_id,dstack_username,dstack_project,dstack_provision_state,markup_bps,state,COALESCE(dstack_project_id::text,'') FROM tenants WHERE id=$1`, string(id)).Scan(&t.ID, &t.Owner, &t.DStack.Username, &t.DStack.Project, &t.DStack.Provision, &t.Markup, &t.State, &t.DStack.ProjectID)
	return t, err
}

func (s *Service) SaveCustomerCredential(ctx context.Context, id domain.TenantID, secret string, key []byte, keyVersion string) error {
	if strings.TrimSpace(secret) == "" {
		return errors.New("empty dstack credential")
	}
	ciphertext, err := EncryptCredential(key, secret)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO tenant_dstack_credentials(tenant_id,ciphertext,key_version) VALUES($1,$2,$3) ON CONFLICT(tenant_id) DO UPDATE SET ciphertext=EXCLUDED.ciphertext,key_version=EXCLUDED.key_version,updated_at=now()`, string(id), ciphertext, keyVersion)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `UPDATE tenants SET dstack_provision_state='ready',state='ready' WHERE id=$1`, string(id))
	return err
}

func (s *Service) CustomerCredential(ctx context.Context, id domain.TenantID, key []byte) (string, error) {
	var ciphertext []byte
	if err := s.DB.QueryRow(ctx, `SELECT ciphertext FROM tenant_dstack_credentials WHERE tenant_id=$1`, string(id)).Scan(&ciphertext); err != nil {
		return "", err
	}
	return DecryptCredential(key, ciphertext)
}

func readResponse(r io.Reader) ([]byte, error) { return io.ReadAll(io.LimitReader(r, 2<<20)) }
