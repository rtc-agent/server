// Package rpchandler 提供 Centrifuge RPC 接口的协议适配层。
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

// scriptExecutionRecorder 异步记录 script 执行详情。
// 使用固定数量的 worker goroutine 从带缓冲 channel 中消费任务，
// 避免高并发场景下产生大量 goroutine。
type scriptExecutionRecorder struct {
	deps  *Dependencies // rpchandler.Dependencies，持有 ScriptExecutionRepo 和 Metrics
	tasks chan scriptRecordTask
	wg    sync.WaitGroup
	quit  chan struct{}
}

// scriptRecordTask 是 recorder channel 中传递的任务单元。
type scriptRecordTask struct {
	ctx context.Context // 使用 context.WithoutCancel 创建，独立于 RPC handler
	rtc *model.Rtc
	req *protocol.SubmitRtcResultRequest
}

// newScriptExecutionRecorder 创建并启动 recorder。
// workerCount 控制并发写入数，bufferSize 控制任务队列容量。
func newScriptExecutionRecorder(deps *Dependencies, workerCount, bufferSize int) *scriptExecutionRecorder {
	r := &scriptExecutionRecorder{
		deps:  deps,
		tasks: make(chan scriptRecordTask, bufferSize),
		quit:  make(chan struct{}),
	}

	// 启动固定数量的 worker
	for i := 0; i < workerCount; i++ {
		r.wg.Add(1)
		go r.worker(i)
	}

	return r
}

// submit 提交一个异步记录任务。非阻塞，channel 满时丢弃。
func (r *scriptExecutionRecorder) submit(ctx context.Context, rtc *model.Rtc, req *protocol.SubmitRtcResultRequest) {
	select {
	case r.tasks <- scriptRecordTask{ctx: ctx, rtc: rtc, req: req}:
		// 成功提交
	default:
		// channel 已满，丢弃任务（非关键路径，不影响主流程）
		logger.Warn(ctx, "[scriptExecutionRecorder] task queue full, dropping record",
			zap.String("rtc", rtc.ID.String()))
	}
}

// shutdown 优雅关闭，等待所有 worker 完成。
func (r *scriptExecutionRecorder) shutdown() {
	close(r.quit)
	r.wg.Wait()
}

// worker 是 recorder 的工作循环。
// 从 tasks channel 消费任务，quit 信号触发后排空剩余任务再退出。
func (r *scriptExecutionRecorder) worker(id int) {
	defer r.wg.Done()

	for {
		select {
		case <-r.quit:
			// 排空 channel 中剩余任务
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

// safeProcessTask 包装 processTask，捕获 panic 防止 worker 永久退出。
// 设计参考：设计文档 Section 9.4.1
func (r *scriptExecutionRecorder) safeProcessTask(task scriptRecordTask) {
	defer func() {
		if rv := recover(); rv != nil {
			logger.Error(task.ctx, "[scriptExecutionRecorder] worker panic recovered",
				zap.Any("panic", rv))
		}
	}()
	r.processTask(task)
}

// processTask 执行 script 执行记录的核心逻辑。
// 从 RTC Parameters 和提交结果中提取信息，构建 ScriptExecution 记录并写入 DB。
func (r *scriptExecutionRecorder) processTask(task scriptRecordTask) {
	ctx := task.ctx
	rtc := task.rtc
	req := task.req

	// 1. 从 RTC.Parameters 中提取 title、action、name、code
	var params struct {
		Title  string `json:"title"`
		Action string `json:"action"`
		Name   string `json:"name"`
		Code   string `json:"code"`
	}
	if rtc.Parameters != "" {
		_ = json.Unmarshal([]byte(rtc.Parameters), &params)
	}
	if params.Action == "" {
		params.Action = "eval" // 默认 action
	}

	// 2. 从 req.Result 中提取前端报告的执行元数据
	var durationMs int64
	var resultSize int64
	var logsList, warningsList, errorsList model.StringArray

	if req.Result != nil {
		resultBytes, _ := json.Marshal(req.Result)
		resultSize = int64(len(resultBytes))

		var resultData struct {
			DurationMs int64    `json:"duration_ms"`
			Logs       []string `json:"logs"`
			Warnings   []string `json:"warnings"`
			Errors     []string `json:"errors"`
		}
		_ = json.Unmarshal(resultBytes, &resultData)
		durationMs = resultData.DurationMs
		logsList = resultData.Logs
		warningsList = resultData.Warnings
		errorsList = resultData.Errors
	}

	// 3. 计算代码大小和 SHA-256 哈希
	var codeSize int64
	var codeHash string
	if params.Code != "" {
		codeSize = int64(len(params.Code))
		sum := sha256.Sum256([]byte(params.Code))
		codeHash = hex.EncodeToString(sum[:])
	}

	// 4. 确定执行状态
	status := "success"
	if !req.Success {
		status = "failed"
	}

	// 5. 获取 user_id（防御性日志，设计文档 Section 9.4）
	userID, ok := contextx.GetUserID(ctx)
	if !ok {
		logger.Warn(ctx, "[scriptExecutionRecorder] missing user_id in context",
			zap.String("rtc", rtc.ID.String()))
	}

	// 6. 构建 ScriptExecution 记录
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

	// 7. 写入数据库（设置超时防止连接池满时无限阻塞）
	dbCtx, dbCancel := context.WithTimeout(ctx, 30*time.Second)
	defer dbCancel()
	if err := r.deps.ScriptExecutionRepo.Create(dbCtx, exec); err != nil {
		logger.Error(ctx, "[scriptExecutionRecorder] create failed",
			zap.String("rtc", rtc.ID.String()),
			zap.Error(err))
		return
	}

	// 8. 记录 Loki 结构化日志
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

	// 9. 记录 Prometheus 指标（Metrics 可能为 nil）
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
