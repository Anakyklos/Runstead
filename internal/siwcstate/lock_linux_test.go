//go:build linux

package siwcstate

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/state"
)

func TestSIWCLockProcessHelper(t *testing.T) {
	if os.Getenv("RUNSTEAD_SIWC_LOCK_HELPER") != "1" {
		return
	}
	domain := Domain{Dir: os.Getenv("RUNSTEAD_SIWC_LOCK_DIR"), Identity: Manifest{ConfigIdentity: testDomainID}}
	lock, err := AcquireLock(context.Background(), domain)
	if err != nil {
		_, _ = os.Stdout.WriteString("LOCK_ERROR=" + err.Error())
		return
	}
	defer lock.Release()
	if os.Getenv("RUNSTEAD_SIWC_CRASH_PERSIST") == "1" {
		store, err := state.Open(state.Options{Path: filepath.Join(domain.Dir, state.DefaultDBFile)})
		if err != nil {
			_, _ = os.Stdout.WriteString("STORE_ERROR=" + err.Error())
			return
		}
		tx, err := store.DB().BeginTx(context.Background(), nil)
		if err != nil {
			_, _ = os.Stdout.WriteString("TX_ERROR=" + err.Error())
			return
		}
		if _, err := tx.Exec(`UPDATE meta SET value='999' WHERE key='identity_sequence'`); err != nil {
			_, _ = os.Stdout.WriteString("WRITE_ERROR=" + err.Error())
			return
		}
		_ = os.WriteFile(os.Getenv("RUNSTEAD_SIWC_READY"), []byte("transaction-open"), 0o600)
		select {}
	}
	_ = os.WriteFile(os.Getenv("RUNSTEAD_SIWC_READY"), []byte("ready"), 0o600)
	if os.Getenv("RUNSTEAD_SIWC_HOLD") == "1" {
		select {}
	}
}

const testDomainID = "provider.v2:sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestAcquireLockExcludesProcessesAndCrashReleases(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	start := func() *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSIWCLockProcessHelper$")
		cmd.Env = append(os.Environ(),
			"RUNSTEAD_SIWC_LOCK_HELPER=1",
			"RUNSTEAD_SIWC_LOCK_DIR="+dir,
			"RUNSTEAD_SIWC_READY="+ready,
			"RUNSTEAD_SIWC_HOLD=1",
		)
		return cmd
	}
	first := start()
	firstOutput, err := first.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = first.Process.Kill()
		_ = first.Wait()
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = first.Process.Kill()
			_ = first.Wait()
			b, _ := io.ReadAll(firstOutput)
			t.Fatalf("first process did not acquire SIWC lock: %s", b)
		}
		time.Sleep(10 * time.Millisecond)
	}

	competing := exec.Command(os.Args[0], "-test.run=^TestSIWCLockProcessHelper$")
	competing.Env = append(os.Environ(), "RUNSTEAD_SIWC_LOCK_HELPER=1", "RUNSTEAD_SIWC_LOCK_DIR="+dir,
		"RUNSTEAD_SIWC_READY="+filepath.Join(t.TempDir(), "unused"))
	out, err := competing.CombinedOutput()
	if err != nil {
		t.Fatalf("competing process failed: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "LOCK_ERROR=SIWC domain lock is held") {
		t.Fatalf("competing process output = %q, want bounded lock refusal", out)
	}

	// A real process crash releases flock automatically and leaves the stable
	// marker inode in place for the next process.
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = first.Wait()
	lock, err := AcquireLock(context.Background(), Domain{Dir: dir, Identity: Manifest{ConfigIdentity: testDomainID}})
	if err != nil {
		t.Fatalf("reacquire after crashed owner: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, lockFile)); err != nil {
		t.Fatalf("persistent lock marker missing after crash: %v", err)
	}
}

func TestAcquireLockRejectsSymlinkDomainAndTamperedMarker(t *testing.T) {
	real := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(context.Background(), Domain{Dir: alias, Identity: Manifest{ConfigIdentity: testDomainID}}); err == nil {
		t.Fatal("symlink alias unexpectedly acquired a lock")
	}
	if err := os.Chmod(real, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireLock(context.Background(), Domain{Dir: real, Identity: Manifest{ConfigIdentity: testDomainID}})
	if err != nil {
		t.Fatalf("acquire on real domain: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, lockFile), []byte(`{"version":1,"domain_id":"tampered"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(context.Background(), Domain{Dir: real, Identity: Manifest{ConfigIdentity: testDomainID}}); err == nil {
		t.Fatal("tampered lock marker unexpectedly acquired a lock")
	}
}

func TestUnsupportedFilesystemMagicFailsClosed(t *testing.T) {
	if supportedLocalFS(0xdeadbeef) {
		t.Fatal("unknown filesystem magic accepted for SIWC locking")
	}
}

func TestProcessCrashDuringSQLitePersistenceReopensUnderLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(state.Options{Path: filepath.Join(dir, state.DefaultDBFile)})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestSIWCLockProcessHelper$")
	cmd.Env = append(os.Environ(), "RUNSTEAD_SIWC_LOCK_HELPER=1", "RUNSTEAD_SIWC_CRASH_PERSIST=1",
		"RUNSTEAD_SIWC_LOCK_DIR="+dir, "RUNSTEAD_SIWC_READY="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			t.Fatal("child did not enter SQLite persistence section")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()

	lock, err := AcquireLock(context.Background(), Domain{Dir: dir, Identity: Manifest{ConfigIdentity: testDomainID}})
	if err != nil {
		t.Fatalf("reacquire after persistence crash: %v", err)
	}
	defer lock.Release()
	store, err = state.Open(state.Options{Path: filepath.Join(dir, state.DefaultDBFile)})
	if err != nil {
		t.Fatalf("SQLite recovery after process crash: %v", err)
	}
	defer store.Close()
	var sequence string
	if err := store.DB().QueryRow(`SELECT value FROM meta WHERE key='identity_sequence'`).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	if sequence != "0" {
		t.Fatalf("uncommitted persistence escaped crash recovery: identity_sequence=%q", sequence)
	}
}
