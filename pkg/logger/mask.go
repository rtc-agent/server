package logger

import (
	"strings"
	"unicode/utf8"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// sensitiveFieldKeys lists zap field keys whose string values must be masked
// before being written to any log output. Keep this list in sync with the
// project's privacy requirements.
var sensitiveFieldKeys = map[string]bool{
	"user_id":     true,
	"user_id_raw": true,
	"email":       true,
}

// MaskUserID masks a user identifier so that only the first 3 and last 3
// characters are visible. For short IDs (<=6 chars) the entire value is
// replaced with "***". This keeps logs useful for debugging while preventing
// accidental leakage of full identifiers.
//
// Examples:
//
//	"abc"            -> "***"
//	"abcdef"         -> "***"
//	"abcdefg"        -> "abc***efg"
//	"550e8400-e29b"  -> "550***29b"  (UUID prefix)
func MaskUserID(userID string) string {
	if userID == "" {
		return ""
	}
	length := utf8.RuneCountInString(userID)
	if length <= 6 {
		return "***"
	}
	runes := []rune(userID)
	return string(runes[:3]) + "***" + string(runes[len(runes)-3:])
}

// MaskEmail masks an email address, keeping only the first character of the
// local part and the full domain. Example: "alice@example.com" -> "a***@example.com".
func MaskEmail(email string) string {
	if email == "" {
		return ""
	}
	at := strings.Index(email, "@")
	if at < 0 {
		// Not a valid email; treat as opaque identifier.
		return MaskUserID(email)
	}
	local := email[:at]
	domain := email[at:]
	if utf8.RuneCountInString(local) <= 1 {
		return "***" + domain
	}
	runes := []rune(local)
	return string(runes[:1]) + "***" + domain
}

// maskField returns a new zap.Field with the value masked if the field key is
// in the sensitive set. Non-string fields and non-sensitive keys pass through
// unchanged.
func maskField(f zap.Field) zap.Field {
	if !sensitiveFieldKeys[f.Key] {
		return f
	}
	// Only mask string-typed fields. Other types (Any, etc.) are left as-is
	// because we cannot safely convert them.
	if f.Type == zapcore.StringType {
		original := f.String
		if f.Key == "email" {
			f.String = MaskEmail(original)
		} else {
			f.String = MaskUserID(original)
		}
		return f
	}
	// For Any-typed fields carrying a string, mask the interface value.
	if f.Type == zapcore.ReflectType && f.Interface != nil {
		if s, ok := f.Interface.(string); ok {
			if f.Key == "email" {
				f.Interface = MaskEmail(s)
			} else {
				f.Interface = MaskUserID(s)
			}
			return f
		}
	}
	return f
}

// maskingCore wraps a zapcore.Core and transparently masks sensitive field
// values before they reach the underlying encoder.
type maskingCore struct {
	zapcore.Core
}

func (c *maskingCore) With(fields []zap.Field) zapcore.Core {
	masked := make([]zap.Field, len(fields))
	for i, f := range fields {
		masked[i] = maskField(f)
	}
	return &maskingCore{Core: c.Core.With(masked)}
}

func (c *maskingCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	// Register ourselves (not the inner core) so that Write is called on us,
	// giving us the chance to mask sensitive fields before they reach the
	// underlying encoder.
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

func (c *maskingCore) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	masked := make([]zapcore.Field, len(fields))
	for i, f := range fields {
		masked[i] = maskField(f)
	}
	return c.Core.Write(ent, masked)
}

// wrapWithMasking returns a *zap.Zap that wraps the given logger's core with
// a maskingCore, ensuring all sensitive fields are automatically masked.
func wrapWithMasking(l *zap.Logger) *zap.Logger {
	return l.WithOptions(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
		return &maskingCore{Core: c}
	}))
}
