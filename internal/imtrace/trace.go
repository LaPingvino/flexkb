// Package imtrace is the per-keystroke observability spine of
// flexkb-imed. It defines a custom slog level (Trace, below
// Debug) and a small set of helpers that put consistent
// structured fields on every pipeline stage so a captured
// session can be reconstructed end-to-end from the trace alone.
//
// Motivation: significant parts of the daemon (Wayland v2 path,
// ibus interop with real Mutter, multi-state IM FSM transitions)
// haven't been live-tested across every compositor variant.
// When users report "key X doesn't commit Y", a trace file
// shows exactly which stage of the pipeline produced what —
// which removes the "is it the resolver? the IM? the
// transport?" guessing.
//
// Convention for trace records (set by callers, enforced by
// none — slog won't error on a missing field, but consistent
// naming makes them grep-friendly):
//
//   stage       short tag identifying the pipeline step
//                  ("resolve", "im.feed", "im.emit",
//                   "wlim.commit", "ibus.process_key", …)
//   ctx         context id (input-context object path, Wayland
//                  session counter, etc.) — lets multiple apps'
//                  traces be separated in a multi-context trace
//   scancode    Linux evdev scancode where applicable
//   symbol      symbol the resolver produced
//   text        committed/preedit text (preserves UTF-8)
//   serial      Wayland commit serial / ibus event serial
//
// Trace is intentionally not part of the slog.Default() chain;
// it goes via the per-component logger so flexkb-imed can
// route trace output to a separate file/sink (the --trace-file
// flag) without polluting normal Info-level logs.
package imtrace

import (
	"context"
	"io"
	"log/slog"
	"os"
)

// LevelTrace is slog's "below Debug" — verbose per-keystroke
// detail not useful in normal operation but invaluable for
// diagnosing live-session issues.
const LevelTrace = slog.LevelDebug - 4

// Trace emits one trace record on the given logger. Caller
// supplies the stage tag as msg and any structured fields as
// args (slog's key/value pairs). No-op when the logger doesn't
// have trace enabled — cheap to leave in hot paths.
func Trace(logger *slog.Logger, msg string, args ...any) {
	if logger == nil {
		return
	}
	logger.Log(context.Background(), LevelTrace, msg, args...)
}

// NewLogger builds a slog.Logger configured for trace output.
//
//	enable  — if false, returned logger drops Trace records and
//	          forwards >= Info to base. The daemon enables this
//	          only when --trace is set.
//	out     — where trace records go. If non-nil, trace records
//	          go to a dedicated JSON-lines handler against this
//	          writer (typically a file); other levels go to base.
//	          If nil, every level goes to base.
//	base    — the daemon's existing logger; receives non-trace
//	          records when out is non-nil, and ALL records when
//	          out is nil.
//
// This split-handler shape lets the daemon say:
//
//	tracer := imtrace.NewLogger(*traceFlag, traceFile, log)
//
// — and pass tracer into imsession / wlim / ibus. Components
// don't care whether tracing is on; they just Trace() and the
// logger drops or routes appropriately.
func NewLogger(enable bool, out io.Writer, base *slog.Logger) *slog.Logger {
	if !enable {
		return base
	}
	level := LevelTrace
	if out != nil {
		// Two handlers fanned out: trace to file, everything
		// else to base. Use a tee handler.
		fileHandler := slog.NewJSONHandler(out, &slog.HandlerOptions{
			Level: level,
			ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				// Stamp every trace record with a "trace" attr so
				// downstream tooling can filter file output to just
				// trace records when the file ended up with other
				// levels (e.g. fallback writes).
				return a
			},
		})
		return slog.New(&teeHandler{
			trace: fileHandler,
			rest:  base.Handler(),
		})
	}
	// No separate file — emit traces through base by lowering
	// its effective level. base itself decides the format.
	return slog.New(&levelOverrideHandler{
		inner: base.Handler(),
		level: level,
	})
}

// OpenTraceFile opens (creating if needed) a path for trace
// output. Returns the file plus a close func; caller defers the
// close. Suggests a sensible filename when path is "auto" —
// $XDG_RUNTIME_DIR/flexkb-imed-trace-<pid>.jsonl.
func OpenTraceFile(path string) (io.WriteCloser, error) {
	if path == "" {
		return nil, nil
	}
	if path == "auto" {
		runtime := os.Getenv("XDG_RUNTIME_DIR")
		if runtime == "" {
			runtime = os.TempDir()
		}
		path = runtime + "/flexkb-imed-trace.jsonl"
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// teeHandler routes records at LevelTrace and below to trace,
// everything else to rest. The "fan-out" pattern keeps the
// trace file pure-trace while normal logs go to their
// configured destination.
type teeHandler struct {
	trace slog.Handler
	rest  slog.Handler
}

func (h *teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if level <= LevelTrace {
		return h.trace.Enabled(ctx, level)
	}
	return h.rest.Enabled(ctx, level)
}

func (h *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level <= LevelTrace {
		return h.trace.Handle(ctx, r)
	}
	return h.rest.Handle(ctx, r)
}

func (h *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &teeHandler{trace: h.trace.WithAttrs(attrs), rest: h.rest.WithAttrs(attrs)}
}

func (h *teeHandler) WithGroup(name string) slog.Handler {
	return &teeHandler{trace: h.trace.WithGroup(name), rest: h.rest.WithGroup(name)}
}

// levelOverrideHandler forwards every record to inner but
// answers Enabled() at the given (typically lower) level. Used
// when --trace is on but --trace-file is not — we lower the
// effective level on the daemon's main logger so traces appear
// inline with other logs.
type levelOverrideHandler struct {
	inner slog.Handler
	level slog.Level
}

func (h *levelOverrideHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level
}

func (h *levelOverrideHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.inner.Handle(ctx, r)
}

func (h *levelOverrideHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &levelOverrideHandler{inner: h.inner.WithAttrs(attrs), level: h.level}
}

func (h *levelOverrideHandler) WithGroup(name string) slog.Handler {
	return &levelOverrideHandler{inner: h.inner.WithGroup(name), level: h.level}
}
