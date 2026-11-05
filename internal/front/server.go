package front

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/ts-db-go/internal/assert"
	"github.com/nickstrad/ts-db-go/internal/metrics"
	"github.com/nickstrad/ts-db-go/internal/panicx"
	"github.com/nickstrad/ts-db-go/internal/ticket"
)

// TicketVerifier is satisfied by ticket.Verifier and ticket.Cache.
type TicketVerifier interface {
	Verify(token string, now time.Time, want ticket.Want) (ticket.Claims, error)
}

// Config sets the pipeline's limits. Zero fields take defaults (WithDefaults).
type Config struct {
	Limits         Limits
	Admission      AdmissionLimits
	DefaultTimeout time.Duration // request deadline when the client sends none; default 2 s
	MaxTimeout     time.Duration // longest deadline a client may ask for; default 30 s
	StmtTimeout    time.Duration // per statement, also capped by the deadline; default 1 s

	// HTTP transport only (Release It T3, §P inbound HTTP). BodyReadTimeout bounds reading the request body, so a
	// client cannot dribble it forever; default 5 s. WriteGrace is how long past the request deadline the answer
	// may take to write, so a client that stops reading cannot hold a handler; it must exceed the executor's
	// reply grace; default 2 s.
	BodyReadTimeout time.Duration
	WriteGrace      time.Duration
}

// WithDefaults fills zero fields.
func (c Config) WithDefaults() Config {
	c.Limits = c.Limits.WithDefaults()
	c.Admission = c.Admission.WithDefaults()
	if c.DefaultTimeout <= 0 {
		c.DefaultTimeout = 2 * time.Second
	}
	if c.MaxTimeout <= 0 {
		c.MaxTimeout = 30 * time.Second
	}
	c.DefaultTimeout = min(c.DefaultTimeout, c.MaxTimeout)
	if c.StmtTimeout <= 0 {
		c.StmtTimeout = time.Second
	}
	if c.BodyReadTimeout <= 0 {
		c.BodyReadTimeout = 5 * time.Second
	}
	if c.WriteGrace <= 0 {
		c.WriteGrace = 2 * time.Second
	}
	return c
}

// timeout is the request's budget: the client's (0 = DefaultTimeout), capped at MaxTimeout.
func (c Config) timeout(client time.Duration) time.Duration {
	if client <= 0 {
		return c.DefaultTimeout
	}
	return min(client, c.MaxTimeout)
}

// Server is the request pipeline, independent of the transport.
type Server struct {
	cfg      Config
	tickets  TicketVerifier
	admitter Admitter
	adm      *Admission
	exec     Executor
	clock    Clock

	mu         sync.Mutex
	draining   bool
	inFlight   int
	inFlightHW int           // highest inFlight since NewServer
	idle       chan struct{} // closed when draining and inFlight reaches 0

	// Outcomes by code (and ambiguity) and latency of every pipeline answer (Release It O1).
	outcomes [numCodes][2]atomic.Int64
	latency  *metrics.LatencyHistogram
}

// NewServer wires the pipeline. clock may be nil (time.Now).
func NewServer(cfg Config, tickets TicketVerifier, admitter Admitter, exec Executor, clock Clock) *Server {
	if clock == nil {
		clock = time.Now
	}
	cfg = cfg.WithDefaults()
	adm := NewAdmission(cfg.Admission)
	adm.now = clock
	return &Server{cfg: cfg, tickets: tickets, admitter: admitter, adm: adm, exec: exec,
		clock: clock, idle: make(chan struct{}), latency: metrics.NewLatency(nil)}
}

// Config returns the effective configuration.
func (s *Server) Config() Config { return s.cfg }

// Admission exposes admission state (for health and stats).
func (s *Server) Admission() *Admission { return s.adm }

