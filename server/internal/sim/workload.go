package sim

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/sqlitedsn"

	_ "modernc.org/sqlite" // the "sqlite" driver: the oracle reads database files with the same SQLite
)

// Kind is what a client request does.
type Kind uint8

const (
	Write    Kind = iota + 1 // applies a mark (its request id) once
	BadWrite                 // a write the database rejects (a statement error): must change nothing
	Read                     // returns every mark in commit order
)

func (k Kind) String() string {
	return [...]string{"?", "write", "badwrite", "read"}[k]
}

// Request is one client request. Writes carry the request id the agent dedupes on; that id is also the mark the
// write leaves in the database, so the checker can tell from a database file which writes it holds.
type Request struct {
	Kind  Kind
	ID    string // writes: request id and mark
	Value int64  // a payload the workload may store (SQL: the v column)
}

// Answer is the agent's answer as a client sees it on the wire.
type Answer struct {
	Code       front.Code
	Ambiguous  bool // an expired write that may have committed
	Duplicate  bool // a write whose request id was applied before
	RetryAfter time.Duration
	Marks      []string // reads: the database's marks in commit order
	Err        error    // the cause the agent logged (internal answers); not part of the trace
}

// Agent is what a workload may call on the incarnation under test.
type Agent interface {
	// Front is the request pipeline (today's raw-SQL path, the /v1/query body).
	Front() *front.Server
	// Ops is the same pipeline with the document protocol (/v1/ops, front.Server.HandleOps) enabled, when the
	// workload is a front.SchemaSource (IR is); nil otherwise.
	Ops() *front.Server
}

// Workload is the port between the simulated clients and the checker: how a database starts, how a request
// is sent, and how a database file is read back. SQL implements it for raw SQL; an IR workload implements it
// for document operations, with the request id as the mark (e.g. an `_id` or a field of the inserted document)
// and Observe listing the marks a document table holds in insertion order.
//
// Contract the checker relies on: a Write's mark appears in the database exactly when the write is applied, at
// most once per apply; a BadWrite never changes the database; marks list in commit order; nothing else writes
// marks.
type Workload interface {
	Name() string
	// Seed returns the bytes of key's initial database file (generation 1, published before the run).
	// dir is scratch space.
	Seed(dir, key string) ([]byte, error)
	// Send delivers r for database key to a and returns the answer. ctx ends when the incarnation dies.
	Send(ctx context.Context, a Agent, key string, r Request) Answer
	// Observe lists the marks in the database file at path, in commit order, duplicates included.
	Observe(path string) ([]string, error)
}

// SQL is the raw-SQL workload: one table of marks, written with INSERT (no unique constraint, so a write
// applied twice shows as two rows) through the front pipeline with request ids.
type SQL struct {
	// Timeout is each request's deadline (front.Input.Timeout). Default 2 s.
	Timeout time.Duration
}

const sqlSchema = `CREATE TABLE log (
	seq INTEGER PRIMARY KEY,
	req TEXT NOT NULL,
	v   INTEGER NOT NULL CHECK (v >= 0)
) STRICT`

func (SQL) Name() string { return "sql" }

func (SQL) Seed(dir, key string) ([]byte, error) {
	path, err := os.CreateTemp(dir, "seed-*.db")
	if err != nil {
		return nil, err
	}
	path.Close()
	defer os.Remove(path.Name())
	db, err := sql.Open("sqlite", sqlitedsn.Build(path.Name()))
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(sqlSchema)
	if cerr := db.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path.Name())
}

type sqlBody struct {
	DB        string `json:"db"`
	RequestID string `json:"request_id,omitempty"`
	Kind      string `json:"kind"`
	SQL       string `json:"sql"`
	Args      []any  `json:"args,omitempty"`
}

func (w SQL) Send(ctx context.Context, a Agent, key string, r Request) Answer {
	b := sqlBody{DB: key}
	switch r.Kind {
	case Write:
		b.RequestID, b.Kind, b.SQL, b.Args = r.ID, "write", `INSERT INTO log (req, v) VALUES (?, ?)`, []any{r.ID, r.Value}
	case BadWrite:
		b.RequestID, b.Kind, b.SQL, b.Args = r.ID, "write", `INSERT INTO log (req, v) VALUES (?, -1)`, []any{r.ID}
	case Read:
		b.Kind, b.SQL = "read", `SELECT req FROM log ORDER BY seq`
	}
	body, err := json.Marshal(b)
	if err != nil {
		panic(fmt.Sprintf("sim: cannot encode a request: %v", err)) // our own types: a bug
	}
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	out := a.Front().Handle(ctx, front.Input{Ticket: "sim", Body: body, Timeout: timeout})
	ans := Answer{Code: out.Resp.Code, Ambiguous: out.Resp.Ambiguous, Duplicate: out.Resp.Duplicate,
		RetryAfter: time.Duration(out.Resp.RetryAfterMS) * time.Millisecond}
	if ans.Code == front.Internal {
		ans.Err = out.Err
	}
	if r.Kind == Read && ans.Code == front.OK {
		for _, row := range out.Resp.Rows {
			s, _ := row[0].(string)
			ans.Marks = append(ans.Marks, s)
		}
	}
	return ans
}

func (SQL) Observe(path string) ([]string, error) {
	db, err := sql.Open("sqlite", sqlitedsn.Build(path, "query_only(1)"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT req FROM log ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var marks []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		marks = append(marks, m)
	}
	return marks, rows.Err()
}
