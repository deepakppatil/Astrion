package domain

import (
	"errors"
	"fmt"
)

// Sentinel errors mapped to HTTP status codes at the API boundary.
var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrForbidden    = errors.New("forbidden")
	ErrUnauthorized = errors.New("unauthorized")
	ErrInvalid      = errors.New("invalid request")
	ErrRateLimited  = errors.New("rate limited")
	ErrUnavailable  = errors.New("capacity unavailable")
	ErrExpired      = errors.New("expired")
)

// ValidationError carries field-level problems.
type ValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e *ValidationError) Error() string { return fmt.Sprintf("%s: %s", e.Field, e.Message) }
func (e *ValidationError) Unwrap() error { return ErrInvalid }

func Invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// Errorf wraps a sentinel with context while keeping errors.Is working.
func Errorf(sentinel error, format string, args ...any) error {
	return fmt.Errorf("%w: %s", sentinel, fmt.Sprintf(format, args...))
}
