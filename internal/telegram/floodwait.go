package telegram

import (
	"context"
	"time"

	"github.com/gotd/td/bin"
	gotdtelegram "github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

type floodGate struct {
	onWait func(time.Duration)
}

func (g *floodGate) middleware() gotdtelegram.Middleware {
	return gotdtelegram.MiddlewareFunc(func(next tg.Invoker) gotdtelegram.InvokeFunc {
		return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
			for {
				err := next.Invoke(ctx, input, output)
				d, ok := tgerr.AsFloodWait(err)
				if !ok {
					return err
				}
				wait := d + time.Second
				if g.onWait != nil {
					g.onWait(wait)
				}
				timer := time.NewTimer(wait)
				select {
				case <-timer.C:
				case <-ctx.Done():
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					return ctx.Err()
				}
			}
		}
	})
}
