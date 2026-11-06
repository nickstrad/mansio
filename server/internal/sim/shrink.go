package sim

import "time"

// Shrink looks for a smaller run that still fails: the duration cut to just past the first violation, then
// each fault class turned off, then fewer databases, keeping each change while the run still fails. Every
// stream is per class and per database, so turning one off does not move the others' draws; the failure may
// still change shape (DST-3 shrinks op scripts properly). run runs one config, each in its own synctest bubble.
//
//	pre: res = run(cfg) failed
func Shrink(cfg Config, res Result, run func(Config) Result) (Config, Result) {
	fails := func(r Result) bool { return r.Err != nil || len(r.Violations) > 0 }
	c := cfg.settle().explicit()
	try := func(next Config) bool {
		r := run(next)
		if !fails(r) {
			return false
		}
		c, res = next, r
		return true
	}
	if len(res.Violations) > 0 {
		next := c
		next.Duration = res.Violations[0].At.Truncate(time.Second) + time.Second
		try(next)
	}
	offs := []func(f *Faults){
		func(f *Faults) { f.CrashEvery = 0 },
		func(f *Faults) { f.StormEvery = 0 },
		func(f *Faults) { f.Stall = 0 },
		func(f *Faults) { f.FailCall = 0 },
		func(f *Faults) { f.LoseReply = 0 },
		func(f *Faults) { f.ClientLose = 0 },
	}
	for _, off := range offs {
		f := *c.Faults
		off(&f)
		if f == *c.Faults {
			continue
		}
		next := c
		next.Faults = &f
		try(next)
	}
	for c.Keys > 1 {
		next := c
		next.Keys--
		if !try(next) {
			break
		}
	}
	return c, res
}
