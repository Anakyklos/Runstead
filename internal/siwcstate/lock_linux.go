//go:build linux

package siwcstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	LockVersion = 1
	lockFile    = ".siwc-domain-lock-v1"
	lockWait    = 250 * time.Millisecond
	lockPoll    = 10 * time.Millisecond
)

var ErrLockBusy = errors.New("SIWC domain lock is held")

type lockMarker struct {
	Version int    `json:"version"`
	Domain  string `json:"domain_id"`
}

// Lock is an exclusive Linux flock held on the stable, domain-local inode.
// The marker file is intentionally persistent; recovery never unlinks it.
type Lock struct {
	file    *os.File
	created bool
}

// AcquireLock takes the exclusive SIWC domain lock. It creates no parent
// directories, rejects symlinks and weak ownership/modes, and bounds waiting
// to 250 ms. Supported filesystems are an explicit local-filesystem allowlist.
func AcquireLock(ctx context.Context, domain Domain) (*Lock, error) {
	if ctx == nil {
		return nil, fmt.Errorf("SIWC lock requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := existingPrivateDir(domain.Dir)
	if err != nil || dir != domain.Dir {
		return nil, fmt.Errorf("%w: unsafe lock directory", ErrUnsafePath)
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil || !supportedLocalFS(uint64(fs.Type)) {
		return nil, fmt.Errorf("%w: filesystem locking semantics are not supported", ErrUnsafePath)
	}
	path := filepath.Join(dir, lockFile)
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	created := err == nil
	if err == unix.EEXIST {
		fd, err = unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: cannot open domain lock", ErrUnsafePath)
	}
	file := os.NewFile(uintptr(fd), path)
	cleanup := func(err error) (*Lock, error) {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close SIWC domain lock: %w", closeErr))
		}
		return nil, err
	}
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil || opened.Mode&unix.S_IFMT != unix.S_IFREG || opened.Nlink != 1 || opened.Uid != uint32(os.Getuid()) || opened.Mode&0o077 != 0 {
		return cleanup(fmt.Errorf("%w: invalid domain lock file", ErrUnsafePath))
	}
	started := time.Now()
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN && err != unix.EINTR {
			return cleanup(fmt.Errorf("%w: cannot acquire domain lock", ErrUnsafePath))
		}
		if err := ctx.Err(); err != nil {
			return cleanup(err)
		}
		if time.Since(started) >= lockWait {
			return cleanup(ErrLockBusy)
		}
		timer := time.NewTimer(lockPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return cleanup(ctx.Err())
		case <-timer.C:
		}
	}
	// Verify the pathname still names the locked inode after acquisition.
	var named unix.Stat_t
	if err := unix.Lstat(path, &named); err != nil || named.Dev != opened.Dev || named.Ino != opened.Ino || named.Mode&unix.S_IFMT != unix.S_IFREG {
		return cleanup(fmt.Errorf("%w: domain lock path changed", ErrUnsafePath))
	}
	marker := lockMarker{Version: LockVersion, Domain: domain.Identity.ConfigIdentity}
	content, err := json.Marshal(marker)
	if err != nil {
		return cleanup(err)
	}
	if err := validateOrInitializeMarker(file, content); err != nil {
		return cleanup(err)
	}
	if err := file.Sync(); err != nil {
		return cleanup(fmt.Errorf("sync SIWC lock marker: %w", err))
	}
	return &Lock{file: file, created: created}, nil
}

func validateOrInitializeMarker(file *os.File, expected []byte) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("%w: cannot inspect lock marker", ErrUnsafePath)
	}
	if info.Size() == 0 {
		if _, err := file.WriteAt(expected, 0); err != nil {
			return fmt.Errorf("write SIWC lock marker: %w", err)
		}
		return nil
	}
	if info.Size() > 1024 {
		return fmt.Errorf("%w: invalid lock marker", ErrUnsafePath)
	}
	actual := make([]byte, info.Size())
	if _, err := file.ReadAt(actual, 0); err != nil || strings.TrimSpace(string(actual)) != string(expected) {
		return fmt.Errorf("%w: lock marker mismatch", ErrUnsafePath)
	}
	return nil
}

// Release unlocks and closes the descriptor. It never removes the persistent
// marker file, so another process cannot accidentally lock a replacement inode.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	unlockErr := unix.Flock(int(file.Fd()), unix.LOCK_UN)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}

func supportedLocalFS(fsType uint64) bool {
	switch int64(fsType) {
	case 0xEF53: // ext2/3/4
		return true
	case 0x58465342: // XFS
		return true
	case 0x9123683E: // Btrfs
		return true
	case 0x01021994: // tmpfs (used for private local state/test roots)
		return true
	case 0x794c7630: // overlayfs; Linux flock is local to this mount
		return true
	default:
		return false
	}
}
