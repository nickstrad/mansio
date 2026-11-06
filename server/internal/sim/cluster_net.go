package sim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickstrad/mansio/server/internal/det"
)

// The simulated network. Every call between two nodes (a client, the edge, controld, an agent) is two
// messages, each on the clock: the request (delay; maybe lost; maybe delivered twice) and the answer (delay;
// maybe lost). A message between two nodes a partition separates is lost; a request to a node that is down
// is refused (an answer that nothing ran). Delays are drawn per call from the caller's stream, so reordering
// comes from the delays, and every draw is fixed by the caller's own sequence of calls.
//
// Delivery does not depend on the caller still waiting: a request whose caller gave up is still delivered
// (and run), as on a real network; the late answer is dropped. The handler runs on the receiving node with
// that node's lifetime (an agent's context ends at its crash), never the caller's.

var (
	errNoAnswer = errors.New("sim: no answer (lost on the network, or the caller gave up)")
	errRefused  = errors.New("sim: connection refused (the node is down; nothing ran)")
)

// Node names. Agents are "vm1".."vmN"; the clients share one node for partitions.
const (
	nodeClients  = "clients"
	nodeEdge     = "edge"
	nodeControld = "controld"
	nodePG       = "pg" // the placement authority (Postgres)
	nodeS3       = "s3"
)

type netw struct {
	w  *cworld
	mu sync.Mutex
	// cut holds the pairs a partition separates (both orders), with how many partitions cut them.
	cut     map[[2]string]int
	packets sync.WaitGroup // messages in flight (the run ends only when every one has landed)
}

func newNet(w *cworld) *netw { return &netw{w: w, cut: map[[2]string]int{}} }

// blocked reports whether a partition separates a and b now.
func (n *netw) blocked(a, b string) bool {
	if a == b {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.cut[[2]string{a, b}] > 0
}

// partition cuts node off from each of others until heal is called.
func (n *netw) partition(node string, others []string) (heal func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, o := range others {
		n.cut[[2]string{node, o}]++
		n.cut[[2]string{o, node}]++
	}
	return func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		for _, o := range others {
			n.cut[[2]string{node, o}]--
			n.cut[[2]string{o, node}]--
		}
	}
}

// draw is one call's network fate, drawn up front (always the same number of draws).
type draw struct {
	out, back    time.Duration
	dropOut, dup bool
	dropBack     bool
	dupLag       time.Duration
	nonce        uint64 // seeds the handler's own stream (any calls it makes)
}

func (n *netw) draw(r *rand.Rand) draw {
	f := n.w.s.faults
	on := n.w.faultsOn.Load()
	lat := func() time.Duration {
		d := 200*time.Microsecond + time.Duration(r.Int64N(int64(5*time.Millisecond)))
		s, x := r.Float64(), r.Int64N(1<<40)
		if on && s < f.Spike && f.SpikeMax > 0 {
			d += time.Duration(x % int64(f.SpikeMax))
		}
		return d
	}
	var d draw
	d.out, d.back = lat(), lat()
	u1, u2, u3 := r.Float64(), r.Float64(), r.Float64()
	d.dupLag = time.Duration(r.Int64N(int64(50 * time.Millisecond)))
	d.nonce = r.Uint64()
	if on {
		d.dropOut, d.dup, d.dropBack = u1 < f.Drop, u2 < f.Duplicate, u3 < f.Drop
	}
	return d
}

type reply struct {
	v   any
	err error
}

