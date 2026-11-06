package sim

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

// ShrinkCluster looks for a smaller cluster run that still fails (DST-3): the drawn nemesis schedule becomes
// an explicit op script; the duration is cut to just past the first violation (ops after it dropped); the
// script is cut down by delta debugging (halves, then smaller chunks, then single ops); then each fault class
// is turned off, the clients and boot databases reduced. Each change is kept while the run still fails. Every
// stream is per class, database and client, so a cut leaves the other draws alone; the failure may still
// change shape, so a change is kept only while the run fails in the same class (FailureClass) as res: the
// shrinker does not drift to a different bug. run runs one config (each in its own synctest bubble). log, if
// set, hears each kept step.
//
//	pre: res = run(cfg) failed
func ShrinkCluster(cfg ClusterConfig, res ClusterResult, run func(ClusterConfig) ClusterResult, log func(string)) (ClusterConfig, ClusterResult) {
	class := FailureClass(res)
	fails := func(r ClusterResult) bool { return FailureClass(r) == class }
	note := func(format string, args ...any) {
		if log != nil {
			log(fmt.Sprintf(format, args...))
		}
	}
	c := res.Config
	c.Dir, c.KeepGoing = cfg.Dir, false
	c.Scripted, c.Script = true, slices.Clone(res.Script)
	try := func(next ClusterConfig, what string) bool {
		r := run(next)
		if !fails(r) {
			return false
		}
		c, res = next, r
		note("kept: %s (%d ops, %v): %s", what, len(c.Script), c.Duration, firstFailure(r))
		return true
	}
	if !try(c, "the drawn schedule as a script") {
		note("the run passes with its schedule as a script: shrinking the knobs only")
	}
	if len(res.Violations) > 0 {
		next := c
		next.Duration = res.Violations[0].At.Truncate(time.Second) + 2*time.Second
		next.Script = slices.DeleteFunc(slices.Clone(c.Script), func(o Op) bool { return o.At > next.Duration })
		if next.Duration < c.Duration {
			try(next, "duration cut")
		}
	}
	// Delta debugging over the script: remove chunks of size n, halving n.
	for n := max(len(c.Script)/2, 1); n >= 1; n /= 2 {
		for i := 0; i < len(c.Script); {
			next := c
			next.Script = slices.Concat(c.Script[:i], c.Script[min(i+n, len(c.Script)):])
			if !try(next, fmt.Sprintf("ops %d..%d removed", i, i+n-1)) {
				i += n
			}
		}
	}
	offs := []struct {
		name string
		off  func(f *ClusterFaults)
	}{
		{"drop", func(f *ClusterFaults) { f.Drop = 0 }}, {"duplicate", func(f *ClusterFaults) { f.Duplicate = 0 }},
		{"spike", func(f *ClusterFaults) { f.Spike = 0 }}, {"stall", func(f *ClusterFaults) { f.Stall = 0 }},
		{"failcall", func(f *ClusterFaults) { f.FailCall = 0 }}, {"losereply", func(f *ClusterFaults) { f.LoseReply = 0 }},
		{"notify", func(f *ClusterFaults) { f.Notify = 0 }},
	}
	for _, o := range offs {
		f := *c.Faults
		o.off(&f)
		if f == *c.Faults {
			continue
		}
		next := c
		next.Faults = &f
		try(next, o.name+" off")
	}
	for c.Clients > 1 {
		next := c
		next.Clients--
		if !try(next, "one client fewer") {
			break
		}
	}
	for c.DBs > 1 {
		next := c
		next.DBs--
		if !try(next, "one boot database fewer") {
			break
		}
	}
	return c, res
}

// failureClasses name the checker's violations by a phrase each message carries, in this order (the first
// match wins).
var failureClasses = []struct{ class, phrase string }{
	{"split-brain", "two owners serve writes"}, {"epoch-order", "committed a write at epoch"},
	{"conservation", "conservation:"}, {"not-linearizable", "not linearizable"},
	{"applied-twice", "applied twice"}, {"applied-though-failed", "is applied though"},
	{"bad-duplicate", "answered duplicate"}, {"unknown-mark", "unknown mark"},
	{"lost-at-handoff", "a hand-off loses nothing"}, {"lost-after-publish", "before a write-back or fence that published"},
	{"lost-beyond-rpo", "(RPO "}, {"lost", "is lost"}, {"missing", "is missing"},
	{"generations", "generation"},
}

// FailureClass names how a run failed: "" (it passed), "error" (the harness failed: Err), or the class of
// its first violation ("other" for a message no class names).
func FailureClass(r ClusterResult) string {
	switch {
	case r.Err != nil:
		return "error"
	case len(r.Violations) == 0:
		return ""
	}
	for _, c := range failureClasses {
		if strings.Contains(r.Violations[0].What, c.phrase) {
			return c.class
		}
	}
	return "other"
}

func firstFailure(r ClusterResult) string {
	if r.Err != nil {
		return r.Err.Error()
	}
	return r.Violations[0].What[:min(len(r.Violations[0].What), 120)]
}

// Vector is a regression vector: a shrunk failing run, committed under testdata/cluster once fixed, so it
// runs (and must pass) in every `go test`.
type Vector struct {
	Name   string        `json:"name"`
	Why    string        `json:"why"`              // the violation it found, and the fix
	Expect string        `json:"expect,omitempty"` // an open finding's FailureClass (testdata/open)
	Config ClusterConfig `json:"config"`
}

// ReadVector reads one vector file.
func ReadVector(path string) (Vector, error) {
	var v Vector
	b, err := os.ReadFile(path)
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return v, fmt.Errorf("sim: vector %s: %w", path, err)
	}
	return v, nil
}

// WriteVector writes v to path (indented JSON).
func WriteVector(path string, v Vector) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
