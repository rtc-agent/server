package turnagent

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/cloudwego/eino/schema"
)

// StreamRecvResult holds the result of a stream recv call.
type StreamRecvResult struct {
	Msg *schema.Message
	Err error
}

// RecvWithTimeout wraps a blocking stream recv call with a timeout.
//
// It spawns a goroutine that calls recv() and sends the result on a buffered
// channel (cap=1). The caller selects on the channel, a timer, and ctx.Done().
//
// Goroutine lifecycle:
//   - If recv() returns before timeout → goroutine exits naturally.
//   - If timeout fires or ctx is cancelled → this function returns immediately.
//     The goroutine remains blocked on recv() until the caller closes the
//     stream, which causes recv() to return (typically with io.EOF or a
//     "use of closed stream" error). The buffered channel ensures the
//     goroutine never blocks on send. This is a bounded, temporary hold —
//     not a leak — as long as the caller closes the stream after timeout.
//
// recv is typically bound to (*schema.StreamReader[*schema.Message]).Recv.
// Internal panic recovery converts panics to error results.
func RecvWithTimeout(
	ctx context.Context,
	recv func() (*schema.Message, error),
	timeout time.Duration,
) (StreamRecvResult, bool) {
	ch := make(chan StreamRecvResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- StreamRecvResult{Err: fmt.Errorf("stream recv panic: %v\nstack: %s", r, string(debug.Stack()))}
			}
		}()
		msg, err := recv()
		ch <- StreamRecvResult{Msg: msg, Err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop() // safe: Stop on already-fired timer is a no-op (returns false)

	select {
	case <-ctx.Done():
		return StreamRecvResult{Err: ctx.Err()}, false
	case <-timer.C:
		return StreamRecvResult{}, true
	case res := <-ch:
		return res, false
	}
}
