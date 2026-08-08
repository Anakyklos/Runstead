package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/RenyEnnos/Runstead/internal/protocol"
)

// signature limits keep the workspace probe bounded for the loop guard.
const (
	signatureMaxEntries     = 1024
	signatureContentMaxRead = 4 << 10
)

// repeatGuard is a workspace-aware loop guard, not an idempotency key store.
// An identical action is rejected only while the workspace signature recorded
// for its fingerprint is unchanged. After an external workspace change the same
// action may run again; the new signature replaces the old one.
type repeatGuard struct {
	mu   sync.Mutex
	seen map[string]string
}

func newRepeatGuard() *repeatGuard {
	return &repeatGuard{seen: make(map[string]string)}
}

// repeat reports whether the action repeats a recorded fingerprint under the
// same workspace signature. A signature must be provided only when the caller
// already computed it; an empty signature never matches a recorded one.
func (g *repeatGuard) repeat(action protocol.Action, signature string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	recorded, ok := g.seen[protocol.ActionFingerprint(action)]
	return ok && recorded == signature && signature != ""
}

// record associates a fingerprint with the workspace signature at execution.
func (g *repeatGuard) record(action protocol.Action, signature string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.seen == nil {
		g.seen = make(map[string]string)
	}
	g.seen[protocol.ActionFingerprint(action)] = signature
}

// seed restores the guard from persisted repeat/loop evidence (issue #9). Only
// fingerprints of actions that were actually executed are seeded, each with
// the workspace signature recorded when the action was accepted. An identical
// proposal is therefore rejected only while the workspace signature is
// unchanged; after an external workspace change the same action may run again
// and produce fresh evidence.
func (g *repeatGuard) seed(fingerprint, signature string) {
	if fingerprint == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.seen == nil {
		g.seen = make(map[string]string)
	}
	g.seen[fingerprint] = signature
}

// workspaceSignature is a bounded, deterministic marker of workspace state used
// only as a loop-guard input. It hashes sorted relative paths with type, size,
// modification time and small-file content, skipping the git directory.
func workspaceSignature(ctx context.Context, root string) (string, error) {
	var entries []signatureEntry
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == ".git" && path != root {
			return filepath.SkipDir
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		item := signatureEntry{
			path:    filepath.ToSlash(relative),
			isDir:   entry.IsDir(),
			size:    info.Size(),
			modTime: info.ModTime().UnixNano(),
		}
		if !entry.IsDir() && info.Mode().IsRegular() && info.Size() <= signatureContentMaxRead && info.Size() >= 0 {
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			item.content = string(content)
		}
		entries = append(entries, item)
		if len(entries) >= signatureMaxEntries {
			return errSignatureLimit
		}
		return nil
	})
	if err != nil && !errors.Is(err, errSignatureLimit) {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	encoded, err := json.Marshal(entries)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

var errSignatureLimit = errors.New("workspace signature entry limit reached")

type signatureEntry struct {
	path    string
	isDir   bool
	size    int64
	modTime int64
	content string
}

func (s signatureEntry) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Path    string `json:"path"`
		Dir     bool   `json:"dir"`
		Size    int64  `json:"size"`
		ModTime int64  `json:"mtime"`
		Content string `json:"content,omitempty"`
	}{
		Path:    s.path,
		Dir:     s.isDir,
		Size:    s.size,
		ModTime: s.modTime,
		Content: s.content,
	})
}
