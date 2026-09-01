package composition

import (
	"errors"
	"fmt"
)

// ErrorCode is the stable classification for composition failures.
type ErrorCode string

const (
	ErrorInvalidProfile        ErrorCode = "invalid_profile"
	ErrorUnknownPackage        ErrorCode = "unknown_package"
	ErrorUnknownPackageVersion ErrorCode = "unknown_package_version"
	ErrorDuplicatePackage      ErrorCode = "duplicate_package"
	ErrorCompositionConflict   ErrorCode = "composition_conflict"
	ErrorMissingRecipeCatalog  ErrorCode = "missing_recipe_catalog"
	ErrorMissingRecipe         ErrorCode = "missing_recipe"
	ErrorMissingTool           ErrorCode = "missing_tool"
	ErrorProviderMismatch      ErrorCode = "provider_mismatch"
	ErrorInvalidContract       ErrorCode = "invalid_contract"
	ErrorInvalidPackage        ErrorCode = "invalid_package"
)

var (
	ErrInvalidProfile        = errors.New("invalid composition profile")
	ErrUnknownPackage        = errors.New("unknown capability package")
	ErrUnknownPackageVersion = errors.New("unknown capability package version")
	ErrDuplicatePackage      = errors.New("duplicate capability package")
	ErrCompositionConflict   = errors.New("capability package conflict")
	ErrMissingRecipeCatalog  = errors.New("recipe catalog required by composition")
	ErrMissingRecipe         = errors.New("recipe required by composition is unavailable")
	ErrMissingTool           = errors.New("tool required by composition is unavailable")
	ErrProviderMismatch      = errors.New("profile provider does not match resolved provider")
	ErrInvalidContract       = errors.New("invalid frozen execution contract")
	ErrInvalidPackage        = errors.New("invalid capability package")
)

// Error is a typed, stable composition error. Its text contains only bounded
// operator-controlled identifiers and never model output or credential values.
type Error struct {
	Code   ErrorCode
	Path   string
	Detail string
	base   error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Path == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Detail)
	}
	return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Detail)
}

func (e *Error) Unwrap() error { return e.base }

func compositionError(code ErrorCode, base error, path, format string, args ...any) error {
	return &Error{Code: code, Path: path, Detail: fmt.Sprintf(format, args...), base: base}
}
