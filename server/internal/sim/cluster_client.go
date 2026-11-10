package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"time"

	"github.com/nickstrad/mansio/server/internal/control/locator"
	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/linearize"
	"github.com/nickstrad/mansio/server/internal/schemareg"
)

// The workload: each database holds registers, bank accounts and a log. Every mutation also inserts one log
// document {rid: its request id} with a server-drawn id, so a database file shows which mutations it holds
// and how often (a mutation applied twice leaves two). Clients run register writes and reads, bank transfers
// (read two accounts, then patch both with expect on the versions read) and full bank reads, through the
// edge, with routes and tickets from the locator, retrying like the SDK (internal/client's rules, the
// Jepsen run's send): the same request id on every attempt.

// clusterSchema is the app schema every database is created with (fingerprint form: tables and fields sorted).
const clusterSchema = `{"accounts":{"fields":{"balance":{"node":{"kind":"integer"}}},"storage":"columns","indexes":[]},` +
	`"log":{"fields":{"rid":{"node":{"kind":"string"}}},"storage":"columns","indexes":[]},` +
	`"registers":{"fields":{"value":{"node":{"kind":"integer"}}},"storage":"columns","indexes":[]}}`

var clusterFingerprint = schemareg.Fingerprint([]byte(clusterSchema))

// simSchemas is the agents' schema registry: one app schema for every database.
type simSchemas struct{}

func (simSchemas) Schema(_ context.Context, _, fingerprint string) (string, error) {
	if fingerprint != clusterFingerprint {
		return "", front.ErrSchemaUnknown
	}
	return clusterSchema, nil
}

func docID(kind, i int) string { return fmt.Sprintf("01900000-0000-7000-8000-%04d%08d", kind, i) }
func regID(i int) string       { return docID(1, i) }
func acctID(i int) string      { return docID(2, i) }

func logOp(rid string) map[string]any {
	return map[string]any{"op": "insert", "table": "log", "value": map[string]any{"rid": rid}}
}

func setupOps(rid string) []map[string]any {
	var ops []map[string]any
	for i := range nRegs {
		ops = append(ops, map[string]any{"op": "insert", "table": "registers", "id": regID(i), "value": map[string]any{"value": 0}})
	}
	for i := range nAccts {
		ops = append(ops, map[string]any{"op": "insert", "table": "accounts", "id": acctID(i), "value": map[string]any{"balance": balance0}})
	}
	return append(ops, logOp(rid))
}

// cclient is one simulated SDK client of one database: its own stream, route cache and request ids.
type cclient struct {
	w     *cworld
	db    *cdb
	i     int
	r     *rand.Rand
	name  string // trace stream
	route *croute
	n     int // operations sent
}

type croute struct {
	ticket  string
	vm      string
	epoch   uint64
	refresh time.Time
}

func newClient(w *cworld, db *cdb, i int) *cclient {
	name := fmt.Sprintf("c/%s/%d", db.key, i)
	return &cclient{w: w, db: db, i: i, r: stream(w.s.Seed, name), name: name}
}

// canswer is a mutation's or query's final outcome as the client sees it.
type canswer struct {
	result  []json.RawMessage
	tag     string // "" when ok
	unknown bool   // a mutation that may have applied (some attempt had no answer, or an ambiguous one)
}

func (a canswer) ok() bool { return a.tag == "" && !a.unknown && a.result != nil }

// resolve asks the locator (controld, over the network) for a route.
func (c *cclient) resolve(ctx context.Context, minEpoch uint64) (*croute, time.Duration, error) {
	w := c.w
	cctx, cancel := det.WithTimeout(ctx, w.clk, 3*time.Second)
	defer cancel()
	v, err := w.net.call(cctx, c.r, nodeClients, nodeControld, nil, nil, func(sctx context.Context) any {
		rt, err := w.loc.Resolve(sctx, locator.Request{Tenant: tenant, Name: c.db.name, App: true, APIKey: w.apiKey, Source: "10.0.0.1:1", MinEpoch: minEpoch})
		return callAnswer{rt, err}
	})
	if err != nil {
		return nil, 0, err
	}
	a := v.(callAnswer)
	if a.err != nil {
		var hint interface{ RetryAfter() time.Duration }
		wait := time.Duration(0)
		if errors.As(a.err, &hint) {
			wait = hint.RetryAfter()
		}
		return nil, wait, a.err
	}
	rt := a.v.(locator.Route)
	return &croute{ticket: rt.Ticket, vm: rt.VM, epoch: rt.Epoch, refresh: w.clk.Now().Add(rt.TTL * 8 / 10)}, 0, nil
}

