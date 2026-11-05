package fronttest

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/nickstrad/ts-db-go/internal/front"
)

// The statements the contract suite uses. The fake executor understands exactly these; the real one runs them
// on a database created with KVSchema.
const (
	KVSchema = `CREATE TABLE kv (k INTEGER PRIMARY KEY, v TEXT NOT NULL)`
	GetSQL   = `SELECT k, v FROM kv WHERE k = ?`
	ScanSQL  = `SELECT k, v FROM kv ORDER BY k`
	PutSQL   = `INSERT INTO kv (k, v) VALUES (?, ?)`
	SlowSQL  = `WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 1000000000) SELECT COUNT(*) FROM c`
	BadSQL   = `SELECT nope FROM nowhere`
)

// KVRows is the suite database's initial content: k = 1..3, v = "v<k>".
var KVRows = [][]any{{int64(1), "v1"}, {int64(2), "v2"}, {int64(3), "v3"}}

// Executor is a fake executor over in-memory kv tables, one per database. Switches: Delay before answering,
// Block until the deadline (then ErrNoReply), Panic, Err (returned as is).
type Executor struct {
	mu    sync.Mutex
	dbs   map[string]map[int64]string
	Delay time.Duration
	Block bool
	Panic bool
	Err   error
	calls int
}

// NewExecutor serves the given databases, each starting with KVRows.
func NewExecutor(dbs ...string) *Executor {
	e := &Executor{dbs: map[string]map[int64]string{}}
	for _, db := range dbs {
		m := map[int64]string{}
		for _, r := range KVRows {
			m[r[0].(int64)] = r[1].(string)
		}
		e.dbs[db] = m
	}
	return e
}

// Calls counts Exec calls.
func (e *Executor) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// Set changes switches under the lock: fn receives the executor.
func (e *Executor) Set(fn func(*Executor)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	fn(e)
}

// Exec implements front.Executor.
func (e *Executor) Exec(ctx context.Context, c front.Call) (front.Reply, error) {
	e.mu.Lock()
	e.calls++
	delay, block, p, ferr := e.Delay, e.Block, e.Panic, e.Err
	e.mu.Unlock()
	if p {
		panic("fronttest: Executor panics on purpose")
	}
	if ferr != nil {
		return front.Reply{}, ferr
	}
	if block {
		<-time.After(time.Until(c.Deadline))
		return front.Reply{}, front.ErrNoReply
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	if !time.Now().Before(c.Deadline) {
		return front.Reply{}, front.ErrExpired
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	kv, ok := e.dbs[c.DB]
	if !ok {
		return front.Reply{}, front.ErrUnknownDB
	}
	cols := []string{"k", "v"}
	switch c.SQL {
	case GetSQL:
		k, _ := c.Args[0].(int64)
		if v, ok := kv[k]; ok {
			return front.Reply{Columns: cols, Rows: [][]any{{k, v}}, Count: 1}, nil
		}
		return front.Reply{Columns: cols, Count: 0}, nil
	case ScanSQL:
		keys := make([]int64, 0, len(kv))
		for k := range kv {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if len(keys) > c.MaxRows {
			return front.Reply{}, front.ErrTooManyRows
		}
		rows := make([][]any, 0, len(keys))
		for _, k := range keys {
			rows = append(rows, []any{k, kv[k]})
		}
		return front.Reply{Columns: cols, Rows: rows, Count: int64(len(rows))}, nil
	case PutSQL:
		if c.Kind != front.KindWrite {
			return front.Reply{}, fmt.Errorf("%w: write in a read", front.ErrStatement)
		}
		k, _ := c.Args[0].(int64)
		v, _ := c.Args[1].(string)
		if _, dup := kv[k]; dup {
			return front.Reply{}, fmt.Errorf("%w: UNIQUE constraint failed: kv.k", front.ErrStatement)
		}
		kv[k] = v
		return front.Reply{Count: 1}, nil
	case SlowSQL:
		e.mu.Unlock()
		time.Sleep(c.StmtTimeout)
		e.mu.Lock()
		return front.Reply{}, front.ErrStmtTimeout
	}
	return front.Reply{}, fmt.Errorf("%w: no such table", front.ErrStatement)
}
