package logx

import (
	"context"
	"log/slog"
	"strings"

	"github.com/nickstrad/mansio/server/internal/breaker"
)

// BreakerLog logs every transition of dep's breaker with a stable message code: <DEP>.BREAKER_OPEN (WARN),
// <DEP>.BREAKER_HALF_OPEN and <DEP>.BREAKER_CLOSED (INFO) — e.g. S3.BREAKER_OPEN (Release It C4, O3). Use it
// as breaker.Config.OnChange.
func BreakerLog(log *slog.Logger, dep string) func(breaker.Transition) {
	return func(t breaker.Transition) {
		code := strings.ToUpper(dep) + ".BREAKER_" + strings.ToUpper(t.To.String())
		level := slog.LevelInfo
		if t.To == breaker.Open {
			level = slog.LevelWarn
		}
		log.Log(context.Background(), level, "circuit breaker "+t.To.String(), "event", code, "dep", dep, "from", t.From.String(),
			"calls", t.Calls, "failures", t.Failures, "manual", t.Manual)
	}
}
