package apikey

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type failingRandomReader struct{}

func (failingRandomReader) Read([]byte) (int, error) {
	return 0, errors.New("random source failed")
}

func TestSecretFromPropagatesRandomReaderFailure(t *testing.T) {
	plain, hash, err := secretFrom(failingRandomReader{})
	if err == nil || err.Error() != "random source failed" || plain != "" || hash != nil {
		t.Fatalf("secretFrom() = (%q, %x, %v), want empty output and random source error", plain, hash, err)
	}
}

type fakeRow struct {
	values []any
	err    error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, value := range r.values {
		switch target := dest[i].(type) {
		case *string:
			*target = value.(string)
		case *KeyID:
			*target = value.(KeyID)
		default:
			return errors.New("unsupported scan target")
		}
	}
	return nil
}

type fakeRows struct {
	scanError error
	rowError  error
	rows      int
}

func (r *fakeRows) Close()                                       {}
func (r *fakeRows) Err() error                                   { return r.rowError }
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) Next() bool {
	if r.rows == 0 {
		return false
	}
	r.rows--
	return true
}
func (r *fakeRows) Scan(...any) error      { return r.scanError }
func (r *fakeRows) Values() ([]any, error) { return nil, nil }
func (r *fakeRows) RawValues() [][]byte    { return nil }
func (r *fakeRows) Conn() *pgx.Conn        { return nil }

type fakeTx struct {
	insertError error
	updateError error
	inserted    bool
}

func (*fakeTx) Conn() *pgx.Conn { return nil }
func (*fakeTx) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("unexpected nested transaction")
}
func (*fakeTx) Commit(context.Context) error   { return nil }
func (*fakeTx) Rollback(context.Context) error { return nil }
func (*fakeTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, nil
}
func (*fakeTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return nil }
func (*fakeTx) LargeObjects() pgx.LargeObjects                         { return pgx.LargeObjects{} }
func (*fakeTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return nil, nil
}
func (tx *fakeTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if tx.updateError != nil {
		return pgconn.CommandTag{}, tx.updateError
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}
func (*fakeTx) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, nil }
func (tx *fakeTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if strings.Contains(sql, "INSERT INTO api_keys") {
		tx.inserted = true
		return fakeRow{values: []any{KeyID("11111111-1111-4111-8111-111111111111")}, err: tx.insertError}
	}
	return fakeRow{values: []any{"11111111-1111-4111-8111-111111111111"}}
}

type fakeDatabase struct {
	tx         *fakeTx
	beginError error
	execError  error
	queryError error
	rows       pgx.Rows
}

func (db fakeDatabase) Begin(context.Context) (pgx.Tx, error) { return db.tx, db.beginError }
func (db fakeDatabase) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	if db.execError != nil {
		return pgconn.CommandTag{}, db.execError
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}
func (db fakeDatabase) Query(context.Context, string, ...any) (pgx.Rows, error) {
	if db.queryError != nil {
		return nil, db.queryError
	}
	return db.rows, nil
}
func (db fakeDatabase) QueryRow(context.Context, string, ...any) pgx.Row {
	return fakeRow{values: []any{"22222222-2222-4222-8222-222222222222"}}
}

func TestIssueReturnsSecretGenerationError(t *testing.T) {
	tx := &fakeTx{}
	service := Service{DB: fakeDatabase{tx: tx}, randomSource: failingRandomReader{}}
	id, plain, err := service.Issue(context.Background(), "tenant", "deploy")
	if err == nil || id != "" || plain != "" || tx.inserted {
		t.Fatalf("Issue() = (%q, %q, %v), want empty values, no insert, and a secret error", id, plain, err)
	}
}

func TestIssueReturnsTransactionBeginError(t *testing.T) {
	want := errors.New("begin failed")
	service := Service{DB: fakeDatabase{tx: &fakeTx{}, beginError: want}}
	if _, _, err := service.Issue(context.Background(), "tenant", "deploy"); !errors.Is(err, want) {
		t.Fatalf("Issue() error = %v, want begin error", err)
	}
}

func TestRotateReturnsTransactionBeginError(t *testing.T) {
	want := errors.New("begin failed")
	service := Service{DB: fakeDatabase{tx: &fakeTx{}, beginError: want}}
	if _, _, err := service.Rotate(context.Background(), "tenant", "key", "deploy"); !errors.Is(err, want) {
		t.Fatalf("Rotate() error = %v, want begin error", err)
	}
}

func TestRotateReturnsRevocationError(t *testing.T) {
	want := errors.New("revocation failed")
	service := Service{DB: fakeDatabase{tx: &fakeTx{updateError: want}}}
	if _, _, err := service.Rotate(context.Background(), "tenant", "key", "deploy"); !errors.Is(err, want) {
		t.Fatalf("Rotate() error = %v, want revocation error", err)
	}
}

func TestRevokeReturnsUpdateError(t *testing.T) {
	want := errors.New("update failed")
	service := Service{DB: fakeDatabase{execError: want}}
	if err := service.Revoke(context.Background(), "tenant", "key"); !errors.Is(err, want) {
		t.Fatalf("Revoke() error = %v, want update error", err)
	}
}

func TestListReturnsQueryError(t *testing.T) {
	want := errors.New("query failed")
	service := Service{DB: fakeDatabase{queryError: want}}
	if keys, err := service.List(context.Background(), "tenant"); !errors.Is(err, want) || keys != nil {
		t.Fatalf("List() = (%v, %v), want nil keys and query error", keys, err)
	}
}

func TestListReturnsRowScanError(t *testing.T) {
	want := errors.New("scan failed")
	service := Service{DB: fakeDatabase{rows: &fakeRows{scanError: want, rows: 1}}}
	if keys, err := service.List(context.Background(), "tenant"); !errors.Is(err, want) || keys != nil {
		t.Fatalf("List() = (%v, %v), want nil keys and scan error", keys, err)
	}
}

func TestListReturnsRowsIterationError(t *testing.T) {
	want := errors.New("iteration failed")
	service := Service{DB: fakeDatabase{rows: &fakeRows{rowError: want}}}
	if keys, err := service.List(context.Background(), "tenant"); !errors.Is(err, want) || keys != nil {
		t.Fatalf("List() = (%v, %v), want nil keys and iteration error", keys, err)
	}
}