// Input is one request as the transport received it.
type Input struct {
	Ticket  string        // from the Authorization header; "" if missing
	Body    []byte        // at most Limits.MaxBody + 1 bytes (the transport stops reading there)
	Timeout time.Duration // client's deadline, relative; 0 = DefaultTimeout
}

// Response is the JSON body of every answer.
type Response struct {
	Code         Code     `json:"code"`
	RequestID    string   `json:"request_id,omitempty"`
	Columns      []string `json:"columns,omitempty"`
	Rows         [][]any  `json:"rows,omitempty"`
	Count        int64    `json:"count"`                    // rows returned (read) or affected (write)
	Ambiguous    bool     `json:"ambiguous,omitempty"`      // expired write that may have committed
	Message      string   `json:"message,omitempty"`        // fixed per code; bad_request explains the input
	RetryAfterMS int      `json:"retry_after_ms,omitempty"` // same as the Retry-After header
	LogID        string   `json:"log_id,omitempty"`         // internal: look it up in the agent's log
	Timing       *Timing  `json:"timing,omitempty"`
}

// Timing says where the time went inside the agent (microseconds).
type Timing struct {
	AdmitUS int64 `json:"admit_us"` // admission queue wait
	ExecUS  int64 `json:"exec_us"`  // executor call, including the actor's queue
}

// Output is the pipeline's answer plus what the transport needs to log it.
type Output struct {
	Status     int
	RetryAfter int // seconds; 0 = no header
	Resp       Response
	Err        error  // the cause, for the log (never sent)
	Stack      string // set when a panic was recovered
	Tenant     string // from the verified ticket, for the log; "" before verification
	Sub        string
}

// Handle runs one request through the pipeline. It never panics: a panic in a port is recovered and answered
// internal (the transport logs it with a log ID).
//
//	post: out.Resp.Code == Classify(out.Err) unless a panic was recovered (then Internal)
//	post: every Admitter release and admission slot taken is released before Handle returns
//	post: the answer is counted in Stats (by code, and its latency)
func (s *Server) Handle(ctx context.Context, in Input) Output {
	t0 := time.Now()
	out := s.handle(ctx, in)
	s.latency.Observe(time.Since(t0))
	amb := 0
	if out.Resp.Ambiguous {
		amb = 1
	}
	s.outcomes[codeIndex[out.Resp.Code]][amb].Add(1)
	return out
}

func (s *Server) handle(ctx context.Context, in Input) (out Output) {
	if !s.enter() {
		return s.answer(Request{}, nil, ErrDraining)
	}
	defer s.leave()
	var req Request
	defer func() {
		if p := panicx.From(recover()); p != nil {
			out = s.answer(req, nil, p)
			out.Resp.Code, out.Status, out.RetryAfter = Internal, Spec[Internal].Status, 0
			out.Resp.Message, out.Stack = message[Internal], p.Stack
		}
	}()
	if in.Ticket == "" {
		return s.answer(req, nil, ticket.ErrMalformed)
	}
	req, err := DecodeRequest(in.Body, s.cfg.Limits)
	if err != nil {
		return s.answer(req, nil, err)
	}
	now := s.clock()
	deadline := now.Add(s.cfg.timeout(in.Timeout))
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	claims, err := s.tickets.Verify(in.Ticket, now, ticket.Want{DB: req.DB, Perm: req.Kind.Perm()})
	if err != nil {
		return s.answer(req, nil, err)
	}
	release, err := s.admitter.Admit(req.DB, claims.Epoch)
	if err != nil {
		return s.answer(req, &claims, err)
	}
	assert.That(release != nil, "Admitter.Admit returned no error and no release")
	defer release()

	t0 := time.Now()
	slot, err := s.adm.Acquire(ctx, claims.TID, req.DB)
	if err != nil {
		return s.answer(req, &claims, err)
	}
	defer slot()
	t1 := time.Now()
	stmt := min(s.cfg.StmtTimeout, max(deadline.Sub(s.clock()), time.Millisecond))
	reply, err := s.exec.Exec(ctx, Call{Tenant: claims.TID, DB: req.DB, Epoch: claims.Epoch, Gen: claims.Gen, RequestID: req.RequestID, Kind: req.Kind,
		SQL: req.SQL, Args: req.Args, Deadline: deadline, StmtTimeout: stmt, MaxRows: s.cfg.Limits.MaxRows})
	t2 := time.Now()
	out = s.answer(req, &claims, err)
	out.Resp.Timing = &Timing{AdmitUS: t1.Sub(t0).Microseconds(), ExecUS: t2.Sub(t1).Microseconds()}
	if err != nil {
		return out
	}
	assert.That(req.Kind == KindWrite || (reply.Count == int64(len(reply.Rows)) && len(reply.Rows) <= s.cfg.Limits.MaxRows),
		"Executor broke its contract: count %d, %d rows, max %d", reply.Count, len(reply.Rows), s.cfg.Limits.MaxRows)
	out.Resp.Columns, out.Resp.Rows, out.Resp.Count = reply.Columns, wireValues(reply.Rows), reply.Count
	return out
}

