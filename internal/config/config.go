// Package config resolves where wamcp keeps its state on disk.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// EnvDataDir overrides the default data directory.
const EnvDataDir = "WAMCP_DATA_DIR"

// Paths groups every file wamcp reads or writes.
type Paths struct {
	Dir        string // root data directory (created with 0700)
	SessionDB  string // whatsmeow device/session store
	MessagesDB string // local message archive
	MediaDir   string // downloaded media
	LockFile   string // single-instance lock
	TokenFile  string // bearer token for the HTTP transport
}

// Resolve returns the paths for dir, or for the default location when dir is
// empty: $WAMCP_DATA_DIR, otherwise <user config dir>/wamcp.
func Resolve(dir string) (Paths, error) {
	if dir == "" {
		dir = os.Getenv(EnvDataDir)
	}
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return Paths{}, fmt.Errorf("locate user config directory: %w", err)
		}
		dir = filepath.Join(base, "wamcp")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Paths{}, fmt.Errorf("resolve data directory: %w", err)
	}
	return Paths{
		Dir:        abs,
		SessionDB:  filepath.Join(abs, "session.db"),
		MessagesDB: filepath.Join(abs, "messages.db"),
		MediaDir:   filepath.Join(abs, "media"),
		LockFile:   filepath.Join(abs, "wamcp.lock"),
		TokenFile:  filepath.Join(abs, "token"),
	}, nil
}

// Ensure creates the data directory with owner-only permissions.
func (p Paths) Ensure() error {
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	// Tighten permissions if the directory already existed.
	if err := os.Chmod(p.Dir, 0o700); err != nil && !errors.Is(err, os.ErrPermission) { //nolint:gosec // directories need the execute bit
		return fmt.Errorf("restrict data directory permissions: %w", err)
	}
	return nil
}
