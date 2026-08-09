// Package recipe implements the control-plane recipe model for bounded local
// process execution (issue #26).
//
// A recipe is an operator-declared, typed description of one allowed local
// process: a stable ID, an executable, a fixed argv, a working directory
// inside the selected workspace, a timeout, output limits, declared
// capabilities and an environment allowlist. The model can only select a
// recipe by ID (`run_recipe` with {"recipe":"<id>"}); it never supplies argv,
// capabilities or environment. There is no generic shell and no
// model-controlled command string.
//
// The runner executes argv directly (no `sh -c`), starts the process in its
// own process group and terminates the full group on timeout or cancellation.
// Child environment is built from an explicit allowlist; provider
// credential-shaped variables are never inherited, even when listed.
package recipe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Capability is a declared effect of a recipe. It is a description of what
// the recipe may do, never an authorization by itself; the control-plane
// policy decides whether the declared set is allowed, denied or requires
// operator approval.
type Capability string

const (
	// CapabilityReadWorkspace reads files inside the selected workspace.
	CapabilityReadWorkspace Capability = "read_workspace"
	// CapabilityWriteWorkspace writes files inside the selected workspace.
	CapabilityWriteWorkspace Capability = "write_workspace"
	// CapabilityTemporaryFiles creates or reads temporary/cache files outside
	// the workspace (for example build caches).
	CapabilityTemporaryFiles Capability = "temporary_files"
	// CapabilityExecuteRepoCode executes code from the repository.
	CapabilityExecuteRepoCode Capability = "execute_repository_code"
	// CapabilityNetwork performs network access. The native runner does not
	// enforce network isolation; this is declared honestly as unenforced in
	// the evidence.
	CapabilityNetwork Capability = "network"
	// CapabilityGitMetadata reads or writes Git metadata (index, objects,
	// refs) of the workspace repository.
	CapabilityGitMetadata Capability = "git_metadata"
	// CapabilityInheritEnvironment inherits the environment variables listed
	// in Recipe.AllowedEnvironment from the parent process (subject to the
	// credential denylist).
	CapabilityInheritEnvironment Capability = "inherit_environment"
)

// AllCapabilities is the deterministic sorted list of recognized
// capabilities, used for validation and rendering.
var AllCapabilities = []Capability{
	CapabilityReadWorkspace,
	CapabilityWriteWorkspace,
	CapabilityTemporaryFiles,
	CapabilityExecuteRepoCode,
	CapabilityNetwork,
	CapabilityGitMetadata,
	CapabilityInheritEnvironment,
}

// ParseCapability validates a capability name strictly.
func ParseCapability(value string) (Capability, error) {
	for _, capability := range AllCapabilities {
		if string(capability) == value {
			return capability, nil
		}
	}
	return "", fmt.Errorf("unknown capability %q", value)
}

// OutputLimits bounds the captured stdout and stderr independently. Zero
// fields receive the package defaults at validation time.
type OutputLimits struct {
	MaxStdoutBytes int `json:"max_stdout_bytes,omitempty"`
	MaxStderrBytes int `json:"max_stderr_bytes,omitempty"`
}

// Recipe is one operator-declared local process.
type Recipe struct {
	// ID is the stable identifier the model uses to select the recipe.
	ID string `json:"id"`
	// Executable is the program to execute. It may be a name resolved via
	// PATH or an absolute path; it is never model-controlled.
	Executable string `json:"executable"`
	// Argv is the fixed argument vector. It is operator-controlled and never
	// model-controlled; shell metacharacters inside arguments stay literal.
	Argv []string `json:"argv,omitempty"`
	// WorkingDirectory is a relative directory inside the selected workspace.
	// Empty means the workspace root. Absolute paths, traversal and symlink
	// escapes are rejected.
	WorkingDirectory string `json:"working_directory,omitempty"`
	// TimeoutNanos bounds the whole process run (including children).
	TimeoutNanos int64 `json:"timeout_nanos,omitempty"`
	// OutputLimits bounds stdout and stderr independently.
	OutputLimits OutputLimits `json:"output_limits,omitempty"`
	// Capabilities is the declared effect set.
	Capabilities []Capability `json:"capabilities"`
	// AllowedEnvironment is the allowlist of parent environment variable NAMES
	// the recipe may inherit. Credential-shaped names are never inherited.
	AllowedEnvironment []string `json:"allowed_environment,omitempty"`
}

