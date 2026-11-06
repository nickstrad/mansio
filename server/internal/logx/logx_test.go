package logx_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/logx"
)

func TestNewWritesJSONInUTC(t *testing.T) {
	var buf bytes.Buffer
	logx.New(&buf, slog.LevelInfo).Info("hello", "k", 1)
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("not JSON: %q: %v", buf.String(), err)
	}
	ts, err := time.Parse(time.RFC3339Nano, line["time"].(string))
	if err != nil || !strings.HasSuffix(line["time"].(string), "Z") || ts.IsZero() {
		t.Fatalf("time %v is not UTC RFC3339", line["time"])
	}
	if line["msg"] != "hello" || line["k"] != float64(1) {
		t.Fatalf("line = %v", line)
	}
}

func TestNewID(t *testing.T) {
	a, b := logx.NewID(), logx.NewID()
	if len(a) != 8 || a == b {
		t.Fatalf("ids %q %q: want 8 hex chars, distinct", a, b)
	}
}
