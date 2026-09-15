package repo

import (
	"fmt"
	"time"

	"github.com/rtc-agent/server/internal/model"
)

// goalTerminalStatuses 定义 Goal 的终态集合
var goalTerminalStatuses = []string{
	string(model.GoalStatusCompleted),
	string(model.GoalStatusCancelled),
	string(model.GoalStatusExhausted),
}

// loopTerminalStatuses 定义 Loop 的终态集合
var loopTerminalStatuses = []string{
	string(model.LoopStatusCompleted),
	string(model.LoopStatusCancelled),
	string(model.LoopStatusExhausted),
}

// autoFillCompletedAt 在终态时自动填充 completed_at 字段。
// 当 fields 中包含 status 且值为终态之一，且 completed_at 未被显式设置时，
// 自动将 completed_at 设为当前时间。
func autoFillCompletedAt(fields map[string]any, terminalStatuses []string) {
	if _, ok := fields["status"]; ok {
		statusStr := fmt.Sprintf("%v", fields["status"])
		for _, ts := range terminalStatuses {
			if statusStr == ts && fields["completed_at"] == nil {
				fields["completed_at"] = time.Now()
				return
			}
		}
	}
}