// call sends one request from → to and waits for the answer or ctx's end. serve runs on to's side with to's
// context (to down: refused). sent, if set, is called when the request leaves (it may then run); dup, if set,
// when the network is going to deliver it twice (the checker then knows a "nothing ran" answer proves nothing).
//
//	post: err == errRefused ⇒ the request never ran; errors.Is(err, errNoAnswer) ⇒ it may have run (even twice)
func (n *netw) call(ctx context.Context, r *rand.Rand, from, to string, sent, dup func(), serve func(ctx context.Context) any) (any, error) {
	d := n.draw(r)
	done := make(chan reply, 1)
	clk := n.w.clk
	deliver := func(copy bool) {
		defer n.packets.Done()
		lag := d.out
		if copy {
			lag += d.dupLag
		}
		if det.Sleep(context.Background(), clk, lag) != nil || d.dropOut && !copy || n.blocked(from, to) {
			return
		}
		var rep reply
		if sctx := n.w.nodeContext(to); sctx == nil {
			rep.err = errRefused
		} else {
			nonce := d.nonce
			if copy {
				nonce ^= 1
			}
			rep.v = serve(withRand(sctx, rand.New(det.NewRand(nonce)))) // the handler's own stream
			if sctx.Err() != nil {
				return // the node died while serving (kill -9): its answer never leaves
			}
		}
		if copy {
			return
		}
		if det.Sleep(context.Background(), clk, d.back) != nil || d.dropBack || n.blocked(to, from) {
			return
		}
		done <- rep
	}
	if sent != nil {
		sent()
	}
	n.packets.Add(1)
	go deliver(false)
	if d.dup {
		if dup != nil {
			dup()
		}
		n.packets.Add(1)
		go deliver(true)
	}
	select {
	case rep := <-done:
		return rep.v, rep.err
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", errNoAnswer, ctx.Err())
	}
}

// httpAnswer is a handler's HTTP answer carried back over the network.
type httpAnswer struct {
	status int
	header http.Header
	body   []byte
}

// serveHTTP runs h on one request and records the answer.
func serveHTTP(ctx context.Context, h http.Handler, method, url string, header http.Header, body []byte) httpAnswer {
	req := httptest.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	req.Header = header.Clone()
	req.RemoteAddr = "10.0.0.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return httpAnswer{status: rec.Code, header: rec.Header().Clone(), body: rec.Body.Bytes()}
}

// transport is an http.RoundTripper from one node over the simulated network to the agents' query handlers
// (the edge's Transport). A URL's host names the agent.
type transport struct {
	n    *netw
	from string
}

func (t transport) RoundTrip(req *http.Request) (*http.Response, error) {
	to, _, _ := strings.Cut(req.URL.Host, ":")
	var body []byte
	if req.Body != nil {
		var err error
		if body, err = io.ReadAll(req.Body); err != nil {
			return nil, err
		}
		req.Body.Close()
	}
	r := ctxRand(req.Context())
	if r == nil {
		panic("sim: an edge call outside a simulated handler (no call stream)")
	}
	trace := httptrace.ContextClientTrace(req.Context())
	sent := func() {
		if t.n.w.nodeContext(to) != nil && trace != nil && trace.WroteRequest != nil {
			trace.WroteRequest(httptrace.WroteRequestInfo{})
		}
	}
	header, url := req.Header.Clone(), req.URL.String()
	dup := func() {
		var b struct {
			DB        struct{ Tenant, App string }
			RequestID string `json:"requestId"`
		}
		if json.Unmarshal(body, &b) == nil && b.RequestID != "" {
			t.n.w.model.duplicated(b.DB.Tenant+":"+b.DB.App, b.RequestID)
		}
	}
	v, err := t.n.call(req.Context(), r, t.from, to, sent, dup, func(ctx context.Context) any {
		h := t.n.w.agentHandler(to)
		if h == nil {
			return httpAnswer{}
		}
		return serveHTTP(ctx, h, http.MethodPost, url, header, body)
	})
	if err != nil {
		return nil, err
	}
	a := v.(httpAnswer)
	if a.status == 0 {
		return nil, errRefused
	}
	return &http.Response{StatusCode: a.status, Header: a.header, Body: io.NopCloser(bytes.NewReader(a.body)), Request: req,
		ContentLength: int64(len(a.body))}, nil
}

// nodesExcept lists every partitionable node but node, sorted.
func (w *cworld) nodesExcept(node string) []string {
	all := append([]string{nodeClients, nodeEdge, nodeControld, nodePG, nodeS3}, w.vms...)
	all = slices.DeleteFunc(all, func(s string) bool { return s == node })
	slices.Sort(all)
	return all
}
