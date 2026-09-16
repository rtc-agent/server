package memory

import "errors"

// Sentinel errors returned by the memory package.
var (
	// ErrNotFound indicates the requested memory record does not exist.
	ErrNotFound = errors.New("memory not found")
	// ErrInvalidScope indicates an unsupported scope type.
	ErrInvalidScope = errors.New("invalid scope type")
	// ErrInvalidType indicates an unsupported memory type.
	ErrInvalidType = errors.New("invalid memory type")
	// ErrDuplicateLink indicates a duplicate memory link.
	ErrDuplicateLink = errors.New("memory link already exists")

	// ErrRequiredField indicates a required field is missing during validation.
	ErrRequiredField = errors.New("required field missing")
)
