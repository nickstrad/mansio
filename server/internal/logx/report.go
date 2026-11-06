package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/nickstrad/mansio/server/internal/errs"
)

// Levels are the log_level config values the services accept.
var Levels = map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn}

// Open returns a logger at level writing to file (appended, its directory created), or to stderr when file is
// "". close closes the file. Errors are the file system's; the caller wraps them in its module.
func Open(file string, level slog.Leveler, stderr io.Writer) (log *slog.Logger, close func(), err error) {
	if file == "" {
		return New(stderr, level), func() {}, nil
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return New(f, level), func() { f.Close() }, nil
}

// Exit codes Reporter returns: an expected failure, and a bug.
const (
	ExitError = 1
	ExitBug   = 70
)

// Reporter classifies a service's top-level error and reports it once (the cmd/sqlbench pattern): an outermost
// error from a Known module is an expected failure (ExitError), anything else a bug (ExitBug). The user gets one
// line with a log ID; the log gets the chain and the stack. Before Log is set a bug prints its details.
type Reporter struct {
	Log    *slog.Logger
	Stderr io.Writer
	Name   string          // the service, in the log message "<name> failed"
	Known  map[string]bool // error modules whose errors are expected
}

// Report reports err and returns the exit code.
func (r Reporter) Report(err error) int {
	if e, ok := errs.Outermost(err); ok && r.Known[e.Module] {
		fmt.Fprintf(r.Stderr, "error: %s%s\n", e.Error(), r.record(slog.LevelWarn, r.Name+" failed", err, errs.Stack(err)))
		return ExitError
	}
	return r.Bug(err, errs.Stack(err))
}

// Bug reports err (a panic or an unclassified error) with its stack and returns ExitBug.
func (r Reporter) Bug(err error, stack string) int {
	if r.Log == nil {
		fmt.Fprintf(r.Stderr, "%v\n%s\nerror: something unexpected went wrong. This is a bug; please report the details above\n", err, stack)
		return ExitBug
	}
	fmt.Fprintf(r.Stderr, "error: something unexpected went wrong. This is a bug; please report%s\n",
		r.record(slog.LevelError, "unexpected error (bug)", err, stack))
	return ExitBug
}

func (r Reporter) record(level slog.Level, msg string, err error, stack string) string {
	if r.Log == nil {
		return ""
	}
	id := NewID()
	attrs := []slog.Attr{slog.String("log_id", id), slog.String("error", err.Error()), slog.Any("chain", errs.Chain(err))}
	if stack != "" {
		attrs = append(attrs, slog.String("stack", stack))
	}
	r.Log.LogAttrs(context.Background(), level, msg, attrs...)
	return " [log ID " + id + "]"
}
