// internal/handler/rpc/error_codes.go
package rpchandler

// Error codes follow dot-separated format: domain.error_type
// This provides consistent namespacing and better categorization.
const (
	// Auth errors
	ErrorCodeUnauthorized     = "auth.unauthorized"
	ErrorCodePermissionDenied = "auth.permission_denied"

	// Session errors
	ErrorCodeSessionNotFound = "session.not_found"
	ErrorCodeSessionClosed   = "session.closed"

	// Message errors
	ErrorCodeMessageNotFound         = "message.not_found"
	ErrorCodeMessageInvalidCursor    = "message.invalid_cursor"
	ErrorCodeMessageClientIDConflict = "message.client_id_conflict"

	// Turn errors
	ErrorCodeTurnNotFound = "turn.not_found"

	// RTC errors
	ErrorCodeRtcNotFound       = "rtc.not_found"
	ErrorCodeRtcClientMismatch = "rtc.client_id_mismatch"
	ErrorCodeRtcInvalidResult  = "rtc.invalid_result"
	ErrorCodeRtcInvalidStatus  = "rtc.invalid_status"
	ErrorCodeRtcInvalidState   = "rtc.invalid_state"

	// File errors
	ErrorCodeFileNotFound = "file.not_found"

	// RPC errors
	ErrorCodeInvalidRequest  = "rpc.invalid_request"
	ErrorCodeMethodNotFound  = "rpc.method_not_found"
	ErrorCodeInternalError   = "rpc.internal_error"
	ErrorCodeInvalidArgument = "rpc.invalid_argument"

	// Interrupt errors
	ErrorCodeInvalidSessionID   = "interrupt.invalid_session_id"
	ErrorCodeInvalidInterruptID = "interrupt.invalid_interrupt_id"
	ErrorCodeEmptyAnswer        = "interrupt.empty_answer"
	ErrorCodeAuthRequired       = "interrupt.auth_required"
	ErrorCodeSessionNotFoundInt = "interrupt.session_not_found"
	ErrorCodeAuthForbidden      = "interrupt.auth_forbidden"

	// Close errors
	ErrorCodeCloseError = "close.error"

	// Compact errors
	ErrorCodeCompactAlreadyPending = "compact.already_pending"
)