// Timeout returns the recipe wall-clock timeout, applying the default when
// unset.
func (r Recipe) Timeout() time.Duration {
	if r.TimeoutNanos <= 0 {
		return DefaultTimeout
	}
	return time.Duration(r.TimeoutNanos)
}

// Digest returns a stable SHA-256 over the EFFECTIVE normalized recipe
// definition: executable, argv, working directory, declared capabilities,
// environment allowlist and the relevant timeout/output limits. The recipe id
// is intentionally excluded (it is the selector; the digest binds the
// definition). Any change to the effective definition changes the digest,
// which invalidates prior approvals and is detected as catalog drift on
// resume. The recipe must be normalized (Catalog normalizes at load).
func (r Recipe) Digest() string {
	payload, err := json.Marshal(struct {
		Executable         string       `json:"executable"`
		Argv               []string     `json:"argv,omitempty"`
		WorkingDirectory   string       `json:"working_directory,omitempty"`
		Capabilities       []Capability `json:"capabilities"`
		AllowedEnvironment []string     `json:"allowed_environment,omitempty"`
		TimeoutNanos       int64        `json:"timeout_nanos"`
		MaxStdoutBytes     int          `json:"max_stdout_bytes"`
		MaxStderrBytes     int          `json:"max_stderr_bytes"`
	}{
		Executable:         r.Executable,
		Argv:               append([]string(nil), r.Argv...),
		WorkingDirectory:   r.WorkingDirectory,
		Capabilities:       append([]Capability(nil), r.Capabilities...),
		AllowedEnvironment: append([]string(nil), r.AllowedEnvironment...),
		TimeoutNanos:       int64(r.Timeout()),
		MaxStdoutBytes:     r.OutputLimits.MaxStdoutBytes,
		MaxStderrBytes:     r.OutputLimits.MaxStderrBytes,
	})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// ApprovalFingerprint is the digest-bound approval identity of one run_recipe
// proposal: it binds the recipe id to its effective definition digest, so an
// operator approval for one definition can never authorize a different
// definition of the same id (capability, argv or environment changes all
// invalidate prior approvals).
func ApprovalFingerprint(recipeID, digest string) string {
	sum := sha256.Sum256([]byte("run_recipe\n" + recipeID + "\n" + digest))
	return hex.EncodeToString(sum[:])
}

// Defaults applied to recipes that do not set the corresponding field.
const (
	// DefaultTimeout is the default recipe wall-clock timeout.
	DefaultTimeout = 60 * time.Second
	// DefaultMaxStdoutBytes bounds stdout capture when unset.
	DefaultMaxStdoutBytes = 256 << 10
	// DefaultMaxStderrBytes bounds stderr capture when unset.
	DefaultMaxStderrBytes = 256 << 10
	// MaxOutputBytes is the hard cap on each captured stream. A recipe cannot
	// configure unbounded output: retention is always bounded in memory.
	MaxOutputBytes = 4 << 20
)

// Normalize fills defaults and validates the recipe. Unknown capabilities,
// missing identifiers, empty executables and absolute/traversing working
// directories are errors so a typo can never silently widen the recipe.
func (r Recipe) Normalize() (Recipe, error) {
	r.ID = strings.TrimSpace(r.ID)
	if r.ID == "" {
		return Recipe{}, errors.New("recipe id must not be empty")
	}
	r.Executable = strings.TrimSpace(r.Executable)
	if r.Executable == "" {
		return Recipe{}, fmt.Errorf("recipe %q: executable must not be empty", r.ID)
	}
	// Executables may be PATH names or absolute paths; they may never contain
	// traversal that could redirect resolution. Absolute paths are allowed for
	// operator-declared executables (for example /usr/bin/go), but `..`
	// components are refused.
	cleanExecutable := filepath.Clean(r.Executable)
	if cleanExecutable == ".." || strings.HasPrefix(cleanExecutable, ".."+string(filepath.Separator)) {
		return Recipe{}, fmt.Errorf("recipe %q: executable must not traverse directories", r.ID)
	}
	if wd := strings.TrimSpace(r.WorkingDirectory); wd != "" {
		if filepath.IsAbs(wd) || filepath.VolumeName(wd) != "" {
			return Recipe{}, fmt.Errorf("recipe %q: working directory must be relative to the workspace", r.ID)
		}
		for _, component := range strings.Split(filepath.ToSlash(wd), "/") {
			if component == ".." {
				return Recipe{}, fmt.Errorf("recipe %q: working directory must not contain ..", r.ID)
			}
		}
		r.WorkingDirectory = filepath.ToSlash(filepath.Clean(wd))
	}
	if r.TimeoutNanos <= 0 {
		r.TimeoutNanos = int64(DefaultTimeout)
	}
	if r.OutputLimits.MaxStdoutBytes <= 0 {
		r.OutputLimits.MaxStdoutBytes = DefaultMaxStdoutBytes
	}
	if r.OutputLimits.MaxStderrBytes <= 0 {
		r.OutputLimits.MaxStderrBytes = DefaultMaxStderrBytes
	}
	if r.OutputLimits.MaxStdoutBytes > MaxOutputBytes {
		return Recipe{}, fmt.Errorf("recipe %q: max_stdout_bytes exceeds the %d cap", r.ID, MaxOutputBytes)
	}
	if r.OutputLimits.MaxStderrBytes > MaxOutputBytes {
		return Recipe{}, fmt.Errorf("recipe %q: max_stderr_bytes exceeds the %d cap", r.ID, MaxOutputBytes)
	}
	seenCapabilities := make(map[Capability]bool, len(r.Capabilities))
	normalized := make([]Capability, 0, len(r.Capabilities))
	for _, raw := range r.Capabilities {
		capability, err := ParseCapability(strings.TrimSpace(string(raw)))
		if err != nil {
			return Recipe{}, fmt.Errorf("recipe %q: %w", r.ID, err)
		}
		if !seenCapabilities[capability] {
			seenCapabilities[capability] = true
			normalized = append(normalized, capability)
		}
	}
	r.Capabilities = normalized
	seenEnv := make(map[string]bool, len(r.AllowedEnvironment))
	allowed := make([]string, 0, len(r.AllowedEnvironment))
	// Inheriting parent environment variables is a declared effect: it is
	// only allowed when the recipe explicitly declares the
	// inherit_environment capability. An allowlist without the capability is
	// refused fail-closed.
	hasInheritEnv := false
	for _, capability := range r.Capabilities {
		if capability == CapabilityInheritEnvironment {
			hasInheritEnv = true
			break
		}
	}
	for _, name := range r.AllowedEnvironment {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if isCredentialName(name) {
			// A credential-shaped name is refused even when listed, so an
			// operator mistake can never leak provider credentials.
			return Recipe{}, fmt.Errorf("recipe %q: environment variable %q is a protected credential name and cannot be allowed", r.ID, name)
		}
		if !hasInheritEnv {
			return Recipe{}, fmt.Errorf("recipe %q: allowed_environment requires the %q capability to be declared", r.ID, CapabilityInheritEnvironment)
		}
		if !seenEnv[name] {
			seenEnv[name] = true
			allowed = append(allowed, name)
		}
	}
	sort.Strings(allowed)
	r.AllowedEnvironment = allowed
	return r, nil
}

// Catalog is the immutable, operator-controlled recipe set. It is read once at
// startup and is never derived from workspace content.
type Catalog struct {
	recipes map[string]Recipe
}

// NewCatalog validates and indexes a recipe set. Duplicate IDs and invalid
// recipes are errors.
func NewCatalog(recipes []Recipe) (*Catalog, error) {
	index := make(map[string]Recipe, len(recipes))
	for _, raw := range recipes {
		normalized, err := raw.Normalize()
		if err != nil {
			return nil, err
		}
		if _, exists := index[normalized.ID]; exists {
			return nil, fmt.Errorf("duplicate recipe id %q", normalized.ID)
		}
		index[normalized.ID] = normalized
	}
	return &Catalog{recipes: index}, nil
}

// ParseCatalog decodes a JSON array of recipes with a strict decoder and
// validates it. Unknown fields, duplicate object keys and any trailing JSON
// after the array are rejected, so a typo or a concatenated payload can never
// silently change a recipe definition (consistent with the main protocol
// parser).
func ParseCatalog(data []byte) (*Catalog, error) {
	if err := rejectDuplicateObjectKeys(data); err != nil {
		return nil, fmt.Errorf("decode recipe catalog: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var recipes []Recipe
	if err := decoder.Decode(&recipes); err != nil {
		return nil, fmt.Errorf("decode recipe catalog: %w", err)
	}
	// Strict single-value decoding: the document must end at EOF. A second
	// JSON value or trailing garbage is refused, never silently ignored.
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode recipe catalog: trailing JSON value after the recipe array")
		}
		return nil, fmt.Errorf("decode recipe catalog: trailing content after the recipe array: %w", err)
	}
	return NewCatalog(recipes)
}

// rejectDuplicateObjectKeys rejects duplicate keys anywhere in the JSON
// document so a duplicated field can never silently override another.
func rejectDuplicateObjectKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := scanJSONValue(decoder, 0); err != nil {
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, depth int) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if depth >= maxCatalogJSONDepth {
		return errors.New("JSON nesting is too deep")
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := seen[name]; exists {
				return fmt.Errorf("duplicate object key %q", name)
			}
			seen[name] = struct{}{}
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("unterminated object")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("unterminated array")
		}
	}
	return nil
}

