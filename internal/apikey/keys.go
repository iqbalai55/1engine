package apikey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type KeyID string
type TenantID string
type Plaintext string

type Key struct {
	ID        KeyID
	Tenant    TenantID
	Name      string
	CreatedAt string
	RevokedAt *string
}
type database interface {
	Begin(context.Context) (pgx.Tx, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type Service struct {
	DB           database
	randomSource io.Reader
}

func secret() (Plaintext, []byte, error) {
	return secretFrom(rand.Reader)
}

func secretFrom(source io.Reader) (Plaintext, []byte, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(source, b); err != nil {
		return "", nil, err
	}
	value := "ik_" + base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(value))
	return Plaintext(value), h[:], nil
}

func (s *Service) create(ctx context.Context, tenant TenantID, name string, tx pgx.Tx) (KeyID, Plaintext, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return "", "", errors.New("key name must contain 1 to 100 characters")
	}
	source := s.randomSource
	if source == nil {
		source = rand.Reader
	}
	plain, hash, err := secretFrom(source)
	if err != nil {
		return "", "", err
	}
	var id KeyID
	err = tx.QueryRow(ctx, `INSERT INTO api_keys(tenant_id,name,secret_hash) VALUES($1,$2,$3) RETURNING id`, string(tenant), name, hash).Scan(&id)
	return id, plain, err
}

func (s *Service) Issue(ctx context.Context, tenant TenantID, name string) (KeyID, Plaintext, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)
	id, plain, err := s.create(ctx, tenant, name, tx)
	if err != nil {
		return "", "", err
	}
	return id, plain, tx.Commit(ctx)
}

func (s *Service) Rotate(ctx context.Context, tenant TenantID, old KeyID, name string) (KeyID, Plaintext, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)
	var exists string
	if err := tx.QueryRow(ctx, `SELECT id FROM api_keys WHERE id=$1 AND tenant_id=$2 AND revoked_at IS NULL FOR UPDATE`, string(old), string(tenant)).Scan(&exists); err != nil {
		return "", "", errors.New("active API key not found")
	}
	id, plain, err := s.create(ctx, tenant, name, tx)
	if err != nil {
		return "", "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE api_keys SET revoked_at=now() WHERE id=$1 AND tenant_id=$2 AND revoked_at IS NULL`, string(old), string(tenant)); err != nil {
		return "", "", err
	}
	return id, plain, tx.Commit(ctx)
}

func (s *Service) Revoke(ctx context.Context, tenant TenantID, id KeyID) error {
	result, err := s.DB.Exec(ctx, `UPDATE api_keys SET revoked_at=now() WHERE id=$1 AND tenant_id=$2 AND revoked_at IS NULL`, string(id), string(tenant))
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return errors.New("active API key not found")
	}
	return nil
}

func (s *Service) List(ctx context.Context, tenant TenantID) ([]Key, error) {
	rows, err := s.DB.Query(ctx, `SELECT id,tenant_id,name,created_at::text,revoked_at::text FROM api_keys WHERE tenant_id=$1 ORDER BY created_at DESC`, string(tenant))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []Key
	for rows.Next() {
		var key Key
		if err := rows.Scan(&key.ID, &key.Tenant, &key.Name, &key.CreatedAt, &key.RevokedAt); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (s *Service) Authenticate(ctx context.Context, raw string) (TenantID, error) {
	h := sha256.Sum256([]byte(raw))
	var tenant TenantID
	err := s.DB.QueryRow(ctx, `SELECT tenant_id FROM api_keys WHERE secret_hash=$1 AND revoked_at IS NULL`, h[:]).Scan(&tenant)
	return tenant, err
}
