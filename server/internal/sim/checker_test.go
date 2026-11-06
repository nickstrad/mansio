package sim

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nickstrad/mansio/server/internal/front"
)

// The checker's teeth, end to end through the real agent: workloads that break the contract on purpose must
// be caught. (Production-code mutants were also run by hand on 2026-11-05: dedupe off and a missed dirty
// commit were each caught within 30 seeds; see knowledge/determinism.)

// doubleApply writes every mark a second time under a fresh request id: the database holds it twice.
type doubleApply struct{ SQL }

func (w doubleApply) Send(ctx context.Context, a Agent, key string, r Request) Answer {
	ans := w.SQL.Send(ctx, a, key, r)
	if r.Kind == Write && ans.Code == front.OK && !ans.Duplicate {
		body, _ := json.Marshal(sqlBody{DB: key, RequestID: r.ID + "-again", Kind: "write",
			SQL: `INSERT INTO log (req, v) VALUES (?, 0)`, Args: []any{r.ID}})
		a.Front().Handle(ctx, front.Input{Ticket: "sim", Body: body, Timeout: 2 * time.Second})
	}
	return ans
}

// forgetful reads every database file without its newest mark, as if storage lost the last write.
type forgetful struct{ SQL }

func (w forgetful) Observe(path string) ([]string, error) {
	marks, err := w.SQL.Observe(path)
	if len(marks) > 0 {
		marks = marks[:len(marks)-1]
	}
	return marks, err
}

func TestCheckerCatchesBrokenWorkloads(t *testing.T) {
	quiet := &Faults{}
	for _, c := range []struct {
		name string
		w    Workload
		want string
	}{
		{"double apply", doubleApply{}, "applied twice"},
		{"lost newest write", forgetful{}, "before a write-back that published"}, // still a prefix: only durability can tell
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			var res Result
			synctest.Test(t, func(t *testing.T) {
				res = Run(Config{Seed: 7, Dir: dir, Keys: 1, Duration: time.Minute, Workload: c.w, Faults: quiet}, nil)
			})
			if res.Err != nil {
				t.Fatal(res.Err)
			}
			for _, v := range res.Violations {
				if strings.Contains(v.What, c.want) {
					return
				}
			}
			t.Fatalf("no violation containing %q in %v", c.want, res.Violations)
		})
	}
}