const maxAttempts = 12

// send runs one request until a final answer: ok, a definite failure (a protocol error such as
// ConflictError), or maxAttempts / the ctx's end. Retries reuse rid. Reroutes drop the route (min_epoch after
// wrong_owner or fenced).
func (c *cclient) send(ctx context.Context, mutation bool, rid string, ops []map[string]any) canswer {
	w := c.w
	kind := "query"
	body := map[string]any{"v": 1, "db": map[string]string{"tenant": tenant, "app": c.db.name}, "schema": clusterFingerprint, "ops": ops}
	if mutation {
		kind, body["requestId"] = "mutation", rid
	}
	body["kind"] = kind
	payload := []byte(mustJSON(body))
	maybe := false
	judge := func(tag string) { // the checker hears every answer to a mutation
		if mutation {
			w.model.answer(c.db.key, rid, tag)
		}
	}
	var minEpoch uint64
	what := kind
	if mutation {
		what = rid
	}
	for attempt := 1; ; attempt++ {
		if attempt > 1 {
			d := 20 * time.Millisecond << min(attempt-2, 6)
			if det.Sleep(ctx, w.clk, d/2+time.Duration(c.r.Int64N(int64(d/2)+1))) != nil {
				return canswer{tag: "gave-up", unknown: maybe}
			}
		}
		if ctx.Err() != nil || attempt > maxAttempts {
			return canswer{tag: "gave-up", unknown: maybe}
		}
		if c.route == nil || minEpoch > 0 || !w.clk.Now().Before(c.route.refresh) {
			rt, wait, err := c.resolve(ctx, minEpoch)
			if err != nil {
				w.tr.add(c.name, "%s try %d resolve -> %s", what, attempt, resolveClass(err))
				w.answered("resolve:" + resolveClass(err))
				if wait > 0 {
					det.Sleep(ctx, w.clk, wait)
				}
				continue
			}
			c.route, minEpoch = rt, 0
		}
		rt := c.route
		cctx, cancel := det.WithTimeout(ctx, w.clk, 2*time.Second)
		var dup func()
		if mutation {
			w.model.attempt(c.db.key, rid)
			dup = func() { w.model.duplicated(c.db.key, rid) }
		}
		v, err := w.net.call(cctx, c.r, nodeClients, nodeEdge, nil, dup, func(sctx context.Context) any {
			h := http.Header{"Authorization": {"Bearer " + rt.ticket}, "Content-Type": {"application/json"}, front.TimeoutHeader: {"1800"}}
			return serveHTTP(sctx, w.edge, http.MethodPost, "http://edge"+front.OpsPath, h, payload)
		})
		cancel()
		if err != nil {
			w.tr.add(c.name, "%s try %d -> no answer", what, attempt)
			w.answered("no-answer")
			maybe = maybe || mutation
			judge("no-answer")
			continue
		}
		a := v.(httpAnswer)
		var out struct {
			Result    []json.RawMessage `json:"result"`
			Duplicate bool              `json:"duplicate"`
			Error     *struct {
				Tag    string         `json:"tag"`
				Fields map[string]any `json:"fields"`
			} `json:"error"`
		}
		if json.Unmarshal(a.body, &out) != nil || out.Error == nil && a.status != 200 {
			w.tr.add(c.name, "%s try %d -> unreadable %d", what, attempt, a.status)
			w.answered("unreadable")
			maybe = maybe || mutation
			judge("unreadable")
			continue
		}
		if out.Error == nil {
			w.tr.add(c.name, "%s try %d -> ok%s at %s e%d", what, attempt, flag(out.Duplicate, " dup"), rt.vm, rt.epoch)
			w.answered("ok")
			judge(okTag(out.Duplicate))
			return canswer{result: out.Result}
		}
		tag := out.Error.Tag
		amb, _ := out.Error.Fields["ambiguous"].(bool)
		w.tr.add(c.name, "%s try %d -> %s%s at %s e%d", what, attempt, tag, flag(amb, " ambiguous"), rt.vm, rt.epoch)
		w.answered(tag)
		if amb {
			judge("expired-ambiguous")
		} else {
			judge(tag)
		}
		switch tag {
		case "wrong_owner", "fenced":
			c.route, minEpoch = nil, rt.epoch+1
		case "ticket_invalid":
			c.route = nil
		case "expired":
			maybe = maybe || mutation && amb
		case "shed", "unavailable", "aborted":
		case "Defect", "StorageError":
			maybe = maybe || mutation // a 500: the outcome is not known
		default:
			return canswer{tag: tag, unknown: maybe}
		}
	}
}

