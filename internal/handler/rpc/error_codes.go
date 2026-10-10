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

	// Send errors
	ErrorCodeSendCreateMessage  = "send.create_message_failed"
	ErrorCodeSendCreateSession  = "send.create_session_failed"
	ErrorCodeSendTouchSession   = "send.touch_session_failed"
	ErrorCodeSendQueueUninit    = "send.queue_uninitialized"
	ErrorCodeSendMarshalPayload = "send.marshal_payload_failed"
	ErrorCodeSendQueuePublish   = "send.queue_publish_failed"

	// Close errors (extended)
	ErrorCodeCloseUpdateStatus = "close.update_status_failed"

	// Open errors
	ErrorCodeOpenUpdateStatus  = "open.update_status_failed"
	ErrorCodeOpenClearClosedAt = "open.clear_closed_at_failed"

	// Fork errors
	ErrorCodeForkCreateSession       = "fork.create_session_failed"
	ErrorCodeForkBatchCreateMessages = "fork.batch_create_messages_failed"

	// Submit RTC result errors
	ErrorCodeSubmitUpdateRtcResult    = "submit.update_rtc_result_failed"
	ErrorCodeSubmitGetInputMessage    = "submit.get_input_message_failed"
	ErrorCodeSubmitParseInputContent  = "submit.parse_input_content_failed"
	ErrorCodeSubmitParseInputToolCall = "submit.parse_input_tool_call_failed"
	ErrorCodeSubmitCreateOutputMsg    = "submit.create_output_message_failed"

	// Update RTC status errors
	ErrorCodeUpdateRtcStatus = "update_rtc.status_update_failed"

	// Update session errors
	ErrorCodeUpdateSessionFields = "update.session_fields_failed"
)
