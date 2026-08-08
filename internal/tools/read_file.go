package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"unicode/utf8"
)

func (r *Registry) executeReadFile(ctx context.Context, observation Observation, path string) Observation {
	resolved, failure := r.workspace.resolve(path)
	if failure != nil {
		observation.Failure = failure
		return observation
	}
	if !resolved.info.Mode().IsRegular() {
		observation.Failure = newFailure(FailureWrongType)
		return observation
	}
	content, original, truncated, binary, invalid, hash, err := readBoundedFile(ctx, resolved.canonical, r.limits.MaxReadBytes)
	if err != nil {
		if failure := contextFailure(ctx); failure != nil {
			observation.Failure = failure
		} else {
			observation.Failure = newFailure(FailureReadFailure)
		}
		return observation
	}
	if binary {
		observation.Failure = newFailure(FailureBinaryFile)
		return observation
	}
	if invalid {
		observation.Failure = newFailure(FailureInvalidUTF8)
		return observation
	}
	content = validUTF8Prefix(content)
	observation.Success = true
	observation.Truncated = truncated
	observation.Data = FileData{Path: resolved.relative, Content: string(content), SHA256: hash}
	observation.Metadata = Metadata{
		Source:        ToolReadFile,
		Untrusted:     true,
		Path:          resolved.relative,
		SizeBytes:     original,
		BytesOriginal: original,
		BytesReturned: int64(len(content)),
		ExitCode:      -1,
	}
	return observation
}

func readBoundedFile(ctx context.Context, path string, limit int) ([]byte, int64, bool, bool, bool, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, false, false, false, "", err
	}
	defer file.Close()

	buffer := make([]byte, 32<<10)
	content := make([]byte, 0, limit)
	var original int64
	pending := []byte(nil)
	binary := false
	invalid := false
	hasher := sha256.New()
	for {
		if failure := contextFailure(ctx); failure != nil {
			return nil, original, false, false, false, "", errors.New(string(failure.Code))
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			chunk := buffer[:count]
			original += int64(count)
			hasher.Write(chunk)
			if len(content) < limit {
				take := limit - len(content)
				if take > len(chunk) {
					take = len(chunk)
				}
				content = append(content, chunk[:take]...)
			}
			if bytes.IndexByte(chunk, 0) >= 0 {
				binary = true
			}
			var invalidChunk bool
			pending, invalidChunk = scanUTF8(pending, chunk)
			if invalidChunk {
				invalid = true
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, original, false, false, false, "", readErr
		}
	}
	if len(pending) != 0 {
		invalid = true
	}
	return content, original, original > int64(limit), binary, invalid, hex.EncodeToString(hasher.Sum(nil)), nil
}

func scanUTF8(pending, chunk []byte) ([]byte, bool) {
	combined := make([]byte, len(pending)+len(chunk))
	copy(combined, pending)
	copy(combined[len(pending):], chunk)
	for len(combined) > 0 {
		runeValue, size := utf8.DecodeRune(combined)
		if runeValue == utf8.RuneError && size == 1 {
			if !utf8.FullRune(combined) {
				return append([]byte(nil), combined...), false
			}
			return nil, true
		}
		combined = combined[size:]
	}
	return nil, false
}

func validUTF8Prefix(content []byte) []byte {
	for len(content) > 0 && !utf8.Valid(content) {
		content = content[:len(content)-1]
	}
	return content
}
