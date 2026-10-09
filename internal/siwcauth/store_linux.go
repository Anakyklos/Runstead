//go:build linux

package siwcauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	storeVersion   = 1
	maxRecordBytes = 1 << 20
)

func validDiskRecord(disk diskRecord) bool {
	r := disk.Registration
	if disk.Version != storeVersion || r.Issuer != Issuer || strings.TrimSpace(r.Subject) == "" || strings.TrimSpace(r.ClientID) == "" || !validHostID(r.HostID) || r.ExpiresAt.IsZero() {
		return false
	}
	scopes, err := requireScopes(strings.Join(r.Scopes, " "))
	if err != nil || !sameScopes(scopes, r.Scopes) {
		return false
	}
	empty := disk.Tokens.AccessToken == "" && disk.Tokens.RefreshToken == "" && disk.Tokens.IDToken == "" && disk.Tokens.TokenType == ""
	complete := disk.Tokens.AccessToken != "" && disk.Tokens.RefreshToken != "" && disk.Tokens.IDToken != "" && disk.Tokens.TokenType == "Bearer"
	return empty || complete
}

// OpenStore creates only the credential-store path. State-domain discovery
// remains read-only and separate; callers must never derive an SQLite path
// from this directory.
func OpenStore(path string, create bool) (*Store, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("SIWC credential path must be absolute")
	}
	path = filepath.Clean(path)
	if create {
		if err := makePrivatePath(path); err != nil {
			return nil, err
		}
	}
	if err := checkPrivateDir(path); err != nil {
		return nil, err
	}
	regs := filepath.Join(path, "registrations")
	if create {
		if err := os.Mkdir(regs, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, errors.New("cannot create SIWC registration directory")
		}
	}
	if err := checkPrivateDir(regs); err != nil {
		return nil, err
	}
	return &Store{root: path}, nil
}

func makePrivatePath(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("SIWC credential path must be absolute")
	}
	_, initialErr := os.Lstat(path)
	createdFinal := errors.Is(initialErr, os.ErrNotExist)
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(path), current), current) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return errors.New("cannot create protected SIWC credential directory")
			}
			info, err = os.Lstat(current)
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("unsafe SIWC credential path")
		}
	}
	if createdFinal {
		if err := os.Chmod(path, 0o700); err != nil {
			return errors.New("cannot protect SIWC credential directory")
		}
	}
	return nil
}

func checkPrivateDir(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("unsafe SIWC credential directory")
	}
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe SIWC credential path")
		}
		if current == "/" {
			break
		}
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("SIWC credential directory must be private mode 0700")
	}
	if !owned(info) {
		return errors.New("SIWC credential directory has unexpected owner")
	}
	return nil
}

