package apikey_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"billing-backend/internal/apikey"
	"billing-backend/internal/identity"
	"billing-backend/internal/store"
)

// fixture holds one isolated tenant pair plus the API key service under test.
// The second tenant exists so that cross-tenant isolation is observable.
type fixture struct {
	keys  *apikey.Service
	owner apikey.TenantID
	other apikey.TenantID
}

func TestMain(m *testing.M) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url != "" {
		db, err := store.Open(context.Background(), url)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open test database: %v\n", err)
			os.Exit(1)
		}
		if err := db.Migrate(context.Background()); err != nil {
			db.Close()
			fmt.Fprintf(os.Stderr, "migrate test database: %v\n", err)
			os.Exit(1)
		}
		db.Close()
	}
	os.Exit(m.Run())
}

// newFixture creates unique users and tenants and registers cleanup for them.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is unset; skipping the PostgreSQL API key lifecycle test")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(db.Close)
	nonce, _, err := identity.NewToken()
	if err != nil {
		t.Fatalf("build unique fixture nonce: %v", err)
	}
	ownerUser := insertUser(t, db, fmt.Sprintf("apikey-owner-%d-%s@example.test", os.Getpid(), nonce))
	otherUser := insertUser(t, db, fmt.Sprintf("apikey-other-%d-%s@example.test", os.Getpid(), nonce))
	owner := insertTenant(t, db, ownerUser, "owner-"+nonce)
	other := insertTenant(t, db, otherUser, "other-"+nonce)
	t.Cleanup(func() {
		cleanup := context.Background()
		for _, tenant := range []string{owner, other} {
			if _, err := db.Pool.Exec(cleanup, `DELETE FROM api_keys WHERE tenant_id=$1`, tenant); err != nil {
				t.Errorf("delete fixture API keys: %v", err)
			}
		}
		for _, tenant := range []string{owner, other} {
			if _, err := db.Pool.Exec(cleanup, `DELETE FROM tenants WHERE id=$1`, tenant); err != nil {
				t.Errorf("delete fixture tenant %s: %v", tenant, err)
			}
		}
		for _, user := range []string{ownerUser, otherUser} {
			if _, err := db.Pool.Exec(cleanup, `DELETE FROM users WHERE id=$1`, user); err != nil {
				t.Errorf("delete fixture user %s: %v", user, err)
			}
		}
	})
	return &fixture{keys: &apikey.Service{DB: db.Pool}, owner: apikey.TenantID(owner), other: apikey.TenantID(other)}
}

