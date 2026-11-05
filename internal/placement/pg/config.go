package pg

import (
	"context"
	"net"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nickstrad/ts-db-go/internal/breaker"
	"github.com/nickstrad/ts-db-go/internal/errs"
)

const module = "placement"

// Config is the adapter's configuration. The zero value of every field but DSN takes the Release It §P
// Postgres defaults.
type Config struct {
	// DSN is a postgres:// URL or key=value string. It holds credentials: load it from the secrets file, not
	// the config file.
	DSN string
	// AppName is the session's application_name (SQL G-5), e.g. "controld/placement". Default "placement".
	AppName string
	// Schema, if set, is the session's search_path: the schema the tables live in. Tests use a fresh schema
	// each; production leaves it empty (the server default).
	Schema string
	// MaxConns is the pool size = reconcile workers + lifecycle concurrency (RI §P, RI B2). Default 20
	// (owner.ReconcileOptions' 16 workers + 4).
	MaxConns int32

	ConnectTimeout   time.Duration // default 3 s
	StatementTimeout time.Duration // server-side, per statement; default 2 s
	LockTimeout      time.Duration // server-side, per lock wait; default 1 s
	IdleInTxTimeout  time.Duration // idle_in_transaction_session_timeout; default 5 s
	// CallTimeout bounds each call client-side (acquire + connect + round trip) when the network, not the
	// server, is what hangs (RI T1). Default StatementTimeout + 500 ms; must be > StatementTimeout so the
	// server's own timeout answers first when it can.
	CallTimeout time.Duration
	// MaxConnIdleTime closes idle connections before any NAT/LB idle timeout drops them silently (RI §P,
	// "5 a.m. problem"). Default 60 s.
	MaxConnIdleTime time.Duration
	MaxConnLifetime time.Duration // default 30 min
	// KeepAlive is the TCP keepalive idle time and probe interval; default 15 s (3 probes), so a dead peer
	// on an idle pooled connection is found in about 45 s.
	KeepAlive time.Duration

	// Breaker configures the pool's circuit breaker (zero value: RI §P defaults). Set Breaker.OnChange to
	// log and expose transitions as "placement" (RI C4).
	Breaker breaker.Config
	// Now is the breaker's clock; nil means time.Now.
	Now func() time.Time
}

var schemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Normalize fills in defaults and checks the config (the edge: it comes from a file). Errors are
// wrapped for module placement and match no sentinel.
func (c *Config) Normalize() error {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	if c.AppName == "" {
		c.AppName = "placement"
	}
	if c.MaxConns == 0 {
		c.MaxConns = 20
	}
	def(&c.ConnectTimeout, 3*time.Second)
	def(&c.StatementTimeout, 2*time.Second)
	def(&c.LockTimeout, time.Second)
	def(&c.IdleInTxTimeout, 5*time.Second)
	def(&c.CallTimeout, c.StatementTimeout+500*time.Millisecond)
	def(&c.MaxConnIdleTime, time.Minute)
	def(&c.MaxConnLifetime, 30*time.Minute)
	def(&c.KeepAlive, 15*time.Second)
	if c.Now == nil {
		c.Now = time.Now
	}
	switch {
	case c.DSN == "":
		return errs.Wrap(module, nil, "invalid placement config: dsn is empty")
	case c.Schema != "" && !schemaName.MatchString(c.Schema):
		return errs.Wrap(module, nil, "invalid placement config: schema %q must match %s", c.Schema, schemaName)
	case c.MaxConns < 1:
		return errs.Wrap(module, nil, "invalid placement config: max_conns %d < 1", c.MaxConns)
	case c.ConnectTimeout < 0, c.StatementTimeout < time.Millisecond, c.LockTimeout < time.Millisecond,
		c.IdleInTxTimeout < time.Millisecond, c.MaxConnIdleTime < 0, c.MaxConnLifetime < 0, c.KeepAlive < 0:
		return errs.Wrap(module, nil, "invalid placement config: timeouts must be positive (statement, lock and idle-in-transaction ≥ 1 ms)")
	case c.LockTimeout >= c.StatementTimeout:
		return errs.Wrap(module, nil, "invalid placement config: lock_timeout %v must be below statement_timeout %v", c.LockTimeout, c.StatementTimeout)
	case c.CallTimeout <= c.StatementTimeout:
		return errs.Wrap(module, nil, "invalid placement config: call_timeout %v must exceed statement_timeout %v", c.CallTimeout, c.StatementTimeout)
	}
	if err := c.Breaker.Normalize(); err != nil {
		return errs.Wrap(module, err, "invalid placement config: breaker")
	}
	return nil
}

