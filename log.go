package aish

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// ctxHandler adds the transaction, event and hook of a core-issued ctx to
// records logged with the *Context methods, so logs line up with the trace.
type ctxHandler struct {
	inner slog.Handler
}

func (h ctxHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	if info := infoOf(ctx); info != nil {
		if info.meta.ID != "" {
			r.AddAttrs(slog.Uint64("tx", info.meta.Tx), slog.String("event", info.meta.ID))
		}
		if info.hook != nil {
			r.AddAttrs(slog.String("hook", info.hook.info.ID))
		}
	}

	return h.inner.Handle(ctx, r) //nolint:wrapcheck // a handler passes its inner handler's error through
}

func (h ctxHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return ctxHandler{h.inner.WithAttrs(attrs)}
}

func (h ctxHandler) WithGroup(name string) slog.Handler {
	return ctxHandler{h.inner.WithGroup(name)}
}

// maxPayloadBytes cuts the event bodies in debug records; the journal and the
// trace keep everything.
const maxPayloadBytes = 64 << 10

// PayloadAttr is a log attribute holding v as JSON, cut at maxPayloadBytes. It
// is for debug records of event bodies; callers check Enabled first.
func PayloadAttr(key string, v any) slog.Attr {
	b, err := json.Marshal(v)
	if err != nil {
		return slog.String(key, fmt.Sprintf("<cannot encode %T: %v>", v, err))
	}
	if len(b) > maxPayloadBytes {
		return slog.String(key, fmt.Sprintf("%s…(cut, %d more bytes)", b[:maxPayloadBytes], len(b)-maxPayloadBytes))
	}

	return slog.String(key, string(b))
}
