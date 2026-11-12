package sim

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/schemareg"
	"github.com/nickstrad/mansio/server/internal/sqlitedsn"
)

// IR is the document-protocol workload for the single-node sim: requests go to /v1/ops (front.Server.HandleOps) and
// run through docexec on the actor. A write inserts one log document {n, rid} (n: the database's write sequence,
// which is commit order with one sequential client; rid: the request id, the mark); a bad write inserts a document
// that fails validation (SchemaError, nothing ran); a read selects every log document in pages of 1000 in one
// snapshot. A database starts empty: its first request creates the tables from the registered schema (front's
// initDB). IR implements front.SchemaSource, which newAgent uses to enable /v1/ops.
type IR struct {
	Timeout time.Duration // each request's deadline; default 2 s

	mu  sync.Mutex
	seq map[string]map[string]int64 // key → request id → n (a retry sends the same body)
}

const irSchema = `{"log":{"fields":{"n":{"node":{"kind":"integer"}},"rid":{"node":{"kind":"string"}}},"storage":"columns","indexes":[]}}`

var irFingerprint = schemareg.Fingerprint([]byte(irSchema))

var _ front.SchemaSource = (*IR)(nil)

func (*IR) Name() string { return "ir" }

// Schema is the registry: one schema for every app.
func (*IR) Schema(_ context.Context, _, fingerprint string) (string, error) {
	if fingerprint != irFingerprint {
		return "", front.ErrSchemaUnknown
	}
	return irSchema, nil
}

// Seed returns an empty database file (no tables yet).
func (*IR) Seed(dir, _ string) ([]byte, error) {
	f, err := os.CreateTemp(dir, "seed-*.db")
	if err != nil {
		return nil, err
	}
	f.Close()
	defer os.Remove(f.Name())
	db, err := sql.Open("sqlite", sqlitedsn.Build(f.Name()))
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`PRAGMA user_version = 1`)
	if cerr := db.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	return os.ReadFile(f.Name())
}

// n is rid's sequence number in key (assigned on first send).
func (w *IR) n(key, rid string) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seq == nil {
		w.seq = map[string]map[string]int64{}
	}
	m := w.seq[key]
	if m == nil {
		m = map[string]int64{}
		w.seq[key] = m
	}
	if n, ok := m[rid]; ok {
		return n
	}
	m[rid] = int64(len(m) + 1)
	return m[rid]
}

const irPages = 64 // a read covers n up to 64 000

func (w *IR) Send(ctx context.Context, a Agent, key string, r Request) Answer {
	tenant, app, _ := strings.Cut(key, ":")
	body := map[string]any{"v": 1, "db": map[string]string{"tenant": tenant, "app": app}, "schema": irFingerprint}
	switch r.Kind {
	case Write:
		body["kind"], body["requestId"] = "mutation", r.ID
		body["ops"] = []map[string]any{{"op": "insert", "table": "log", "value": map[string]any{"n": w.n(key, r.ID), "rid": r.ID}}}
	case BadWrite:
		body["kind"], body["requestId"] = "mutation", r.ID
		body["ops"] = []map[string]any{{"op": "insert", "table": "log", "value": map[string]any{"n": "not a number", "rid": r.ID}}}
	case Read:
		ops := make([]map[string]any, irPages)
		for p := range ops {
			ops[p] = map[string]any{"op": "select", "table": "log", "filters": []map[string]any{{"field": "n", "op": "gt", "value": p * 1000}},
				"order": map[string]any{"field": "n", "direction": "asc"}, "limit": 1000}
		}
		body["kind"], body["ops"] = "query", ops
	}
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	out := a.Ops().HandleOps(ctx, front.Input{Ticket: "sim", Body: []byte(mustJSON(body)), Timeout: timeout})
	ans := Answer{Code: out.Code, Ambiguous: out.Ambiguous, RetryAfter: time.Duration(out.RetryAfter) * time.Second}
	if out.Code == front.Internal {
		ans.Err = fmt.Errorf("%s: %w", out.Tag, out.Err)
	}
	if out.Code != front.OK {
		return ans
	}
	var res struct {
		Result    []json.RawMessage `json:"result"`
		Duplicate bool              `json:"duplicate"`
	}
	if err := json.Unmarshal(out.Body, &res); err != nil {
		return Answer{Code: front.Internal, Err: fmt.Errorf("sim: unreadable ok answer %s: %w", out.Body, err)}
	}
	ans.Duplicate = res.Duplicate
	if r.Kind == Read {
		for p, page := range res.Result {
			var docs []struct {
				N   int64  `json:"n"`
				Rid string `json:"rid"`
			}
			if err := json.Unmarshal(page, &docs); err != nil {
				return Answer{Code: front.Internal, Err: fmt.Errorf("sim: unreadable page %d: %w", p, err)}
			}
			for _, d := range docs { // page p holds n in (p·1000, …]; keep its own thousand
				if d.N <= int64(p+1)*1000 {
					ans.Marks = append(ans.Marks, d.Rid)
				}
			}
		}
	}
	return ans
}

// Observe lists the log's marks in a database file in insertion order (none before its tables exist).
func (*IR) Observe(path string) ([]string, error) { return observeLog(path) }

// observeLog lists the log table's marks in a database file, in insertion order (empty before the database
// has its tables).
func observeLog(path string) ([]string, error) {
	db, err := sql.Open("sqlite", sqlitedsn.Build(path, "query_only(1)"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'log'`).Scan(&n); err != nil || n == 0 {
		return nil, err
	}
	rows, err := db.Query(`SELECT rid FROM "log" ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var marks []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		marks = append(marks, s)
	}
	return marks, rows.Err()
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("sim: cannot encode %T: %v", v, err))
	}
	return string(b)
}