const maxCatalogJSONDepth = 64

// LoadCatalog reads and parses a recipe catalog file.
func LoadCatalog(path string) (*Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read recipe catalog %s: %w", path, err)
	}
	return ParseCatalog(data)
}

// Get returns the recipe with the given id.
func (c *Catalog) Get(id string) (Recipe, bool) {
	if c == nil {
		return Recipe{}, false
	}
	recipe, ok := c.recipes[id]
	return recipe, ok
}

// IDs returns the sorted recipe ids.
func (c *Catalog) IDs() []string {
	if c == nil {
		return nil
	}
	ids := make([]string, 0, len(c.recipes))
	for id := range c.recipes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Len returns the number of recipes.
func (c *Catalog) Len() int {
	if c == nil {
		return 0
	}
	return len(c.recipes)
}

// Digest returns a stable SHA-256 over the whole effective catalog: the
// sorted "id=definitionDigest" pairs. It is persisted with the task
// configuration so resume can reject catalog drift fail-closed (a re-supplied
// catalog whose effective definitions changed can never silently continue a
// task under a different recipe set). An empty catalog hashes to the empty
// string.
func (c *Catalog) Digest() string {
	if c == nil || len(c.recipes) == 0 {
		return ""
	}
	lines := make([]string, 0, len(c.recipes))
	for _, id := range c.IDs() {
		recipe, ok := c.Get(id)
		if !ok {
			continue
		}
		lines = append(lines, id+"="+recipe.Digest())
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// credentialNameMarkers are case-insensitive substrings that mark an
// environment variable name as credential-shaped. They always win over the
// recipe allowlist so provider credentials and secrets can never reach a
// child process.
var credentialNameMarkers = []string{
	"API_KEY", "APISECRET", "AUTHORIZATION", "CHATGPT", "CLIENT_SECRET",
	"COOKIE", "CREDENTIAL", "OMNIROUTE", "PASSWD", "PASSWORD", "SECRET",
	"SESSION", "TOKEN",
}

// isCredentialName reports whether the environment variable name is
// credential-shaped.
func isCredentialName(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range credentialNameMarkers {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

// BuildEnvironment constructs the child environment from an explicit
// allowlist. The complete parent environment is never inherited: PATH is
// always passed (so the executable can be resolved), the recipe id marker is
// always set, and only Recipe.AllowedEnvironment names are copied from the
// parent, subject to the credential denylist AND to the recipe declaring the
// inherit_environment capability (without it, nothing is inherited even when
// an allowlist is present).
func BuildEnvironment(parent []string, r Recipe) []string {
	parentValues := make(map[string]string, len(parent))
	for _, entry := range parent {
		if index := strings.IndexByte(entry, '='); index > 0 {
			parentValues[entry[:index]] = entry[index+1:]
		}
	}
	hasInheritEnv := false
	for _, capability := range r.Capabilities {
		if capability == CapabilityInheritEnvironment {
			hasInheritEnv = true
			break
		}
	}
	seen := make(map[string]bool)
	var env []string
	add := func(name, value string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		env = append(env, name+"="+value)
	}
	if path, ok := parentValues["PATH"]; ok {
		add("PATH", path)
	} else {
		add("PATH", "/usr/bin:/bin")
	}
	add("RUNSTEAD_RECIPE_ID", r.ID)
	if hasInheritEnv {
		for _, name := range r.AllowedEnvironment {
			if isCredentialName(name) {
				continue
			}
			if value, ok := parentValues[name]; ok {
				add(name, value)
			}
		}
	}
	return env
}

// boundedBuffer retains at most limit bytes while counting the total observed.
type boundedBuffer struct {
	limit int
	total int64
	data  []byte
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	b.total += int64(len(data))
	if len(b.data) < b.limit {
		remaining := b.limit - len(b.data)
		if remaining > len(data) {
			remaining = len(data)
		}
		b.data = append(b.data, data[:remaining]...)
	}
	return len(data), nil
}

func (b *boundedBuffer) truncated() bool {
	return b.total > int64(len(b.data))
}

// Result is the typed outcome of one recipe execution. ExitCode and Signal
// preserve the real process termination; a negative ExitCode means the
// process did not terminate normally (killed by signal, timeout or
// cancellation).
type Result struct {
	Started         bool
	ExitCode        int
	Signal          string
	Stdout          []byte
	Stderr          []byte
	StdoutBytes     int64
	StderrBytes     int64
	StdoutTruncated bool
	StderrTruncated bool
	TimedOut        bool
	Canceled        bool
	DurationNanos   int64
	Err             error
}

// StartError reports a failure before the process started (for example an
// executable that cannot be found).
var StartError = errors.New("recipe process could not start")

// Run executes the recipe argv directly in its own process group inside cwd.
// On timeout or cancellation the whole process group is terminated (SIGTERM
// then, after a grace period, SIGKILL on Unix). env is the already-built
// allowlisted environment. cwd must be an absolute canonical path already
// validated to live inside the workspace.
func Run(ctx context.Context, r Recipe, cwd string, env []string) Result {
	start := time.Now()
	timeoutCtx, cancel := context.WithTimeout(ctx, r.Timeout())
	defer cancel()

	command := exec.Command(r.Executable, r.Argv...)
	command.Dir = cwd
	command.Env = env
	if runtime.GOOS != "windows" {
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	var stdout, stderr boundedBuffer
	stdout.limit = r.OutputLimits.MaxStdoutBytes
	stderr.limit = r.OutputLimits.MaxStderrBytes
	command.Stdout = &stdout
	command.Stderr = &stderr

	if err := command.Start(); err != nil {
		return Result{
			Started: false,
			Err:     fmt.Errorf("%w: %v", StartError, err),
		}
	}
	pgid := command.Process.Pid
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		done <- command.Wait()
		close(finished)
	}()

	// Tree-termination barrier: the termination routine must complete before
	// Run returns, so no SIGTERM-ignoring child can outlive the attempt's TX2.
	// The goroutine signals completion via treeTerminated after the whole
	// process group was terminated (or after the process finished on its own).
	treeTerminated := make(chan struct{})
	go func() {
		defer close(treeTerminated)
		select {
		case <-timeoutCtx.Done():
			select {
			case <-finished:
				// The process finished on its own; nothing to terminate.
			default:
				terminateProcessGroup(pgid, 2*time.Second)
			}
		case <-finished:
			// Finished normally.
		}
	}()

	waitErr := <-done
	// Synchronous barrier: only return after the termination routine
	// completed, so the full process tree is dead (or the process finished on
	// its own) before the caller persists TX2.
	<-treeTerminated
	duration := time.Since(start)
	timedOut := timeoutCtx.Err() == context.DeadlineExceeded
	canceled := ctx.Err() != nil

	result := Result{
		Started:         true,
		Stdout:          stdout.data,
		Stderr:          stderr.data,
		StdoutBytes:     stdout.total,
		StderrBytes:     stderr.total,
		StdoutTruncated: stdout.truncated(),
		StderrTruncated: stderr.truncated(),
		TimedOut:        timedOut,
		Canceled:        canceled,
		DurationNanos:   duration.Nanoseconds(),
		ExitCode:        -1,
	}
	if command.ProcessState != nil {
		result.ExitCode = command.ProcessState.ExitCode()
		if status, ok := command.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			result.Signal = status.Signal().String()
		}
	}
	if waitErr != nil && !timedOut && !canceled {
		// The process failed to run or was killed outside a Runstead-initiated
		// termination; keep the real exit status and report the error.
		result.Err = waitErr
	}
	return result
}

// terminateProcessGroup terminates the whole process group. On Unix this
// sends SIGTERM to -pgid and escalates to SIGKILL after the grace period. On
// platforms without process groups it falls back to killing the direct
// process. A reaped group kill is a harmless ESRCH.
func terminateProcessGroup(pgid int, grace time.Duration) {
	if pgid <= 0 {
		return
	}
	if runtime.GOOS == "windows" {
		// Windows has no POSIX process groups; terminate the direct process.
		if process, err := os.FindProcess(pgid); err == nil {
			_ = process.Kill()
		}
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	time.Sleep(grace)
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// PolicyDecision records the control-plane decision that authorized (or
// refused) a recipe execution.
type PolicyDecision struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

// NetworkIsolationValue is the honest, always-applied value: the native
// runner does not enforce network isolation. A recipe that declares the
// network capability is simply allowed by the operator to touch the network.
const NetworkIsolationValue = "unenforced"

// Evidence is the structured process evidence persisted for the verifier
// (#11). It preserves the real exit status, signal, bounded output and
// truncation; it never concludes task completion.
type Evidence struct {
	RecipeID         string         `json:"recipe_id"`
	Executable       string         `json:"executable"`
	Argv             []string       `json:"argv,omitempty"`
	WorkingDirectory string         `json:"working_directory"`
	Capabilities     []Capability   `json:"capabilities"`
	Policy           PolicyDecision `json:"policy"`
	DurationNanos    int64          `json:"duration_nanos"`
	ExitCode         int            `json:"exit_code"`
	Signal           string         `json:"signal,omitempty"`
	Stdout           string         `json:"stdout,omitempty"`
	Stderr           string         `json:"stderr,omitempty"`
	StdoutBytes      int64          `json:"stdout_bytes"`
	StderrBytes      int64          `json:"stderr_bytes"`
	StdoutTruncated  bool           `json:"stdout_truncated"`
	StderrTruncated  bool           `json:"stderr_truncated"`
	TimedOut         bool           `json:"timed_out,omitempty"`
	Canceled         bool           `json:"canceled,omitempty"`
	NetworkIsolation string         `json:"network_isolation"`
	Started          bool           `json:"started"`
	ActionID         string         `json:"action_id,omitempty"`
	ExecutionID      string         `json:"execution_id,omitempty"`
	EvidenceID       string         `json:"evidence_id,omitempty"`
}

// BuildEvidence constructs the persisted process evidence from a result.
// workingDirectory is the normalized slash-separated path relative to the
// workspace. The policy decision is the control-plane decision recorded
// before execution.
func BuildEvidence(r Recipe, workingDirectory string, policy PolicyDecision, result Result) Evidence {
	return Evidence{
		RecipeID:         r.ID,
		Executable:       r.Executable,
		Argv:             append([]string(nil), r.Argv...),
		WorkingDirectory: workingDirectory,
		Capabilities:     append([]Capability(nil), r.Capabilities...),
		Policy:           policy,
		DurationNanos:    result.DurationNanos,
		ExitCode:         result.ExitCode,
		Signal:           result.Signal,
		Stdout:           string(result.Stdout),
		Stderr:           string(result.Stderr),
		StdoutBytes:      result.StdoutBytes,
		StderrBytes:      result.StderrBytes,
		StdoutTruncated:  result.StdoutTruncated,
		StderrTruncated:  result.StderrTruncated,
		TimedOut:         result.TimedOut,
		Canceled:         result.Canceled,
		NetworkIsolation: NetworkIsolationValue,
		Started:          result.Started,
	}
}
