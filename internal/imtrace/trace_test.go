package imtrace

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLevelTraceBelowDebug — fundamental contract: LevelTrace must
// be strictly below slog.LevelDebug so a handler configured at
// debug level drops traces. The daemon relies on this to keep
// `flexkb-imed -v` (debug) from drowning in per-keystroke spam.
func TestLevelTraceBelowDebug(t *testing.T) {
	if LevelTrace >= slog.LevelDebug {
		t.Errorf("LevelTrace (%d) must be < slog.LevelDebug (%d)", LevelTrace, slog.LevelDebug)
	}
}

// TestNewLoggerDisabledIsIdentity — when enable=false, NewLogger
// returns the base logger unchanged. Daemon must remain
// zero-overhead when --trace is off.
func TestNewLoggerDisabledIsIdentity(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	out := NewLogger(false, nil, base)
	if out != base {
		t.Errorf("NewLogger(false) should return base unchanged")
	}
}

// TestTraceCallDropsWhenDisabled — when the logger doesn't have
// trace enabled, Trace() emits nothing. Helper must be cheap to
// leave in hot paths.
func TestTraceCallDropsWhenDisabled(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	Trace(base, "hot.path", "key", "value")
	if buf.Len() != 0 {
		t.Errorf("Trace at info-level logger should be silent; got: %s", buf.String())
	}
}

// TestTraceCallRoutesWhenEnabled — with NewLogger(true, nil, base)
// the level-override handler lowers the threshold and trace
// records show up on the base output.
func TestTraceCallRoutesWhenEnabled(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	tracer := NewLogger(true, nil, base)
	Trace(tracer, "im.feed", "scancode", 30, "symbol", "a")
	out := buf.String()
	if !strings.Contains(out, "im.feed") {
		t.Errorf("trace record missing from output: %q", out)
	}
	if !strings.Contains(out, "scancode") || !strings.Contains(out, "symbol") {
		t.Errorf("trace attrs missing from output: %q", out)
	}
}

// TestTeeHandlerSplitsByLevel — when both a file writer and a base
// are supplied, trace records go to the file (as JSON-lines), and
// non-trace records go to the base (as text).
func TestTeeHandlerSplitsByLevel(t *testing.T) {
	var fileBuf, baseBuf bytes.Buffer
	base := slog.New(slog.NewTextHandler(&baseBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	tracer := NewLogger(true, &fileBuf, base)

	Trace(tracer, "wlim.commit", "text", "你好")
	tracer.Info("daemon ready", "backend", "ibus")

	// Trace record should be in the file (JSON shape).
	if !strings.Contains(fileBuf.String(), "wlim.commit") {
		t.Errorf("trace record missing from file output: %q", fileBuf.String())
	}
	// And the file should be JSON, not text.
	if !strings.Contains(fileBuf.String(), `"msg":"wlim.commit"`) {
		t.Errorf("file output should be JSON; got: %q", fileBuf.String())
	}
	// Info record should be on base, NOT in the file.
	if !strings.Contains(baseBuf.String(), "daemon ready") {
		t.Errorf("info record missing from base output: %q", baseBuf.String())
	}
	if strings.Contains(fileBuf.String(), "daemon ready") {
		t.Errorf("info record leaked into trace file: %q", fileBuf.String())
	}
}

// TestTeeHandlerJSONShape — each trace line in the file must be a
// parseable JSON object. Downstream tooling (jq, log aggregators)
// relies on the format.
func TestTeeHandlerJSONShape(t *testing.T) {
	var fileBuf bytes.Buffer
	base := slog.New(slog.NewTextHandler(io.Discard, nil))
	tracer := NewLogger(true, &fileBuf, base)

	Trace(tracer, "ibus.tier.v2", "ctx", "/app/ic1", "consumed", true)

	for _, line := range bytes.Split(bytes.TrimSpace(fileBuf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Errorf("trace line is not valid JSON: %v\n  line: %s", err, line)
		}
		if m["msg"] != "ibus.tier.v2" {
			t.Errorf("msg field: got %v, want ibus.tier.v2", m["msg"])
		}
	}
}

// TestOpenTraceFileAuto — "auto" should resolve to a path under
// $XDG_RUNTIME_DIR.
func TestOpenTraceFileAuto(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmp)
	w, err := OpenTraceFile("auto")
	if err != nil {
		t.Fatalf("OpenTraceFile(auto): %v", err)
	}
	defer w.Close()
	if _, err := os.Stat(filepath.Join(tmp, "flexkb-imed-trace.jsonl")); err != nil {
		t.Errorf("auto-derived path not created: %v", err)
	}
}

// TestOpenTraceFileEmpty — empty path returns (nil, nil) so the
// daemon can skip the file-handler branch entirely.
func TestOpenTraceFileEmpty(t *testing.T) {
	w, err := OpenTraceFile("")
	if err != nil {
		t.Fatal(err)
	}
	if w != nil {
		t.Errorf("OpenTraceFile(\"\") should return nil writer; got %v", w)
	}
}

// TestOpenTraceFileExplicit — an explicit path opens that file
// and subsequent writes append.
func TestOpenTraceFileExplicit(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "explicit.jsonl")
	w1, err := OpenTraceFile(path)
	if err != nil {
		t.Fatal(err)
	}
	w1.Write([]byte("line1\n"))
	w1.Close()

	w2, err := OpenTraceFile(path)
	if err != nil {
		t.Fatal(err)
	}
	w2.Write([]byte("line2\n"))
	w2.Close()

	contents, _ := os.ReadFile(path)
	if got := string(contents); got != "line1\nline2\n" {
		t.Errorf("file did not append: got %q", got)
	}
}

// TestTraceNilLoggerSafe — Trace must tolerate a nil logger
// because the hot-path callers don't all guard against it (and
// shouldn't have to).
func TestTraceNilLoggerSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Trace(nil) panicked: %v", r)
		}
	}()
	Trace(nil, "should.not.crash", "k", "v")
}
