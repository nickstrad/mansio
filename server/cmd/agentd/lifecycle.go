package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/nickstrad/mansio/server/internal/errs"
)

// phase is one step of the shutdown sequence (Release It H3: shutdown has one budget).
type phase struct {
	name string
	max  time.Duration // cap on this phase; 0 = whatever is left of the budget
	run  func(ctx context.Context) error
}

// phaseDeadline is when a phase starting at now must end: its own cap, but never past the budget's end.
//
//	post: result <= end; max > 0 ⇒ result <= now + max
func phaseDeadline(now, end time.Time, max time.Duration) time.Time {
	if max > 0 && now.Add(max).Before(end) {
		return now.Add(max)
	}
	return end
}

// shutdown runs phases in order under one total budget that starts now. Each phase gets a ctx with
// phaseDeadline; a phase that fails or runs out does not stop the next one (a forced drain still closes the
// databases), and a phase that starts after the budget ended still runs with an expired ctx, so it can do its
// non-blocking part (close what is clean, keep the rest). Every phase logs one line (event AGENT.PHASE_DONE,
// or AGENT.PHASE_FAILED at WARN); the errors come back joined and wrapped once.
func shutdown(log *slog.Logger, budget time.Duration, phases []phase) error {
	start := time.Now()
	end := start.Add(budget)
	var failed []error
	for _, p := range phases {
		t0 := time.Now()
		ctx, cancel := context.WithDeadline(context.Background(), phaseDeadline(t0, end, p.max))
		err := p.run(ctx)
		cancel()
		took := time.Since(t0)
		if err != nil {
			log.Warn("shutdown phase failed", "event", "AGENT.PHASE_FAILED", "phase", p.name, "took_ms", took.Milliseconds(),
				"error", err.Error())
			failed = append(failed, err)
			continue
		}
		log.Info("shutdown phase done", "event", "AGENT.PHASE_DONE", "phase", p.name, "took_ms", took.Milliseconds())
	}
	took := time.Since(start)
	log.Info("shutdown done", "event", "AGENT.SHUTDOWN_DONE", "took_ms", took.Milliseconds(), "budget_ms", budget.Milliseconds(),
		"ok", len(failed) == 0)
	if len(failed) > 0 {
		return errs.Wrap(module, errors.Join(failed...), "shutdown did not finish cleanly within its %v budget", budget)
	}
	return nil
}
