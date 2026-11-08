package architecture

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCreditBalanceWritesBelongToLedger(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(current), ".."))
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && filepath.Base(path) == "ledger" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, statement := range []string{"INSERT INTO credit_entries", "UPDATE credit_accounts SET balance_micros"} {
			if strings.Contains(string(contents), statement) {
				t.Errorf("%s owns ledger write %q; credit accounting must have one owner", path, statement)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTransportAndCompositionRootDoNotOwnPersistence(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	backend := filepath.Clean(filepath.Join(filepath.Dir(current), "..", ".."))
	for _, dir := range []string{filepath.Join(backend, "internal", "httpapi"), filepath.Join(backend, "cmd")} {
		err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			contents, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, forbidden := range []string{"pgxpool", "SELECT ", "INSERT INTO ", "UPDATE ", "DELETE FROM "} {
				if strings.Contains(string(contents), forbidden) {
					t.Errorf("%s contains persistence implementation %q", path, forbidden)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestDomainContextsDoNotImportPeerInfrastructure(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(current), ".."))
	for _, packageName := range []string{"identity", "tenant", "apikey", "ledger", "payments", "compute", "dstack"} {
		err := filepath.WalkDir(filepath.Join(root, packageName), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			contents, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, forbidden := range []string{"internal/httpapi", "internal/cmd", "github.com/gin-gonic/gin"} {
				if strings.Contains(string(contents), forbidden) {
					t.Errorf("context package %s imports outer adapter %q", path, forbidden)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestPricePersistenceBelongsToPricing(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(current), ".."))
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && filepath.Base(path) == "pricing" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(contents), "dstack_instance_prices") {
			t.Errorf("%s accesses the pricing context's table", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
