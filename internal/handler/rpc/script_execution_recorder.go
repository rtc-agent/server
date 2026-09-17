package rpchandler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
	"go.uber.org/zap"
)

// scriptExecutionRecorder asynchronously records script execution details.
// It uses a fixed number of worker goroutines consuming from a buffered channel,
// avoiding the creation of many goroutines under high concurrency.
type scriptExecutionRecorder struct {
	deps  *Dependencies // rpchandler.Dependencies, holds ScriptExecutionRepo and Metrics
	tasks chan scriptRecordTask
	wg    sync.WaitGroup
	quit  chan struct{}
}

// scriptRecordTask is the task unit passed through the recorder channel.
type scriptRecordTask struct {
	ctx context.Context // created with context.WithoutCancel, independent of RPC handler
	rtc *model.Rtc
	req *protocol.SubmitRtcResultRequest
}

// newScriptExecutionRecorder creates and starts the recorder.
// workerCount controls the number of concurrent writers, bufferSize controls the task queue capacity.
func newScriptExecutionRecorder(deps *Dependencies, workerCount, bufferSize int) *scriptExecutionRecorder {
	r := &scriptExecutionRecorder{
		deps:  deps,
		tasks: make(chan scriptRecordTask, bufferSize),
		quit:  make(chan struct{}),
	}

	// Start a fixed number of workers.
	for i := 0; i < workerCount; i++ {
		r.wg.Add(1)
		go r.worker(i)
	}

	return r
}

// submit submits an asynchronous recording task. Non-blocking; drops the task if the channel is full.
func (r *scriptExecutionRecorder) submit(ctx context.Context, rtc *model.Rtc, req *protocol.SubmitRtcResultRequest) {
	select {
	case r.tasks <- scriptRecordTask{ctx: ctx, rtc: rtc, req: req}:
		// Successfully submitted.
	default:
		// Channel is full, drop the task (non-critical path, does not affect main flow).
		logger.Warn(ctx, "[scriptExecutionRecorder] task queue full, dropping record",
			zap.String("rtc", rtc.ID.String()))
	}
}

// shutdown gracefully shuts down, waiting for all workers to finish.
func (r *scriptExecutionRecorder) shutdown() {
	close(r.quit)
	r.wg.Wait()
}

// worker is the recorder's work loop.
// It consumes tasks from the tasks channel; on quit signal, drains remaining tasks before exiting.
func (r *scriptExecutionRecorder) worker(id int) {
	defer r.wg.Done()
	defer func() {
		if rv := recover(); rv != nil {
			logger.Error(context.Background(), "[scriptExecutionRecorder] worker goroutine panic",
				zap.Int("worker_id", id),
				zap.Any("panic", rv),
			)
		}
	}()

	for {
		select {
		case <-r.quit:
			// Drain remaining tasks in the channel.
			for {
				select {
				case task := <-r.tasks:
					r.safeProcessTask(task)
				default:
					return
				}
			}
		case task := <-r.tasks:
			r.safeProcessTask(task)
		}
	}
}

// safeProcessTask wraps processTask with panic recovery to prevent permanent worker exit.
// Design reference: design doc Section 9.4.1
func (r *scriptExecutionRecorder) safeProcessTask(task scriptRecordTask) {
	defer func() {
		if rv := recover(); rv != nil {
			logger.Error(task.ctx, "[scriptExecutionRecorder] worker panic recovered",
				zap.Any("panic", rv))
		}
	}()
	r.processTask(task)
}

