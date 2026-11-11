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
	"billing-backend/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct{ DB *pgxpool.Pool }

func (s *Service) StateAndMarkup(ctx context.Context, tx store.Querier, id domain.TenantID) (domain.MarkupBPS, domain.TenantState, error) {
	var markup domain.MarkupBPS
	var state domain.TenantState
	err := tx.QueryRow(ctx, `SELECT markup_bps,state FROM tenants WHERE id=$1`, string(id)).Scan(&markup, &state)
	return markup, state, err
}

func (s *Service) CreateForOwner(ctx context.Context, tx store.Querier, owner domain.UserID, username, project string) (domain.TenantID, error) {
	var id domain.TenantID
	err := tx.QueryRow(ctx, `INSERT INTO tenants(owner_user_id,dstack_username,dstack_project) VALUES($1,$2,$3) ON CONFLICT(owner_user_id) DO NOTHING RETURNING id`, string(owner), username, project).Scan(&id)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
		if err := tx.QueryRow(ctx, `SELECT id FROM tenants WHERE owner_user_id=$1`, string(owner)).Scan(&id); err != nil {
			return "", err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE tenants SET state='provisioning' WHERE owner_user_id=$1`, string(owner))
	return id, err
}

func (s *Service) FindByOwner(ctx context.Context, owner domain.UserID) (domain.TenantID, error) {
	var id domain.TenantID
	err := s.DB.QueryRow(ctx, `SELECT id FROM tenants WHERE owner_user_id=$1`, string(owner)).Scan(&id)
	return id, err
}

func (s *Service) Exists(ctx context.Context, id domain.TenantID) (bool, error) {
	var exists bool
	err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tenants WHERE id=$1)`, string(id)).Scan(&exists)
	return exists, err
}

func (s *Service) ListAwaitingProvisioning(ctx context.Context, limit int) ([]domain.TenantID, error) {
	rows, err := s.DB.Query(ctx, `SELECT id FROM tenants WHERE dstack_provision_state IN ('pending','failed') ORDER BY created_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []domain.TenantID
	for rows.Next() {
		var id domain.TenantID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Service) ListReadyForUsage(ctx context.Context, limit int) ([]domain.TenantID, error) {
	rows, err := s.DB.Query(ctx, `SELECT t.id FROM tenants t WHERE t.state='ready' AND t.dstack_provision_state='ready' ORDER BY COALESCE((SELECT min(c.updated_at) FROM dstack_poll_cursors c WHERE c.tenant_id=t.id),to_timestamp(0)),t.created_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []domain.TenantID
	for rows.Next() {
		var id domain.TenantID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Service) HasCredential(ctx context.Context, id domain.TenantID) (bool, error) {
	var exists bool
	err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tenant_dstack_credentials WHERE tenant_id=$1)`, string(id)).Scan(&exists)
	return exists, err
}

func (s *Service) SetProjectID(ctx context.Context, id domain.TenantID, projectID string) error {
	_, err := s.DB.Exec(ctx, `UPDATE tenants SET dstack_project_id=$2 WHERE id=$1 AND (dstack_project_id IS NULL OR dstack_project_id=$2)`, string(id), projectID)
	return err
}

func (s *Service) SetAccountUsername(ctx context.Context, id domain.TenantID, username string) error {
	tag, err := s.DB.Exec(ctx, `UPDATE tenants SET dstack_account_username=$2 WHERE id=$1 AND (dstack_account_username IS NULL OR dstack_account_username=$2)`, string(id), username)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("tenant already belongs to another dstack account")
	}
	return nil
}

func (s *Service) MarkProvisionFailed(ctx context.Context, id domain.TenantID) error {
	_, err := s.DB.Exec(ctx, `UPDATE tenants SET dstack_provision_state='failed',state='provisioning' WHERE id=$1`, string(id))
	return err
}

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
	rows, err := s.DB.Query(ctx, `SELECT id,owner_user_id,dstack_username,dstack_project,dstack_provision_state,markup_bps,state,COALESCE(dstack_account_username,'') FROM tenants ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tenants []domain.Tenant
	for rows.Next() {
		var t domain.Tenant
		if err := rows.Scan(&t.ID, &t.Owner, &t.DStack.Username, &t.DStack.Project, &t.DStack.Provision, &t.Markup, &t.State, &t.DStack.AccountUsername); err != nil {
			return nil, err
		}
		tenants = append(tenants, t)
	}
	return tenants, rows.Err()
}

func (s *Service) Get(ctx context.Context, id domain.TenantID) (domain.Tenant, error) {
	var t domain.Tenant
	err := s.DB.QueryRow(ctx, `SELECT id,owner_user_id,dstack_username,dstack_project,dstack_provision_state,markup_bps,state,COALESCE(dstack_project_id::text,''),COALESCE(dstack_account_username,'') FROM tenants WHERE id=$1`, string(id)).Scan(&t.ID, &t.Owner, &t.DStack.Username, &t.DStack.Project, &t.DStack.Provision, &t.Markup, &t.State, &t.DStack.ProjectID, &t.DStack.AccountUsername)
	return t, err
}

func (s *Service) Markup(ctx context.Context, id domain.TenantID) (domain.MarkupBPS, error) {
	var markup domain.MarkupBPS
	err := s.DB.QueryRow(ctx, `SELECT markup_bps FROM tenants WHERE id=$1`, string(id)).Scan(&markup)
	return markup, err
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
