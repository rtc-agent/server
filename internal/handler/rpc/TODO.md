# Action RPC Handlers Implementation Plan

## 未实现的 Action RPC Handlers (3个)

### 1. StopTurn (`v1.turn.stop`)
- **文件**: `action.stopturn.go` + `usecase/turn.go`
- **功能**: 
  - 停止正在执行的 Turn（更新状态为 cancelled）
  - 跨节点取消：通过 Redis Pub/Sub 发布取消信号
  - Worker 订阅 `turn:cancel:{turn_id}`，收到消息后 cancel context
  - Session 串行：使用 asynq Group 确保同一 Session 的任务串行执行
- **状态**: ✅ 已完成

### 2. UpdateRtcStatus (`v1.rtc.update_status`)
- **文件**: `action.updatertcstatus.go` + `usecase/rtc.go`
- **功能**: 更新 RTC 执行状态（executing/failed/timeout/rejected）
- **状态**: ❌ 待实现

### 3. SubmitRtcResult (`v1.rtc.submit_result`)
- **文件**: `action.submitrtcresult.go` + `usecase/rtc.go`
- **功能**: 提交 RTC 执行结果，继续 LLM 流程
- **状态**: ❌ 待实现

---

## 已完成
- ✅ SendMessage (`v1.message.send`)
- ✅ CloseSession (`v1.session.close`)
- ✅ UpdateSession (`v1.session.update`)
