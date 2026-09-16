package turnagent

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"
)

// StreamRecvResult 流接收结果
type StreamRecvResult struct {
	Msg *schema.Message
	Err error
}

// RecvWithTimeout 带超时的流接收。
// 返回结果和是否超时的标志。
// 超时调用方需要自行调用 stream.Close()。
//
// recv 通常绑定到 *schema.StreamReader[*schema.Message].Recv。
// 内部 goroutine 会通过 panic recover 捕获底层 panic 并以 error 形式返回。
func RecvWithTimeout(
	ctx context.Context,
	recv func() (*schema.Message, error),
	timeout time.Duration,
) (StreamRecvResult, bool) {
	ch := make(chan StreamRecvResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- StreamRecvResult{Err: fmt.Errorf("stream recv panic: %v", r)}
			}
		}()
		msg, err := recv()
		ch <- StreamRecvResult{Msg: msg, Err: err}
	}()

	timer := time.NewTimer(timeout)

	select {
	case <-ctx.Done():
		timer.Stop()
		return StreamRecvResult{Err: ctx.Err()}, false
	case <-timer.C:
		return StreamRecvResult{}, true
	case res := <-ch:
		timer.Stop()
		return res, false
	}
}
