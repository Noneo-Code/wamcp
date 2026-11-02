package wa

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilePolicy(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	shared := filepath.Join(base, "shared")
	for _, d := range []string{dataDir, shared} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p string) string {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	session := write(filepath.Join(dataDir, "session.db"))
	doc := write(filepath.Join(shared, "devis.pdf"))
	outside := write(filepath.Join(base, "secret.txt"))
	link := filepath.Join(shared, "innocent.pdf")
	if err := os.Symlink(session, link); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(base, "data-backup")
	_ = os.MkdirAll(sibling, 0o700)
	siblingFile := write(filepath.Join(sibling, "a.txt"))

	// On case-insensitive file systems (macOS, Windows) another spelling of the
	// data directory designates the same files.
	upperSession := filepath.Join(base, "DATA", "session.db")
	caseInsensitive := false
	if _, err := os.Stat(upperSession); err == nil {
		caseInsensitive = true
	}

	open := FilePolicy{Protected: dataDir}
	rooted := FilePolicy{Protected: dataDir, Root: shared}
	tests := []struct {
		name    string
		policy  FilePolicy
		path    string
		wantErr string
	}{
		{"normal file", open, doc, ""},
		{"relative path", open, "devis.pdf", "absolute"},
		{"session keys", open, session, "data directory"},
		{"symlink to session keys", open, link, "data directory"},
		{"dot-dot into data dir", open, filepath.Join(shared, "..", "data", "session.db"), "data directory"},
		{"sibling with common prefix", open, siblingFile, ""},
		{"inside send root", rooted, doc, ""},
		{"outside send root", rooted, outside, "only files under"},
		{"missing file", open, filepath.Join(base, "nope"), "file:"},
	}
	if caseInsensitive {
		tests = append(tests, struct {
			name    string
			policy  FilePolicy
			path    string
			wantErr string
		}{"other case spelling", open, upperSession, "data directory"})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.policy.check(tt.path)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
