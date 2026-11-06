package front_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/ir"
	"github.com/nickstrad/mansio/server/internal/ticket"
)

// observed collects OpsEvents.
type observed struct {
	mu sync.Mutex
	ev []front.OpsEvent
}

func (o *observed) observe(e front.OpsEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ev = append(o.ev, e)
}

func (o *observed) last(t *testing.T) front.OpsEvent {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.ev) == 0 {
		t.Fatal("no event observed")
	}
	return o.ev[len(o.ev)-1]
}

// TestOpsObserver: the observer sees each request's trace id (kept or generated, always echoed), handler
// name, sizes, outcome, kind, app, tenant, and the transaction with its statements.
func TestOpsObserver(t *testing.T) {
	o := &observed{}
	r := newObservedOpsRig(t, front.Config{}, o.observe, "acme:notes")
	r.schemas.add("notes", notesSchema)
	w, rd := r.tok(t, "acme:notes", ticket.Write), r.tok(t, "acme:notes", ticket.Read)

	body := request("acme", "notes", "mutation", "r1", notesFP, `[{"op":"insert","table":"notes","value":{"title":"a","n":1}}]`)
	res := r.post(t, w, body, map[string]string{front.TraceHeader: "t-1", front.HandlerHeader: "addNote"})
	if res.status != 200 || res.header.Get(front.TraceHeader) != "t-1" {
		t.Fatalf("%d %s, trace %q", res.status, res.raw, res.header.Get(front.TraceHeader))
	}
	e := r.wait(t, o, 1)
	if e.TraceID != "t-1" || e.Handler != "addNote" || e.Method != "POST" || e.Path != front.OpsPath ||
		e.BytesIn != len(body) || e.BytesOut != len(res.raw) || e.Out.Status != 200 || e.Duration <= 0 {
		t.Errorf("event %+v", e)
	}
	ops := e.Ops
	if ops.Kind != ir.Mutation || ops.App != "notes" || ops.Tenant == "" || ops.Pipeline <= 0 || ops.Pipeline > e.Duration {
		t.Errorf("ops %+v", ops)
	}
	if ops.Began.IsZero() || ops.Began.Before(ops.Submitted) || ops.Ended.Before(ops.Began) || len(ops.Statements) == 0 {
		t.Fatalf("transaction %+v", ops)
	}
	var insert bool
	for _, st := range ops.Statements {
		if strings.HasPrefix(st.SQL, "INSERT") && st.Rows == 1 && st.Binds > 0 && st.Err == nil {
			insert = true
		}
	}
	if !insert {
		t.Errorf("no INSERT statement recorded: %+v", ops.Statements)
	}

	// A bad trace header and no handler header: a generated trace id, echoed; the shim's route name.
	q := r.post(t, rd, request("acme", "notes", "query", "", notesFP, `[{"op":"get","table":"notes","id":"x"}]`),
		map[string]string{front.TraceHeader: "has space", front.HandlerHeader: "bad name!"})
	e = r.wait(t, o, 2)
	if q.status != 200 || e.TraceID == "" || e.TraceID == "has space" || q.header.Get(front.TraceHeader) != e.TraceID ||
		e.Handler != front.OpsName || e.Ops.Kind != ir.Query {
		t.Errorf("query: %d trace %q (echoed %q) handler %q kind %q", q.status, e.TraceID, q.header.Get(front.TraceHeader), e.Handler, e.Ops.Kind)
	}

	// A duplicate mutation never runs its closure; a refused request never decodes.
	r.post(t, w, body, nil)
	if e = r.wait(t, o, 3); !e.Ops.Began.IsZero() || e.Ops.Kind != ir.Mutation {
		t.Errorf("duplicate: %+v", e.Ops)
	}
	bad := r.post(t, "", []byte(`{`), nil)
	if e = r.wait(t, o, 4); bad.status != 401 || e.Out.Status != 401 || e.Ops.Kind != "" || e.Out.Tag == "" {
		t.Errorf("refused: %d %+v", bad.status, e)
	}
}

// wait returns the n-th event: the observer runs after the answer is written, so it may trail the reply.
func (r *opsRig) wait(t *testing.T, o *observed, n int) front.OpsEvent {
	t.Helper()
	for range 2000 {
		o.mu.Lock()
		got := len(o.ev)
		o.mu.Unlock()
		if got >= n {
			return o.last(t)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%d events observed, want %d", len(o.ev), n)
	return front.OpsEvent{}
}
