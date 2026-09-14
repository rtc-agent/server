package memory

import "errors"

var (
	ErrNotFound      = errors.New("memory not found")
	ErrInvalidScope  = errors.New("invalid scope type")
	ErrInvalidType   = errors.New("invalid memory type")
	ErrDuplicateLink = errors.New("memory link already exists")
)
