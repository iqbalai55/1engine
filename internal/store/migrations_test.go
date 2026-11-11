package store

import (
	"io/fs"
	"strconv"
	"strings"
	"testing"
)

// The migration runner keys schema_migrations by the numeric filename prefix, so two
// files sharing a prefix make one of them look already applied and silently skip it.
func TestMigrationVersionsAreUnique(t *testing.T) {
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[int64]string, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			t.Fatalf("migration %q has no version prefix", name)
		}
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			t.Fatalf("migration %q has a non-numeric version prefix", name)
		}
		if other, exists := seen[version]; exists {
			t.Fatalf("migrations %q and %q share version %d", other, name, version)
		}
		seen[version] = name
	}
	if len(seen) == 0 {
		t.Fatal("no migrations are embedded")
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		if _, err := fs.Stat(migrationFiles, "migrations/"+entry.Name()); err != nil {
			t.Fatalf("embedded migration %q is unreadable: %v", entry.Name(), err)
		}
	}
}