func okTag(dup bool) string {
	if dup {
		return "ok-dup"
	}
	return "ok"
}

func resolveClass(err error) string {
	for _, s := range []struct {
		err  error
		name string
	}{{locator.ErrMoving, "moving"}, {locator.ErrNotFound, "not-found"}, {locator.ErrSuspended, "suspended"},
		{locator.ErrUnauthorized, "unauthorized"}, {errNoAnswer, "no-answer"}, {errPGDown, "pg-down"}} {
		if errors.Is(err, s.err) {
			return s.name
		}
	}
	return "error"
}

func (w *cworld) answered(tag string) {
	w.count(func(s *ClusterStats) {
		if s.Answers == nil {
			s.Answers = map[string]int{}
		}
		s.Answers[tag]++
		s.Requests++
		if tag == "ok" {
			s.Acked++
		}
	})
}

// --- Operations and their histories -----------------------------------------------------------------------------

// run is the client's loop: think, pick an operation, run it, record it.
func (c *cclient) run(ctx context.Context) {
	w := c.w
	for {
		if det.Sleep(ctx, w.clk, exp(c.r, w.s.think)) != nil {
			return
		}
		c.n++
		switch x := c.r.IntN(20); {
		case x < 8:
			c.writeRegister(ctx)
		case x < 13:
			c.readRegister(ctx)
		case x < 18:
			c.transfer(ctx)
		default:
			c.readAll(ctx)
		}
	}
}

func (c *cclient) now() int64 { return int64(c.w.clk.Now().Sub(c.w.t0)) }

// opCtx bounds one operation: 10 s, ending early only when the run stops.
func (c *cclient) opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return det.WithTimeout(ctx, c.w.clk, 10*time.Second)
}

func (c *cclient) rid() string { return fmt.Sprintf("r-%s-%d-%d", c.db.name, c.i, c.n) }

func (c *cclient) writeRegister(ctx context.Context) {
	k := c.r.IntN(nRegs)
	v := int64(c.i+1)<<32 | int64(c.n) // unique per database
	rid := c.rid()
	call := c.now()
	octx, cancel := c.opCtx(ctx)
	a := c.send(octx, true, rid, []map[string]any{
		{"op": "patch", "table": "registers", "id": regID(k), "value": map[string]any{"value": v}}, logOp(rid)})
	cancel()
	op := linearize.Op[regIn, regOut]{Call: call, Return: c.now(), Input: regIn{key: k, write: true, value: v, rid: rid}}
	switch {
	case a.ok():
	case a.unknown || c.w.model.uncertain(c.db.key, rid):
		op.Return = linearize.Unknown
	default:
		return // definitely not applied
	}
	c.w.model.regOp(c.db.key, op)
}

