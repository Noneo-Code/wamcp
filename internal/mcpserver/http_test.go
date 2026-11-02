package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestRequireBearer(t *testing.T) {
	h := RequireBearer("s3cret", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for header, want := range map[string]int{
		"":              http.StatusUnauthorized,
		"Bearer wrong":  http.StatusUnauthorized,
		"s3cret":        http.StatusUnauthorized,
		"Bearer s3cret": http.StatusNoContent,
	} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Authorization %q: status %d, want %d", header, rec.Code, want)
		}
	}
}

func TestLoadOrCreateToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	first, err := LoadOrCreateToken(path)
	if err != nil || len(first) != 64 {
		t.Fatalf("token %q, err %v", first, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %v", info.Mode().Perm())
	}
	second, _ := LoadOrCreateToken(path)
	if second != first {
		t.Error("token changed between calls")
	}
}

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8765": true, "localhost:8765": true, "[::1]:8765": true,
		"0.0.0.0:8765": false, ":8765": false, "192.168.1.10:8765": false, "nonsense": false,
	} {
		if got := IsLoopback(addr); got != want {
			t.Errorf("IsLoopback(%q) = %v", addr, got)
		}
	}
}
