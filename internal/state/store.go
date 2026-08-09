package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

// Default file and directory permissions for persisted state. The directory
// is private to the invoking user and the database file is owner-only, since
// it may contain task objectives and sanitized repository content.
const (
	DefaultDirMode  = 0o700
	DefaultFileMode = 0o600
)

// SQLite operational policy (measured in the #8 driver spike):
//
//   - journal_mode = WAL: readers never block the single writer and a crash
//     between commits cannot corrupt the main database file;
//   - synchronous = NORMAL: in WAL mode every committed transaction survives
//     process death (the WAL is replayed on open), while the per-commit fsync
//     is skipped. Power-loss durability would require synchronous=FULL, which
//     Runstead deliberately does not need for a CLI whose realistic failure
//     boundary is process interruption;
//   - busy_timeout = 5000ms: a second process (for example `runstead inspect`
//     during a run) waits a bounded time for the writer instead of failing
//     immediately;
//   - foreign_keys = ON: referential integrity is enforced (no cascades).
//
// The store uses exactly one connection (MaxOpenConns(1)): Runstead is a
// single-writer CLI process, and serializing in-process access removes the
// driver-specific locking corner cases while cross-process behavior is
// governed by WAL and busy_timeout.
const (
	busyTimeout  = 5000
	journalMode  = "WAL"
	syncMode     = "NORMAL"
	maxOpenConns = 1
	sqliteDriver = "sqlite"
	// DefaultDBFile is the database file name inside the state directory.
	DefaultDBFile = "runstead.db"
)

// Clock is the store's time seam so tests stay deterministic.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Options configures one Store.
type Options struct {
	// Path is the SQLite database file. The parent directory is created with
	// DefaultDirMode when it does not exist.
	Path string
	// Clock is optional; time.Now is used when nil.
	Clock Clock
	// DirMode and FileMode are optional overrides for created paths.
	DirMode  os.FileMode
	FileMode os.FileMode
	// BusyTimeout overrides the default 5s busy timeout (a second process
	// waits this long for the writer before failing). Tests shorten it.
	BusyTimeout time.Duration
}

// Store is the narrow persistence boundary of the modular monolith. It owns
// all SQLite access for the runtime: task/action/attempt projections, the
// append-only events journal and the persisted governor protection state.
// It implements agent.Persistence and governor.Persistence.
type Store struct {
	db          *sql.DB
	clock       Clock
	dirMode     os.FileMode
	fileMode    os.FileMode
	busyTimeout time.Duration
}

// crashPoint is a deterministic test seam invoked at named persistence
// boundaries so subprocess crash tests can simulate process death between
// transactions. Production code leaves it nil.
var crashPoint func(string)

// SetCrashPoint installs the crash test seam. Only tests call this.
func SetCrashPoint(fn func(string)) { crashPoint = fn }

func hitCrashPoint(name string) {
	if crashPoint != nil {
		crashPoint(name)
	}
}

// Open creates (or reopens) the database at path, applies embedded
// migrations, configures the SQLite operational policy and returns a ready
// Store. The parent directory is created when missing; failures to create or
// open are returned as explicit errors.
func Open(options Options) (*Store, error) {
	path := filepath.Clean(options.Path)
	if path == "" || path == "." {
		return nil, errors.New("state database path must be a file path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, modeOr(options.DirMode, DefaultDirMode)); err != nil {
		return nil, fmt.Errorf("create state directory %s: %w", dir, err)
	}
	db, err := sql.Open(sqliteDriver, path)
	if err != nil {
		return nil, fmt.Errorf("open state database %s: %w", path, err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	store := &Store{
		db:          db,
		clock:       options.Clock,
		dirMode:     modeOr(options.DirMode, DefaultDirMode),
		fileMode:    modeOr(options.FileMode, DefaultFileMode),
		busyTimeout: options.BusyTimeout,
	}
	if store.clock == nil {
		store.clock = realClock{}
	}
	if store.busyTimeout <= 0 {
		store.busyTimeout = busyTimeout * time.Millisecond
	}
	if err := store.configure(); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate state database %s: %w", path, err)
	}
	if err := store.seedMeta(); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(path, store.fileMode); err != nil {
		db.Close()
		return nil, fmt.Errorf("set state database permissions: %w", err)
	}
	return store, nil
}

func modeOr(value, fallback os.FileMode) os.FileMode {
	if value != 0 {
		return value
	}
	return fallback
}

// configure applies the documented SQLite operational policy on the store's
// single connection.
func (s *Store) configure() error {
	statements := []string{
		fmt.Sprintf("PRAGMA busy_timeout = %d", s.busyTimeout.Milliseconds()),
		fmt.Sprintf("PRAGMA journal_mode = %s", journalMode),
		fmt.Sprintf("PRAGMA synchronous = %s", syncMode),
		"PRAGMA foreign_keys = ON",
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("apply SQLite policy %q: %w", statement, err)
		}
	}
	return nil
}

// seedMeta ensures the identity counter row exists.
func (s *Store) seedMeta() error {
	_, err := s.db.Exec(
		"INSERT OR IGNORE INTO meta (key, value) VALUES ('identity_sequence', '0')")
	if err != nil {
		return fmt.Errorf("seed identity counter: %w", err)
	}
	return nil
}

// Close checkpoints and closes the database. It is safe to call more than
// once.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	db := s.db
	s.db = nil
	var errs []error
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		errs = append(errs, err)
	}
	if err := db.Close(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("close state database: %v", errs[0])
	}
	return nil
}