// processTask executes the core logic of script execution recording.
// It extracts information from RTC Parameters and submission results, builds a ScriptExecution record, and writes it to the DB.
func (r *scriptExecutionRecorder) processTask(task scriptRecordTask) {
	ctx := task.ctx
	rtc := task.rtc
	req := task.req

	// 1. Extract title, action, name, code from RTC.Parameters.
	var params struct {
		Title  string `json:"title"`
		Action string `json:"action"`
		Name   string `json:"name"`
		Code   string `json:"code"`
	}
	if rtc.Parameters != "" {
		if err := json.Unmarshal([]byte(rtc.Parameters), &params); err != nil {
			logger.Warn(ctx, "[scriptExecutionRecorder] unmarshal parameters failed",
				zap.String("rtc", rtc.ID.String()),
				zap.Error(err))
		}
	}
	if params.Action == "" {
		params.Action = "eval" // default action
	}

	// 2. Extract frontend-reported execution metadata from req.Result.
	var durationMs int64
	var resultSize int64
	var logsList, warningsList, errorsList model.StringArray

	if req.Result != nil {
		// Calculate result size (still requires Marshal).
		resultBytes, err := json.Marshal(req.Result)
		if err != nil {
			logger.Warn(ctx, "[scriptExecutionRecorder] marshal result failed",
				zap.String("rtc", rtc.ID.String()),
				zap.Error(err))
		}
		resultSize = int64(len(resultBytes))

		// Extract fields directly from the map to avoid a second JSON parse.
		if resultData, ok := req.Result.(map[string]interface{}); ok {
			if v, ok := resultData["duration_ms"]; ok {
				switch d := v.(type) {
				case float64:
					durationMs = int64(d)
				case int64:
					durationMs = d
				case int:
					durationMs = int64(d)
				}
			}
			if v, ok := resultData["logs"]; ok {
				logsList = extractStringSlice(v)
			}
			if v, ok := resultData["warnings"]; ok {
				warningsList = extractStringSlice(v)
			}
			if v, ok := resultData["errors"]; ok {
				errorsList = extractStringSlice(v)
			}
		}
	}

	// 3. Calculate code size and SHA-256 hash.
	var codeSize int64
	var codeHash string
	if params.Code != "" {
		codeSize = int64(len(params.Code))
		sum := sha256.Sum256([]byte(params.Code))
		codeHash = hex.EncodeToString(sum[:])
	}

	// 4. Determine execution status.
	status := "success"
	if !req.Success {
		status = "failed"
	}

	// 5. Retrieve user_id (defensive logging, design doc Section 9.4).
	userID, ok := contextx.GetUserID(ctx)
	if !ok {
		logger.Warn(ctx, "[scriptExecutionRecorder] missing user_id in context",
			zap.String("rtc", rtc.ID.String()))
	}

	// 6. Build the ScriptExecution record.
	now := time.Now()
	exec := &model.ScriptExecution{
		RtcID:        rtc.ID,
		SessionID:    rtc.SessionID,
		TurnID:       rtc.TurnID,
		UserID:       userID,
		Title:        params.Title,
		Action:       params.Action,
		ScriptName:   params.Name,
		CodeHash:     codeHash,
		Status:       status,
		DurationMs:   durationMs,
		ResultSize:   resultSize,
		CodeSize:     codeSize,
		ErrorMessage: model.DerefStr(req.Error),
		Logs:         logsList,
		Warnings:     warningsList,
		Errors:       errorsList,
		CreatedAt:    rtc.CreatedAt,
		CompletedAt:  &now,
	}

	// 7. Write to the database (with timeout to prevent indefinite blocking when connection pool is full).
	dbCtx, dbCancel := context.WithTimeout(ctx, 30*time.Second)
	defer dbCancel()
	if err := r.deps.ScriptExecutionRepo.Create(dbCtx, exec); err != nil {
		logger.Error(ctx, "[scriptExecutionRecorder] create failed",
			zap.String("rtc", rtc.ID.String()),
			zap.Error(err))
		return
	}

	// 8. Log structured Loki entry.
	logger.Info(ctx, "script.execution_completed",
		zap.String("rtc_id", rtc.ID.String()),
		zap.String("session_id", rtc.SessionID.String()),
		zap.String("user_id", userID.String()),
		zap.String("title", params.Title),
		zap.String("action", params.Action),
		zap.String("status", status),
		zap.Int64("duration_ms", durationMs),
		zap.Int64("result_size", resultSize),
		zap.Int64("code_size", codeSize),
	)

	// 9. Record Prometheus metrics (Metrics may be nil).
	if r.deps.Metrics != nil {
		r.deps.Metrics.RecordScriptExecution(ctx, turnagent.ScriptExecutionMetricsAttrs{
			Action:     params.Action,
			Status:     status,
			DurationMs: durationMs,
			ResultSize: resultSize,
			CodeSize:   codeSize,
		})
	}
}

// extractStringSlice safely extracts a model.StringArray from an interface{}.
// After JSON deserialization, a string slice is typically []interface{},
// but it may also be []string (when constructed directly).
func extractStringSlice(v interface{}) model.StringArray {
	switch s := v.(type) {
	case []interface{}:
		result := make(model.StringArray, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				result = append(result, str)
			}
		}
		return result
	case []string:
		return model.StringArray(s)
	default:
		return nil
	}
}