func insertUser(t *testing.T, db *store.Postgres, email string) string {
	t.Helper()
	var id string
	if err := db.Pool.QueryRow(context.Background(), `INSERT INTO users(email,password_hash,email_verified_at) VALUES($1,'test',now()) RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("create fixture user: %v", err)
	}
	return id
}

func insertTenant(t *testing.T, db *store.Postgres, ownerUserID, suffix string) string {
	t.Helper()
	var id string
	if err := db.Pool.QueryRow(context.Background(), `INSERT INTO tenants(owner_user_id,dstack_username,dstack_project,state) VALUES($1,$2,$3,'ready') RETURNING id`, ownerUserID, "apikey-"+suffix, "apikey-project-"+suffix).Scan(&id); err != nil {
		t.Fatalf("create fixture tenant: %v", err)
	}
	return id
}

func listedKey(t *testing.T, keys []apikey.Key, id apikey.KeyID) apikey.Key {
	t.Helper()
	for _, key := range keys {
		if key.ID == id {
			return key
		}
	}
	t.Fatalf("key %s is missing from the tenant key listing", id)
	return apikey.Key{}
}

func TestIssueReturnsKeyThatAuthenticatesForOwningTenant(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id, plain, err := f.keys.Issue(ctx, f.owner, "deploy")
	if err != nil {
		t.Fatalf("issue key: %v", err)
	}
	if id == "" {
		t.Fatal("issued key has no id")
	}
	if plain == "" {
		t.Fatal("issued key has no plaintext secret")
	}
	tenant, err := f.keys.Authenticate(ctx, string(plain))
	if err != nil {
		t.Fatalf("issued key does not authenticate: %v", err)
	}
	if tenant != f.owner {
		t.Fatalf("issued key authenticated tenant %q, want %q", tenant, f.owner)
	}
	keys, err := f.keys.List(ctx, f.owner)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	key := listedKey(t, keys, id)
	if key.Name != "deploy" {
		t.Fatalf("listed key name = %q, want deploy", key.Name)
	}
	if key.RevokedAt != nil {
		t.Fatalf("freshly issued key is listed as revoked at %q", *key.RevokedAt)
	}
	if err := f.keys.Revoke(ctx, f.owner, id); err != nil {
		t.Fatalf("revoke issued key: %v", err)
	}
}

func TestEachTenantSeesAndAuthenticatesOnlyItsOwnKeys(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, ownerPlain, err := f.keys.Issue(ctx, f.owner, "owner-key")
	if err != nil {
		t.Fatalf("issue owner key: %v", err)
	}
	otherID, otherPlain, err := f.keys.Issue(ctx, f.other, "other-key")
	if err != nil {
		t.Fatalf("issue other key: %v", err)
	}
	tenant, err := f.keys.Authenticate(ctx, string(ownerPlain))
	if err != nil {
		t.Fatalf("owner key does not authenticate: %v", err)
	}
	if tenant != f.owner {
		t.Fatalf("owner key authenticated tenant %q, want %q", tenant, f.owner)
	}
	tenant, err = f.keys.Authenticate(ctx, string(otherPlain))
	if err != nil {
		t.Fatalf("other key does not authenticate: %v", err)
	}
	if tenant != f.other {
		t.Fatalf("other key authenticated tenant %q, want %q", tenant, f.other)
	}
	otherKeys, err := f.keys.List(ctx, f.other)
	if err != nil {
		t.Fatalf("list other tenant keys: %v", err)
	}
	if len(otherKeys) != 1 {
		t.Fatalf("other tenant has %d keys, want 1", len(otherKeys))
	}
	for _, key := range otherKeys {
		if key.ID != otherID {
			t.Fatalf("other tenant lists key %s, want its own key %s", key.ID, otherID)
		}
	}
	if err := f.keys.Revoke(ctx, f.other, otherID); err != nil {
		t.Fatalf("revoke other tenant key: %v", err)
	}
}

func TestRotateRetiresOldKeyAndActivatesNewKey(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	oldID, oldPlain, err := f.keys.Issue(ctx, f.owner, "deploy")
	if err != nil {
		t.Fatalf("issue key: %v", err)
	}
	newID, newPlain, err := f.keys.Rotate(ctx, f.owner, oldID, "deploy-next")
	if err != nil {
		t.Fatalf("rotate key: %v", err)
	}
	if newID == oldID {
		t.Fatal("rotate returned the retired key id")
	}
	if newPlain == oldPlain {
		t.Fatal("rotate returned the retired plaintext secret")
	}
	if _, err := f.keys.Authenticate(ctx, string(oldPlain)); err == nil {
		t.Fatal("retired key still authenticates after rotate")
	}
	tenant, err := f.keys.Authenticate(ctx, string(newPlain))
	if err != nil {
		t.Fatalf("rotated key does not authenticate: %v", err)
	}
	if tenant != f.owner {
		t.Fatalf("rotated key authenticated tenant %q, want %q", tenant, f.owner)
	}
	keys, err := f.keys.List(ctx, f.owner)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	retired := listedKey(t, keys, oldID)
	if retired.RevokedAt == nil {
		t.Fatal("retired key is listed without a revocation timestamp")
	}
	active := listedKey(t, keys, newID)
	if active.RevokedAt != nil {
		t.Fatalf("rotated key is listed as revoked at %q", *active.RevokedAt)
	}
	if err := f.keys.Revoke(ctx, f.owner, newID); err != nil {
		t.Fatalf("revoke rotated key: %v", err)
	}
}

func TestRevokeStopsKeyFromAuthenticating(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	revokedID, revokedPlain, err := f.keys.Issue(ctx, f.owner, "retired")
	if err != nil {
		t.Fatalf("issue revoked key: %v", err)
	}
	keptID, keptPlain, err := f.keys.Issue(ctx, f.owner, "kept")
	if err != nil {
		t.Fatalf("issue surviving key: %v", err)
	}
	if err := f.keys.Revoke(ctx, f.owner, revokedID); err != nil {
		t.Fatalf("revoke key: %v", err)
	}
	if _, err := f.keys.Authenticate(ctx, string(revokedPlain)); err == nil {
		t.Fatal("revoked key still authenticates")
	}
	tenant, err := f.keys.Authenticate(ctx, string(keptPlain))
	if err != nil {
		t.Fatalf("revoking one key stopped another key from authenticating: %v", err)
	}
	if tenant != f.owner {
		t.Fatalf("surviving key authenticated tenant %q, want %q", tenant, f.owner)
	}
	keys, err := f.keys.List(ctx, f.owner)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	revoked := listedKey(t, keys, revokedID)
	if revoked.RevokedAt == nil {
		t.Fatal("revoked key is listed without a revocation timestamp")
	}
	kept := listedKey(t, keys, keptID)
	if kept.RevokedAt != nil {
		t.Fatalf("surviving key is listed as revoked at %q", *kept.RevokedAt)
	}
	if err := f.keys.Revoke(ctx, f.owner, keptID); err != nil {
		t.Fatalf("revoke surviving key: %v", err)
	}
}

func TestKeyManagementIsScopedToOwningTenant(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id, plain, err := f.keys.Issue(ctx, f.owner, "owner-key")
	if err != nil {
		t.Fatalf("issue key: %v", err)
	}
	if err := f.keys.Revoke(ctx, f.other, id); err == nil {
		t.Fatal("another tenant revoked a key it does not own")
	}
	if _, _, err := f.keys.Rotate(ctx, f.other, id, "taken-over"); err == nil {
		t.Fatal("another tenant rotated a key it does not own")
	}
	tenant, err := f.keys.Authenticate(ctx, string(plain))
	if err != nil {
		t.Fatalf("owner key stopped authenticating after another tenant touched it: %v", err)
	}
	if tenant != f.owner {
		t.Fatalf("owner key authenticated tenant %q, want %q", tenant, f.owner)
	}
	keys, err := f.keys.List(ctx, f.owner)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("owner tenant holds %d keys after another tenant attempted rotation, want 1", len(keys))
	}
	if err := f.keys.Revoke(ctx, f.owner, id); err != nil {
		t.Fatalf("revoke owner key: %v", err)
	}
}

func TestIssueEnforcesKeyNameBoundaries(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, name := range []string{"", "   ", strings.Repeat("a", 101)} {
		if _, _, err := f.keys.Issue(ctx, f.owner, name); err == nil {
			t.Fatalf("issue accepted the invalid key name %q", name)
		}
	}
	keys, err := f.keys.List(ctx, f.owner)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("rejected issues created %d keys, want 0", len(keys))
	}
	longest := strings.Repeat("a", 100)
	longestID, _, err := f.keys.Issue(ctx, f.owner, longest)
	if err != nil {
		t.Fatalf("issue rejected a 100 character key name: %v", err)
	}
	trimmedID, _, err := f.keys.Issue(ctx, f.owner, "  deploy  ")
	if err != nil {
		t.Fatalf("issue rejected a padded key name: %v", err)
	}
	keys, err = f.keys.List(ctx, f.owner)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if key := listedKey(t, keys, longestID); key.Name != longest {
		t.Fatalf("listed key name = %q, want %q", key.Name, longest)
	}
	if key := listedKey(t, keys, trimmedID); key.Name != "deploy" {
		t.Fatalf("listed key name = %q, want deploy", key.Name)
	}
	if err := f.keys.Revoke(ctx, f.owner, longestID); err != nil {
		t.Fatalf("revoke longest-name key: %v", err)
	}
	if err := f.keys.Revoke(ctx, f.owner, trimmedID); err != nil {
		t.Fatalf("revoke trimmed-name key: %v", err)
	}
}

func TestRotateRejectedForItsNameLeavesTheOriginalKeyWorking(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id, plain, err := f.keys.Issue(ctx, f.owner, "deploy")
	if err != nil {
		t.Fatalf("issue key: %v", err)
	}
	if _, _, err := f.keys.Rotate(ctx, f.owner, id, ""); err == nil {
		t.Fatal("rotate accepted an empty key name")
	}
	tenant, err := f.keys.Authenticate(ctx, string(plain))
	if err != nil {
		t.Fatalf("original key stopped authenticating after a rejected rotate: %v", err)
	}
	if tenant != f.owner {
		t.Fatalf("original key authenticated tenant %q, want %q", tenant, f.owner)
	}
	keys, err := f.keys.List(ctx, f.owner)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("rejected rotate left %d keys, want 1", len(keys))
	}
	if err := f.keys.Revoke(ctx, f.owner, id); err != nil {
		t.Fatalf("revoke original key: %v", err)
	}
}
