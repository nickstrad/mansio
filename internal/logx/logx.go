// Package logx builds the structured logger: JSON lines with UTC timestamps, plus short log IDs that are
// shown to the user and written to the log so one can be found from the other.
package logx

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
)

// New returns a JSON logger writing to w with UTC timestamps. level may be a *slog.LevelVar to change it at
// run time.
func New(w io.Writer, level slog.Leveler) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				a.Value = slog.TimeValue(a.Value.Time().UTC())
			}
			return a
		},
	}))
}

// NewID returns a short random ID such as "3f9a1c0b".
func NewID() string {
	b := make([]byte, 4)
	rand.Read(b) // never fails (Go 1.24+)
	return hex.EncodeToString(b)
}