func (c *cclient) readRegister(ctx context.Context) {
	k := c.r.IntN(nRegs)
	call := c.now()
	octx, cancel := c.opCtx(ctx)
	a := c.send(octx, false, "", []map[string]any{{"op": "get", "table": "registers", "id": regID(k)}})
	cancel()
	if !a.ok() || len(a.result) != 1 {
		return
	}
	var doc struct{ Value int64 }
	if err := json.Unmarshal(a.result[0], &doc); err != nil {
		c.w.model.violateNow(c.db.key, "register read answered %s: %v", a.result[0], err)
		return
	}
	c.w.model.regOp(c.db.key, linearize.Op[regIn, regOut]{Call: call, Return: c.now(), Input: regIn{key: k}, Output: regOut{doc.Value}})
}

// readAccounts reads accounts in one query, records the read, and answers what it saw.
func (c *cclient) readAccounts(ctx context.Context, accounts []int) (bankOut, bool) {
	ops := make([]map[string]any, len(accounts))
	for i, a := range accounts {
		ops[i] = map[string]any{"op": "get", "table": "accounts", "id": acctID(a)}
	}
	call := c.now()
	octx, cancel := c.opCtx(ctx)
	a := c.send(octx, false, "", ops)
	cancel()
	if !a.ok() || len(a.result) != len(accounts) {
		return bankOut{}, false
	}
	var out bankOut
	for _, raw := range a.result {
		var doc struct {
			Balance int64
			Version int64 `json:"_version"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			c.w.model.violateNow(c.db.key, "account read answered %s: %v", raw, err)
			return bankOut{}, false
		}
		out.balances, out.versions = append(out.balances, doc.Balance), append(out.versions, doc.Version)
	}
	c.w.model.bankOp(c.db.key, linearize.Op[bankIn, bankOut]{Call: call, Return: c.now(), Input: bankIn{accounts: slices.Clone(accounts)}, Output: out})
	return out, true
}

func (c *cclient) readAll(ctx context.Context) {
	all := make([]int, nAccts)
	for i := range all {
		all[i] = i
	}
	if out, ok := c.readAccounts(ctx, all); ok {
		var sum int64
		for _, b := range out.balances {
			sum += b
		}
		if sum != nAccts*balance0 {
			c.w.model.violateNow(c.db.key, "conservation: a full bank read sums to %d, want %d (balances %v versions %v)", sum, nAccts*balance0, out.balances, out.versions)
		}
	}
}

func (c *cclient) transfer(ctx context.Context) {
	from := c.r.IntN(nAccts)
	to := (from + 1 + c.r.IntN(nAccts-1)) % nAccts
	amount := int64(1 + c.r.IntN(10))
	seen, ok := c.readAccounts(ctx, []int{from, to})
	if !ok {
		return
	}
	in := bankIn{transfer: true, from: from, to: to, amount: amount, vFrom: seen.versions[0], vTo: seen.versions[1],
		newFrom: seen.balances[0] - amount, newTo: seen.balances[1] + amount}
	rid := c.rid()
	in.rid = rid
	ops := []map[string]any{
		{"op": "patch", "table": "accounts", "id": acctID(from), "value": map[string]any{"balance": in.newFrom}, "expect": map[string]any{"version": in.vFrom}},
		{"op": "patch", "table": "accounts", "id": acctID(to), "value": map[string]any{"balance": in.newTo}, "expect": map[string]any{"version": in.vTo}},
		logOp(rid),
	}
	call := c.now()
	octx, cancel := c.opCtx(ctx)
	a := c.send(octx, true, rid, ops)
	cancel()
	op := linearize.Op[bankIn, bankOut]{Call: call, Return: c.now(), Input: in}
	switch {
	case a.ok():
		var got [2]struct {
			Version int64 `json:"_version"`
		}
		for i := range got {
			if len(a.result) != len(ops) || json.Unmarshal(a.result[i], &got[i]) != nil || got[i].Version == 0 {
				c.w.model.violateNow(c.db.key, "transfer %s answered %s: want a new _version for each of its two patches", rid, a.result)
				return
			}
		}
		op.Input.gotFrom, op.Input.gotTo = got[0].Version, got[1].Version
	case a.unknown || c.w.model.uncertain(c.db.key, rid):
		op.Return = linearize.Unknown
	default:
		return // a precondition failed, or a definite refusal: nothing applied
	}
	c.w.model.bankOp(c.db.key, op)
}
