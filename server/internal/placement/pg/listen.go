package pg

import (
	"context"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nickstrad/ts-db-go/internal/errs"
)

// The notification channels: Channel (0003_notify.sql) carries database_id ':' epoch for every placement change;
// StateChannel (0005_state_notify.sql) carries database_id for every change of a database's registry state.
const (
	Channel      = "placement"
	StateChannel = "database_state"
)

// ListenHooks receive a listener's events, in order, from one goroutine.
type ListenHooks struct {
	// Ready runs after every successful LISTEN (the first and each reconnect), before any notification of that
	// session is delivered: drop or reload whatever was cached (LISTEN first, then load, then apply; ch 40).
	Ready func()
	// Notify is one change: key's row changed and is now at epoch.
	Notify func(key string, epoch uint64)
	// State is one registry change: key's lifecycle state (or move target) changed. Nil: not listened to.
	State func(key string)
	// Lost runs when the session fails: notifications may be missed until the next Ready.
	Lost func(err error)
}

// ListenConfig tunes Listen. Zero fields take the defaults.
type ListenConfig struct {
	// Ping is how long the connection may stay silent before a "select 1" checks it (a half-open connection
	// is otherwise only found by TCP keepalive). Default 5 s.
	Ping time.Duration
	// MaxBackoff caps the jittered exponential reconnect backoff (100 ms ×2). Default 5 s.
	MaxBackoff time.Duration
}

// Listen keeps one dedicated connection (not from the pool) LISTENing on Channel (and StateChannel when h.State
// is set) until ctx ends, reconnecting with jittered backoff after any failure. Its application_name is AppName + "/listen". Payloads that do not
// parse are skipped (they are not ours). It returns when ctx ends.
func (p *Placement) Listen(ctx context.Context, cfg ListenConfig, h ListenHooks) {
	if cfg.Ping <= 0 {
		cfg.Ping = 5 * time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 5 * time.Second
	}
	pc, err := p.cfg.poolConfig()
	if err != nil { // Open already built it once: unreachable
		h.Lost(err)
		return
	}
	cc := pc.ConnConfig
	cc.RuntimeParams["application_name"] = p.cfg.AppName + "/listen"
	backoff := 100 * time.Millisecond
	for ctx.Err() == nil {
		began := time.Now()
		err := p.listenOnce(ctx, cc.Copy(), cfg, h)
		if ctx.Err() != nil {
			return
		}
		h.Lost(errs.Wrap(module, err, "placement listener lost; notifications may be missed until it reconnects"))
		if time.Since(began) > 10*cfg.Ping {
			backoff = 100 * time.Millisecond // a session that lived a while starts the backoff over
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff/2 + rand.N(backoff/2+1)):
		}
		backoff = min(2*backoff, cfg.MaxBackoff)
	}
}

// listenOnce runs one session: connect, LISTEN, Ready, then deliver notifications until the connection fails.
func (p *Placement) listenOnce(ctx context.Context, cc *pgx.ConnConfig, cfg ListenConfig, h ListenHooks) error {
	cctx, cancel := context.WithTimeout(ctx, p.cfg.ConnectTimeout+p.cfg.CallTimeout)
	conn, err := pgx.ConnectConfig(cctx, cc)
	cancel()
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), closeBudget)
		conn.Close(closeCtx)
		cancel()
	}()
	if err := p.exec(ctx, conn, "listen "+Channel); err != nil {
		return err
	}
	if h.State != nil {
		if err := p.exec(ctx, conn, "listen "+StateChannel); err != nil {
			return err
		}
	}
	h.Ready()
	for {
		wctx, cancel := context.WithTimeout(ctx, cfg.Ping)
		n, err := conn.WaitForNotification(wctx)
		silent := wctx.Err() != nil
		cancel()
		switch {
		case ctx.Err() != nil:
			return nil
		case err == nil && n.Channel == Channel:
			if key, epoch, ok := parsePayload(n.Payload); ok {
				h.Notify(key, epoch)
			}
		case err == nil && n.Channel == StateChannel && n.Payload != "": // LISTENed only when h.State is set
			h.State(n.Payload)
		case err == nil: // another channel: not ours
		case silent && !conn.IsClosed():
			if err := p.exec(ctx, conn, "select 1"); err != nil {
				return err
			}
		default:
			return err
		}
	}
}

// exec runs one statement on conn under the call deadline.
func (p *Placement) exec(ctx context.Context, conn *pgx.Conn, sql string) error {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.CallTimeout)
	defer cancel()
	_, err := conn.Exec(ctx, sql)
	return err
}

// parsePayload splits "database_id:epoch" at the last colon (keys contain colons: tenant:name).
func parsePayload(s string) (key string, epoch uint64, ok bool) {
	i := strings.LastIndexByte(s, ':')
	if i <= 0 {
		return "", 0, false
	}
	e, err := strconv.ParseUint(s[i+1:], 10, 64)
	if err != nil || e == 0 {
		return "", 0, false
	}
	return s[:i], e, true
}