// poolConfig is the one place the connection settings are built (SQL G-5, G-6).
//
//	pre: c was normalized
func (c *Config) poolConfig() (*pgxpool.Config, error) {
	pc, err := pgxpool.ParseConfig(c.DSN)
	if err != nil {
		// The parse error may quote the DSN, password included: do not wrap it.
		return nil, errs.Wrap(module, nil, "invalid placement config: dsn does not parse")
	}
	pc.MaxConns = c.MaxConns
	pc.MinConns = 0 // connect lazily: startup must not block on the authority (RI H1)
	pc.MaxConnIdleTime = c.MaxConnIdleTime
	pc.MaxConnLifetime = c.MaxConnLifetime
	pc.HealthCheckPeriod = min(c.MaxConnIdleTime/2, 30*time.Second)
	cc := pc.ConnConfig
	cc.ConnectTimeout = c.ConnectTimeout
	d := &net.Dialer{KeepAliveConfig: net.KeepAliveConfig{Enable: c.KeepAlive > 0, Idle: c.KeepAlive, Interval: c.KeepAlive, Count: 3}}
	cc.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		// pgx's cancel request dials with a 15 s budget of its own; ours is the connect timeout (RI T4).
		ctx, cancel := context.WithTimeout(ctx, c.ConnectTimeout)
		defer cancel()
		conn, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return boundedConn{conn}, nil
	}
	ms := func(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }
	rp := cc.RuntimeParams
	rp["application_name"] = c.AppName
	rp["statement_timeout"] = ms(c.StatementTimeout)
	rp["lock_timeout"] = ms(c.LockTimeout)
	rp["idle_in_transaction_session_timeout"] = ms(c.IdleInTxTimeout)
	rp["default_transaction_isolation"] = "read committed" // SQL G-9: a racing CAS re-checks and updates 0 rows
	rp["synchronous_commit"] = "on"                        // never lowered on the safety path, whatever the DSN or role says
	if c.Schema != "" {
		rp["search_path"] = c.Schema
	}
	if len(cc.Fallbacks) > 0 {
		// Fallback hosts may be replicas; the port needs the primary (linearizable Get).
		return nil, errs.Wrap(module, nil, "invalid placement config: dsn names %d fallback hosts; give the primary only", len(cc.Fallbacks))
	}
	return pc, nil
}

// closeBudget bounds how long closing a broken connection may block. pgx closes a connection whose call
// timed out by sending a cancel request and draining it under a fixed 15 s deadline; on a half-open
// connection the drain never ends, and the pool slot stays taken until it does (with MaxConns 1, every
// call fails for 15 s). Found by TestHalfOpenRecyclesTheStaleConnection.
const closeBudget = time.Second

// boundedConn caps every non-zero deadline pgx sets at closeBudget from now. pgx sets deadlines only to
// interrupt a call whose ctx ended (now) and to bound that close (now + 15 s); zero clears them.
type boundedConn struct{ net.Conn }

func (c boundedConn) SetDeadline(t time.Time) error {
	if limit := time.Now().Add(closeBudget); !t.IsZero() && t.After(limit) {
		t = limit
	}
	return c.Conn.SetDeadline(t)
}
