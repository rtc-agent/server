// Package templateutil provides shared text/template rendering helpers.
//
// Several packages within the agent subsystem needed identical
// parse-then-execute logic with only minor variations in error handling.
// This package consolidates them so the core rendering path lives in one
// place and callers can choose the error semantics they need.
package templateutil

import (
	"bytes"
	"context"
	"fmt"
	"text/template"

	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
)

// Render parses and executes a text/template, returning the result or an error.
//
// This is the error-returning variant intended for callers that want to
// propagate template failures (e.g. system-prompt and command rendering).
func Render(name, tmplStr string, data any) (string, error) {
	t, err := template.New(name).Parse(tmplStr)
	if err != nil {
		return "", fmt.Errorf("template parse %q: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("template execute %q: %w", name, err)
	}
	return buf.String(), nil
}

// MustRender is like Render but degrades gracefully on error: it logs the
// failure and returns a placeholder string instead of propagating the error.
//
// This is intended for embedded / hard-coded templates where a failure
// indicates a programmer error but crashing the process is unacceptable.
func MustRender(name, tmplStr string, data any) string {
	result, err := Render(name, tmplStr, data)
	if err != nil {
		logger.Error(context.Background(), "template render failed",
			zap.String("template", name), zap.Error(err))
		return fmt.Sprintf("[template render error: %s]", name)
	}
	return result
}
