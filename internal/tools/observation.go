package tools

import "fmt"

type FailureCode string

const (
	FailureUnknownTool      FailureCode = "unknown_tool"
	FailureInvalidArguments FailureCode = "invalid_arguments"
	FailureAbsolutePath     FailureCode = "absolute_path"
	FailurePathTraversal    FailureCode = "path_traversal"
	FailureSymlinkEscape    FailureCode = "symlink_escape"
	FailurePathNotFound     FailureCode = "path_not_found"
	FailureWrongType        FailureCode = "wrong_type"
	FailureBinaryFile       FailureCode = "binary_file"
	FailureInvalidUTF8      FailureCode = "invalid_utf8"
	FailureOutputTruncated  FailureCode = "output_truncated"
	FailureNotGitRepository FailureCode = "not_git_repository"
	FailureGitFailure       FailureCode = "git_failure"
	FailureSearchFailure    FailureCode = "search_failure"
	FailureReadFailure      FailureCode = "read_failure"
	FailureListFailure      FailureCode = "list_failure"
	FailureCanceled         FailureCode = "canceled"
	FailureTimeout          FailureCode = "timeout"
	FailureStaleState       FailureCode = "stale_state"
	FailureInvalidPatch     FailureCode = "invalid_patch"
	FailureWriteFailure     FailureCode = "write_failure"
	FailureWriteTooLarge    FailureCode = "write_too_large"
	FailureUnknownRecipe    FailureCode = "unknown_recipe"
	FailureRecipeStart      FailureCode = "recipe_start_failed"
	FailureNoRecipes        FailureCode = "no_recipes_configured"
)

var failureMessages = map[FailureCode]string{
	FailureUnknownTool:      "the requested tool is not registered",
	FailureInvalidArguments: "the tool arguments are invalid",
	FailureAbsolutePath:     "absolute paths are not allowed",
	FailurePathTraversal:    "path traversal is not allowed",
	FailureSymlinkEscape:    "the path resolves outside the workspace",
	FailurePathNotFound:     "the requested path does not exist",
	FailureWrongType:        "the target has the wrong type",
	FailureBinaryFile:       "binary content is not returned as text",
	FailureInvalidUTF8:      "invalid UTF-8 content is not returned as text",
	FailureOutputTruncated:  "the observation output was truncated",
	FailureNotGitRepository: "the workspace is not a Git repository",
	FailureGitFailure:       "the Git observation failed",
	FailureSearchFailure:    "the text search failed",
	FailureReadFailure:      "the file could not be read",
	FailureListFailure:      "the directory could not be listed",
	FailureCanceled:         "the observation was canceled",
	FailureTimeout:          "the observation timed out",
	FailureStaleState:       "the file changed since the observed before-state; the write is refused",
	FailureInvalidPatch:     "the patch is malformed or cannot be applied deterministically",
	FailureWriteFailure:     "the write effect failed",
	FailureWriteTooLarge:    "the write target exceeds the configured bound",
	FailureUnknownRecipe:    "the requested recipe is not in the configured catalog",
	FailureRecipeStart:      "the recipe process could not start",
	FailureNoRecipes:        "no recipes are configured; run_recipe is unavailable",
}

type Failure struct {
	Code    FailureCode `json:"code"`
	Message string      `json:"message"`
}

func newFailure(code FailureCode) *Failure {
	return &Failure{Code: code, Message: failureMessages[code]}
}

// InvalidArgumentFailure returns a typed invalid-arguments failure. It is
// exported for the agent loop's argument decoding.
func InvalidArgumentFailure() *Failure {
	return newFailure(FailureInvalidArguments)
}

func (f Failure) Error() string {
	if f.Code == "" {
		return "tool failure"
	}
	return fmt.Sprintf("tool failure: %s", f.Code)
}

type Observation struct {
	ID        string   `json:"id"`
	Tool      string   `json:"tool"`
	Arguments any      `json:"arguments"`
	Success   bool     `json:"success"`
	Data      any      `json:"data,omitempty"`
	Failure   *Failure `json:"failure,omitempty"`
	Truncated bool     `json:"truncated"`
	Metadata  Metadata `json:"metadata"`
}

type Metadata struct {
	Source              string `json:"source"`
	Backend             string `json:"backend,omitempty"`
	Untrusted           bool   `json:"untrusted"`
	Path                string `json:"path,omitempty"`
	SizeBytes           int64  `json:"size_bytes,omitempty"`
	BytesOriginal       int64  `json:"bytes_original,omitempty"`
	BytesReturned       int64  `json:"bytes_returned,omitempty"`
	EntriesOriginal     int    `json:"entries_original,omitempty"`
	EntriesReturned     int    `json:"entries_returned,omitempty"`
	MatchesOriginal     int    `json:"matches_original,omitempty"`
	MatchesReturned     int    `json:"matches_returned,omitempty"`
	StdoutBytesOriginal int64  `json:"stdout_bytes_original,omitempty"`
	StdoutBytesReturned int64  `json:"stdout_bytes_returned,omitempty"`
	StderrBytesOriginal int64  `json:"stderr_bytes_original,omitempty"`
	StderrBytesReturned int64  `json:"stderr_bytes_returned,omitempty"`
	ExitCode            int    `json:"exit_code"`
	Signal              string `json:"signal,omitempty"`
}

type FileData struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	// SHA256 is the sha256 of the COMPLETE file content (never truncated),
	// even when the returned Content is bounded. It is the stale-state
	// precondition source for write_file and apply_patch.
	SHA256 string `json:"sha256,omitempty"`
}

type EntryType string

const (
	EntryFile      EntryType = "file"
	EntryDirectory EntryType = "directory"
	EntrySymlink   EntryType = "symlink"
	EntryOther     EntryType = "other"
)

type FileEntry struct {
	Path string    `json:"path"`
	Type EntryType `json:"type"`
	Size int64     `json:"size_bytes,omitempty"`
}

type ListData struct {
	Path    string      `json:"path"`
	Entries []FileEntry `json:"entries"`
}

type SearchMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type SearchData struct {
	Matches            []SearchMatch `json:"matches"`
	SkippedBinaryFiles int           `json:"skipped_binary_files"`
	SkippedInvalidUTF8 int           `json:"skipped_invalid_utf8_files"`
}

type GitData struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}