// SchemaVersion returns the applied schema version (PRAGMA user_version).
func (s *Store) SchemaVersion() (int, error) {
	return schemaVersion(s.db)
}

// nextIdentity returns the next value of the store-owned identity sequence
// and increments the persisted counter inside the caller's transaction. It
// must be called inside an open write transaction.
func nextIdentity(tx *sql.Tx, prefix string) (string, error) {
	var next int64
	if err := tx.QueryRow(
		"UPDATE meta SET value = CAST(value AS INTEGER) + 1 WHERE key = 'identity_sequence' RETURNING value",
	).Scan(&next); err != nil {
		return "", fmt.Errorf("allocate identity: %w", err)
	}
	return fmt.Sprintf("%s-%06d", prefix, next), nil
}

// now formats a timestamp as RFC 3339 UTC text for deterministic storage.
func (s *Store) now() string {
	return formatTime(s.clock.Now())
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

// Persistence is the semantic task-lifecycle boundary the agent loop depends
// on. It deliberately contains no SQL: the loop persists meaningful
// transitions and the store owns the schema. Nil is never passed to these
// methods; the loop treats any returned error as a terminal persistence
// failure.
type Persistence interface {
	// CreateTask persists a new task in status 'planned' and appends the
	// task_created event in the same transaction.
	CreateTask(ctx context.Context, task TaskRecord) error
	// StartTask moves a task to 'running' and appends task_started.
	StartTask(ctx context.Context, taskID string) error
	// RecordAction persists one accepted envelope as a logical action in
	// status 'planned' and returns its action_id. It happens before repeat
	// guard and policy decisions.
	RecordAction(ctx context.Context, record ActionRecord) (string, error)
	// RejectAction marks a planned action as rejected (repeat guard or
	// policy) without creating any tool attempt.
	RejectAction(ctx context.Context, taskID, actionID, reason string) error
	// PrepareToolAttempt persists tool intent (TX 1) and returns the
	// execution_id. It commits before the tool effect starts.
	PrepareToolAttempt(ctx context.Context, record ToolAttemptPrepared) (string, error)
	// CompleteToolAttempt persists the tool result and observation evidence
	// (TX 2) after the tool effect returned.
	CompleteToolAttempt(ctx context.Context, record ToolAttemptCompleted) error
	// RecordWritePolicyDecision persists one typed control-plane decision for
	// a write proposal (issue #10) with its journal event.
	RecordWritePolicyDecision(ctx context.Context, record WritePolicyDecision) error
	// RecordRecipePolicyDecision persists one typed control-plane decision for
	// a run_recipe proposal (issue #26) with its journal event.
	RecordRecipePolicyDecision(ctx context.Context, record RecipePolicyDecision) error
	// MarkTaskApprovalRequired records a control-plane pause: the task stays
	// resumable (status running) with the pending action and a
	// task_approval_required event; no terminal finalize happens.
	MarkTaskApprovalRequired(ctx context.Context, taskID, actionID, reason string) error
	// MarkTaskVerificationPaused records a control-plane verification pause:
	// the task stays durably resumable (status running) with the typed outcome
	// and a task_verification_paused event; no terminal finalize happens
	// (issue #11).
	MarkTaskVerificationPaused(ctx context.Context, taskID, stopReason string) error
	// PendingApprovals returns the write actions of one task still awaiting an
	// operator decision (approval_required policy decision, no approval row).
	PendingApprovals(ctx context.Context, taskID string) ([]PendingApproval, error)
	// FinalizeTask persists the terminal task outcome, summary and evidence.
	FinalizeTask(ctx context.Context, record TaskFinalize) error
	// SaveAcceptancePlan persists the operator acceptance plan of one task
	// with its journal event (issue #11).
	SaveAcceptancePlan(ctx context.Context, taskID string, planJSON []byte, digest string) error
	// AcceptancePlan returns the persisted operator acceptance plan spec JSON
	// and digest. The third result reports whether a plan exists.
	AcceptancePlan(ctx context.Context, taskID string) (specJSON []byte, digest string, ok bool, err error)
	// SaveWorkspaceBaseline persists the bounded real git status/diff observed
	// at task start with its truncation flags (issue #11 review).
	SaveWorkspaceBaseline(ctx context.Context, taskID, gitStatus, gitDiff string, statusTruncated, diffTruncated bool) error
	// WorkspaceBaseline returns the persisted git baseline of one task with its
	// truncation flags.
	WorkspaceBaseline(ctx context.Context, taskID string) (gitStatus, gitDiff string, statusTruncated, diffTruncated bool, ok bool, err error)
	// SaveVerificationAttempt persists one verification attempt and its
	// journal event atomically (issue #11).
	SaveVerificationAttempt(ctx context.Context, record VerificationAttemptRecord) error
	// VerificationAttempts returns the persisted verification attempts of one
	// task with their per-check results.
	VerificationAttempts(ctx context.Context, taskID string) ([]VerificationAttemptRow, error)
	// LatestVerificationDecision returns the decision of the latest
	// verification attempt of one task. The second result reports whether any
	// attempt exists.
	LatestVerificationDecision(ctx context.Context, taskID string) (string, bool, error)
	// LoadRecoverySnapshot loads the full persisted history one resume or one
	// verification needs. It returns ErrTaskNotFound when the task row is
	// absent.
	LoadRecoverySnapshot(ctx context.Context, taskID string) (*RecoverySnapshot, error)
}

// TaskRecord is the durable task root.
type TaskRecord struct {
	TaskID    string
	Objective string
	Workspace string
	Model     string
	// ConfigJSON is a sanitized JSON snapshot of the meaningful execution
	// configuration, excluding secrets.
	ConfigJSON []byte
}

// TaskFinalize is the terminal loop outcome of one task.
type TaskFinalize struct {
	TaskID         string
	Outcome        string
	StopReason     string
	Summary        string
	Classification string
	Evidence       []string
	Turns          int
	Attempts       int
	Observations   int
	Corrections    int
	Repeated       int
	MixedProse     int
}

// ActionRecord is one accepted logical action envelope.
type ActionRecord struct {
	TaskID string
	Tool   string
	// Arguments is the sanitized canonical JSON of the accepted arguments.
	Arguments []byte
	// Fingerprint is repeat/loop evidence only.
	Fingerprint string
	// RecipeFingerprint is the digest-bound approval identity of a run_recipe
	// proposal (issue #26 review): it binds the recipe id to the effective
	// definition digest, so an operator approval for one definition can never
	// authorize a different definition of the same id. Empty for non-recipe
	// actions, which use Fingerprint for approval identity.
	RecipeFingerprint string
	// WorkspaceSignature is the workspace state marker recorded when the
	// action was accepted. It is repeat/loop evidence only (issue #9): resume
	// seeds the repeat guard with it so an identical proposal is rejected only
	// while the workspace signature is unchanged. It is never an idempotency
	// key and never authorizes reusing an old observation.
	WorkspaceSignature string
}

// ToolAttemptPrepared is the TX 1 intent for one concrete tool execution.
type ToolAttemptPrepared struct {
	TaskID        string
	ActionID      string
	Tool          string
	Arguments     []byte
	RecoveryClass int
	// EffectAfterHash is the deterministic expected after-state hash of a
	// write effect, computed from the real arguments before TX 1 commits
	// (issue #10). It is empty for read-only attempts and is used by recovery
	// to reconcile an interrupted write against the current filesystem state.
	EffectAfterHash string
	// PlannedEffect is the bounded, sanitized planned diff of a write effect
	// (issue #10 review). It is evidence of intent only: recovery promotes it
	// to reconciled completed evidence only when the current filesystem state
	// matches EffectAfterHash. Empty for read-only attempts.
	PlannedEffect tools.PlannedEffect
	// ProcessIntent is the bounded, sanitized process intent of a run_recipe
	// attempt (issue #26): resolved recipe, argv, capabilities and the
	// control-plane policy decision. It is evidence of intent only; a
	// prepared process attempt left by a crash is recovery class 4 and is
	// never blindly re-run. Empty for non-process attempts.
	ProcessIntent []byte
}

// ToolAttemptCompleted is the TX 2 result of one concrete tool execution.
type ToolAttemptCompleted struct {
	TaskID      string
	ExecutionID string
	Status      string
	// Classification is the typed failure classification for failed attempts
	// (for example a tools.FailureCode); empty for successful attempts.
	Classification string
	EvidenceID     string
	DurationNanos  int64
	// Observation is the tool observation captured by the registry. The
	// store marshals, redacts and persists its data and metadata.
	Observation tools.Observation
}

// Renderer is the inspect contract: a Store renders one task for humans.
type Renderer interface {
	RenderInspect(ctx context.Context, out io.Writer, taskID string) error
}

var _ Persistence = (*Store)(nil)
var _ governor.Persistence = (*Store)(nil)