func owned(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

func (s *Store) withLock(fn func() error) error {
	path := filepath.Join(s.root, ".custody-lock-v1")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return errors.New("cannot open SIWC credential lock")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !owned(info) {
		return errors.New("unsafe SIWC credential lock")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Nlink != 1 {
		return errors.New("unsafe SIWC credential lock identity")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return errors.New("cannot lock SIWC credentials")
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func (s *Store) hostIDLocked(create bool) (string, error) {
	path := filepath.Join(s.root, "host-id")
	data, err := readPrivateFile(path, 256)
	if err == nil {
		id := strings.TrimSpace(string(data))
		if !validHostID(id) {
			return "", errors.New("invalid SIWC host ID")
		}
		return id, nil
	}
	if !errors.Is(err, os.ErrNotExist) || !create {
		return "", errors.New("SIWC host ID unavailable")
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("cannot create SIWC host ID")
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	id := fmt.Sprintf("urn:uuid:%s-%s-%s-%s-%s", h[:8], h[8:12], h[12:16], h[16:20], h[20:])
	if err := atomicWrite(path, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) HostID(create bool) (string, error) {
	var id string
	err := s.withLock(func() error { var e error; id, e = s.hostIDLocked(create); return e })
	return id, err
}

func (s *Store) bindingKeyLocked(create bool) ([]byte, error) {
	path := filepath.Join(s.root, "binding.key")
	key, err := readPrivateFile(path, 64)
	if err == nil {
		if len(key) != 32 {
			return nil, errors.New("invalid SIWC binding key")
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) || !create {
		return nil, errors.New("SIWC binding key unavailable")
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, errors.New("cannot create SIWC binding key")
	}
	if err := atomicWrite(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func (s *Store) Bindings(reg PublicRegistration, create bool) (string, string, error) {
	var a, c string
	err := s.withLock(func() error {
		key, err := s.bindingKeyLocked(create)
		if err != nil {
			return err
		}
		a, c, err = Bindings(key, reg)
		return err
	})
	return a, c, err
}

func registrationID(r PublicRegistration) string {
	sum := sha256.Sum256([]byte(r.Issuer + "\x00" + r.Subject + "\x00" + r.ClientID))
	return hex.EncodeToString(sum[:])
}

func (s *Store) Save(reg Registration) error {
	if reg.Issuer != Issuer || reg.Subject == "" || reg.ClientID == "" || !validHostID(reg.HostID) || reg.tokens.AccessToken == "" || reg.tokens.RefreshToken == "" || reg.tokens.IDToken == "" || reg.tokens.TokenType != "Bearer" {
		return ErrInvalidToken
	}
	if _, err := requireScopes(strings.Join(reg.Scopes, " ")); err != nil {
		return err
	}
	return s.withLock(func() error {
		key, err := s.bindingKeyLocked(true)
		if err != nil {
			return err
		}
		_ = key
		if _, err := s.hostIDLocked(true); err != nil {
			return err
		}
		if reg.HostID != mustHostID(s) {
			return errors.New("SIWC registration host mismatch")
		}
		var existing diskRecord
		err = s.findRecord(reg.ClientID, &existing)
		if err == nil {
			if existing.Registration.Issuer != reg.Issuer || existing.Registration.Subject != reg.Subject || existing.Registration.HostID != reg.HostID {
				return ErrIdentityChanged
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := s.writeRecord(reg, false); err != nil {
			return err
		}
		marker := s.refreshMarker(reg.Public())
		markerData, err := readPrivateFile(marker, 256)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil || strings.TrimSpace(string(markerData)) != registrationID(reg.Public()) {
			return ErrRefreshUncertain
		}
		if err := os.Remove(marker); err != nil {
			return ErrRefreshUncertain
		}
		if err := syncDirectory(s.root); err != nil {
			_ = atomicWrite(marker, []byte(registrationID(reg.Public())+"\n"), 0o600)
			return ErrRefreshUncertain
		}
		return nil
	})
}

func mustHostID(s *Store) string { id, _ := s.hostIDLocked(false); return id }

func (s *Store) writeRecord(reg Registration, pending bool) error {
	disk := diskRecord{Version: storeVersion, Registration: reg.Public(), Tokens: reg.tokens, RefreshPending: pending}
	b, err := json.Marshal(disk)
	if err != nil {
		return errors.New("cannot encode SIWC credential")
	}
	return atomicWrite(filepath.Join(s.root, "registrations", registrationID(reg.Public())+".json"), b, 0o600)
}

func readPrivateFile(path string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !owned(info) {
		return nil, errors.New("unsafe SIWC credential file")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Nlink != 1 {
		return nil, errors.New("unsafe SIWC credential file identity")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(b)) > max {
		return nil, errors.New("SIWC credential file exceeds limit")
	}
	return b, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := checkPrivateDir(dir); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		if _, err := readPrivateFile(path, maxRecordBytes); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot inspect SIWC credential target")
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return errors.New("cannot prepare atomic credential write")
	}
	tmp := filepath.Join(dir, ".tmp-"+hex.EncodeToString(random[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return errors.New("cannot create atomic SIWC credential file")
	}
	good := false
	defer func() {
		f.Close()
		if !good {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return errors.New("cannot write SIWC credential file")
	}
	if err = f.Sync(); err != nil {
		return errors.New("cannot sync SIWC credential file")
	}
	if err = f.Close(); err != nil {
		return errors.New("cannot close SIWC credential file")
	}
	if err = os.Rename(tmp, path); err != nil {
		return errors.New("cannot commit SIWC credential file")
	}
	d, err := os.Open(dir)
	if err != nil {
		return errors.New("cannot open SIWC credential directory")
	}
	defer d.Close()
	if err = d.Sync(); err != nil {
		return errors.New("cannot sync SIWC credential directory")
	}
	good = true
	return nil
}

func (s *Store) Load(clientID string) (Registration, error) {
	var result Registration
	err := s.withLock(func() error {
		var disk diskRecord
		if err := s.findRecord(clientID, &disk); err != nil {
			return err
		}
		if disk.RefreshPending || s.refreshPending(disk.Registration) {
			return ErrRefreshUncertain
		}
		if disk.Tokens.AccessToken == "" || disk.Tokens.RefreshToken == "" || disk.Tokens.IDToken == "" {
			return errors.New("SIWC registration is signed out")
		}
		result = Registration{Issuer: disk.Registration.Issuer, Subject: disk.Registration.Subject, ClientID: disk.Registration.ClientID, HostID: disk.Registration.HostID, Email: disk.Registration.Email, Scopes: append([]string(nil), disk.Registration.Scopes...), ExpiresAt: disk.Registration.ExpiresAt, Model: disk.Registration.Model, tokens: disk.Tokens}
		return nil
	})
	return result, err
}

func (s *Store) refreshMarker(reg PublicRegistration) string {
	return filepath.Join(s.root, "rotation-"+registrationID(reg)+".pending")
}

func (s *Store) refreshPending(reg PublicRegistration) bool {
	path := s.refreshMarker(reg)
	b, err := readPrivateFile(path, 256)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	return err != nil || strings.TrimSpace(string(b)) != registrationID(reg)
}

func (s *Store) SetModel(clientID, slug string, visible []Model) error {
	if strings.TrimSpace(slug) == "" {
		return errors.New("an exact model slug is required")
	}
	return s.withLock(func() error {
		var disk diskRecord
		if err := s.findRecord(clientID, &disk); err != nil {
			return err
		}
		if disk.RefreshPending || s.refreshPending(disk.Registration) {
			return ErrRefreshUncertain
		}
		listed := false
		for _, model := range visible {
			if model.Slug == slug {
				listed = true
				break
			}
		}
		if !listed {
			return errors.New("selected model is absent from the account catalog")
		}
		disk.Registration.Model = slug
		b, err := json.Marshal(disk)
		if err != nil {
			return errors.New("cannot encode SIWC model selection")
		}
		return atomicWrite(filepath.Join(s.root, "registrations", registrationID(disk.Registration)+".json"), b, 0o600)
	})
}

func (s *Store) Registrations() ([]PublicRegistration, error) {
	var result []PublicRegistration
	err := s.withLock(func() error {
		entries, err := os.ReadDir(filepath.Join(s.root, "registrations"))
		if err != nil {
			return errors.New("SIWC registrations unavailable")
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				return errors.New("unexpected SIWC credential entry")
			}
			b, err := readPrivateFile(filepath.Join(s.root, "registrations", entry.Name()), maxRecordBytes)
			if err != nil {
				return err
			}
			if rejectDuplicateJSONKeys(b) != nil {
				return errors.New("corrupt SIWC credential record")
			}
			var disk diskRecord
			dec := json.NewDecoder(strings.NewReader(string(b)))
			dec.DisallowUnknownFields()
			if dec.Decode(&disk) != nil || dec.Decode(new(any)) != io.EOF || !validDiskRecord(disk) || entry.Name() != registrationID(disk.Registration)+".json" {
				return errors.New("corrupt SIWC credential record")
			}
			result = append(result, disk.Registration)
		}
		return nil
	})
	return result, err
}

func (s *Store) findRecord(clientID string, dst *diskRecord) error {
	entries, err := os.ReadDir(filepath.Join(s.root, "registrations"))
	if err != nil {
		return errors.New("SIWC registrations unavailable")
	}
	found := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return errors.New("unexpected SIWC credential entry")
		}
		b, err := readPrivateFile(filepath.Join(s.root, "registrations", entry.Name()), maxRecordBytes)
		if err != nil {
			return err
		}
		if rejectDuplicateJSONKeys(b) != nil {
			return errors.New("corrupt SIWC credential record")
		}
		var disk diskRecord
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if dec.Decode(&disk) != nil || dec.Decode(new(any)) != io.EOF || !validDiskRecord(disk) || entry.Name() != registrationID(disk.Registration)+".json" {
			return errors.New("corrupt SIWC credential record")
		}
		if disk.Registration.ClientID == clientID {
			if found {
				return errors.New("ambiguous SIWC credential registration")
			}
			*dst = disk
			found = true
		}
	}
	if !found {
		return os.ErrNotExist
	}
	return nil
}

func (s *Store) VerifyBindings(account, credential string) error {
	return s.withLock(func() error {
		entries, err := os.ReadDir(filepath.Join(s.root, "registrations"))
		if err != nil {
			return errors.New("SIWC registrations unavailable")
		}
		key, err := s.bindingKeyLocked(false)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				return errors.New("unexpected SIWC credential entry")
			}
			b, err := readPrivateFile(filepath.Join(s.root, "registrations", entry.Name()), maxRecordBytes)
			if err != nil {
				return err
			}
			if rejectDuplicateJSONKeys(b) != nil {
				return errors.New("corrupt SIWC credential record")
			}
			var disk diskRecord
			dec := json.NewDecoder(strings.NewReader(string(b)))
			dec.DisallowUnknownFields()
			if dec.Decode(&disk) != nil || dec.Decode(new(any)) != io.EOF || !validDiskRecord(disk) || entry.Name() != registrationID(disk.Registration)+".json" {
				return errors.New("corrupt SIWC credential record")
			}
			a, c, err := Bindings(key, disk.Registration)
			if err != nil {
				return err
			}
			if hmacEqual(a, account) && hmacEqual(c, credential) {
				return nil
			}
		}
		return errors.New("SIWC provider binding has no matching authenticated credential")
	})
}

func hmacEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Refresh persists an intent marker before sending the rotating credential.
// A crash after that point is deliberately non-retryable: the server may have
// consumed the old token even if the response was lost.
func (s *Store) refresh(clientID string, refreshBefore time.Time, request func(Registration) (Registration, error)) (Registration, error) {
	var updated Registration
	err := s.withLock(func() error {
		var disk diskRecord
		if err := s.findRecord(clientID, &disk); err != nil {
			return err
		}
		if disk.RefreshPending || s.refreshPending(disk.Registration) {
			return ErrRefreshUncertain
		}
		current := Registration{Issuer: disk.Registration.Issuer, Subject: disk.Registration.Subject, ClientID: disk.Registration.ClientID, HostID: disk.Registration.HostID, Email: disk.Registration.Email, Scopes: append([]string(nil), disk.Registration.Scopes...), ExpiresAt: disk.Registration.ExpiresAt, Model: disk.Registration.Model, tokens: disk.Tokens}
		if disk.Tokens.RefreshToken == "" || disk.Tokens.AccessToken == "" {
			return errors.New("SIWC renewable session unavailable")
		}
		if current.ExpiresAt.After(refreshBefore) {
			updated = current
			return nil
		}
		disk.RefreshPending = true
		intent, err := json.Marshal(disk)
		if err != nil {
			return errors.New("cannot record SIWC refresh intent")
		}
		if err = atomicWrite(filepath.Join(s.root, "registrations", registrationID(disk.Registration)+".json"), intent, 0o600); err != nil {
			return err
		}
		if err = atomicWrite(s.refreshMarker(disk.Registration), []byte(registrationID(disk.Registration)+"\n"), 0o600); err != nil {
			return ErrRefreshUncertain
		}
		candidate, err := request(current)
		if err != nil {
			return errors.New("SIWC refresh failed; sign in again")
		}
		if candidate.Issuer != current.Issuer || candidate.Subject != current.Subject || candidate.ClientID != current.ClientID || candidate.HostID != current.HostID {
			return ErrIdentityChanged
		}
		if candidate.tokens.AccessToken == "" || candidate.tokens.RefreshToken == "" || candidate.tokens.TokenType != "Bearer" || candidate.ExpiresAt.IsZero() {
			return ErrInvalidToken
		}
		if _, err := requireScopes(strings.Join(candidate.Scopes, " ")); err != nil {
			return err
		}
		if candidate.tokens.IDToken == "" {
			candidate.tokens.IDToken = current.tokens.IDToken
		}
		if err := s.writeRecord(candidate, false); err != nil {
			return errors.New("SIWC refresh rotation could not be committed; sign in again")
		}
		marker := s.refreshMarker(disk.Registration)
		if err := os.Remove(marker); err != nil {
			return ErrRefreshUncertain
		}
		if err := syncDirectory(s.root); err != nil {
			_ = atomicWrite(marker, []byte(registrationID(disk.Registration)+"\n"), 0o600)
			return ErrRefreshUncertain
		}
		updated = candidate
		return nil
	})
	return updated, err
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Store) RemoveTokens(clientID string) error {
	return s.withLock(func() error {
		var disk diskRecord
		if err := s.findRecord(clientID, &disk); err != nil {
			return err
		}
		if disk.RefreshPending || s.refreshPending(disk.Registration) {
			return ErrRefreshUncertain
		}
		return s.removeTokensLocked(disk)
	})
}

func (s *Store) removeTokensLocked(disk diskRecord) error {
	disk.Tokens = TokenSet{}
	disk.RefreshPending = false
	b, err := json.Marshal(disk)
	if err != nil {
		return errors.New("cannot encode signed-out SIWC registration")
	}
	return atomicWrite(filepath.Join(s.root, "registrations", registrationID(disk.Registration)+".json"), b, 0o600)
}