// answer builds the output for err (nil = ok) from the spec table. claims may be nil (not verified yet).
func (s *Server) answer(req Request, claims *ticket.Claims, err error) Output {
	code := Classify(err)
	ra := retryAfter(code, err)
	out := Output{Status: Spec[code].Status, RetryAfter: ra, Err: err,
		Resp: Response{Code: code, RequestID: req.RequestID, Message: message[code], RetryAfterMS: ra * 1000}}
	if claims != nil {
		out.Tenant, out.Sub = claims.TID, claims.Sub
	}
	var bad *BadRequestError
	if errors.As(err, &bad) {
		out.Resp.Message = bad.Msg
		if bad.Status != 0 {
			out.Status = bad.Status
		}
	}
	if code == Expired && errors.Is(err, ErrNoReply) && req.Kind == KindWrite {
		out.Resp.Ambiguous = true
	}
	return out
}

func (s *Server) enter() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return false
	}
	s.inFlight++
	s.inFlightHW = max(s.inFlightHW, s.inFlight)
	return true
}

func (s *Server) leave() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight--
	assert.That(s.inFlight >= 0, "Server: in-flight count went negative")
	if s.draining && s.inFlight == 0 {
		close(s.idle)
	}
}

// Drain makes every new request answer shed and returns a channel closed once no request is in flight.
func (s *Server) Drain() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.draining {
		s.draining = true
		if s.inFlight == 0 {
			close(s.idle)
		}
	}
	return s.idle
}

// Draining reports whether Drain was called.
func (s *Server) Draining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

// InFlight is the number of requests inside Handle.
func (s *Server) InFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inFlight
}

// ServerStats are the pipeline's counters since NewServer (Release It O1). Every request is counted once it is
// answered; Handle is bounded by the request deadline plus the executor's grace, so a hung request shows in the
// latency histogram at its deadline, not never.
type ServerStats struct {
	Outcomes          map[Code]int64 // answers by wire code
	Ambiguous         int64          // of the expired answers, writes that may have committed
	InFlight          int
	InFlightHighWater int
	LatencyBounds     []time.Duration
	LatencyCounts     []uint64 // per bound, plus one past the last
	LatencySum        time.Duration
}

// Stats returns a snapshot.
func (s *Server) Stats() ServerStats {
	st := ServerStats{Outcomes: make(map[Code]int64, len(Codes))}
	for i, c := range Codes {
		st.Outcomes[c] = s.outcomes[i][0].Load() + s.outcomes[i][1].Load()
		st.Ambiguous += s.outcomes[i][1].Load()
	}
	st.LatencyBounds, st.LatencyCounts, st.LatencySum = s.latency.Snapshot()
	s.mu.Lock()
	st.InFlight, st.InFlightHighWater = s.inFlight, s.inFlightHW
	s.mu.Unlock()
	return st
}
