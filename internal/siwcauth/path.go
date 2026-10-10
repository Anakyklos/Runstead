package siwcauth

import (
	"errors"
	"path/filepath"
)

type Store struct{ root string }

type diskRecord struct {
	Version        int                `json:"version"`
	Registration   PublicRegistration `json:"registration"`
	Tokens         TokenSet           `json:"tokens"`
	RefreshPending bool               `json:"refresh_pending"`
}

// DefaultStorePath resolves the protected SIWC custody directory. An explicit
// XDG_CONFIG_HOME must be absolute; silently falling back to HOME would split
// one operator's credentials across two roots when environments diverge.
func DefaultStorePath(home, xdgConfigHome string) (string, error) {
	if !filepath.IsAbs(home) {
		return "", errors.New("SIWC home directory must be absolute")
	}
	if xdgConfigHome != "" {
		if !filepath.IsAbs(xdgConfigHome) || filepath.Clean(xdgConfigHome) != xdgConfigHome {
			return "", errors.New("SIWC XDG config directory must be a normalized absolute path")
		}
		return filepath.Join(xdgConfigHome, "runstead", "siwc"), nil
	}
	return filepath.Join(home, ".config", "runstead", "siwc"), nil
}
