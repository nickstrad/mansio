package front

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/assert"
	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/docexec"
	"github.com/nickstrad/mansio/server/internal/docexec/docactor"
	"github.com/nickstrad/mansio/server/internal/ir"
	"github.com/nickstrad/mansio/server/internal/panicx"
	"github.com/nickstrad/mansio/server/internal/slots"
	"github.com/nickstrad/mansio/server/internal/ticket"
)

// The document protocol (IR, ir/spec/v1.md): POST /v1/ops carries decoded-and-validated document operations
// instead of SQL. It shares the pipeline's ticket check, ownership, admission, deadlines and executor with
// /v1/query; what differs is the body (ir.DecodeBody), the work (a docexec closure on the actor) and the
// answer: {"result": […]} or the spec's {"error": {tag, message, op?, fields?}}, written with ir's encoder.
//
// Error mapping (spec §8):
//
//	decode, validation, expect, unknown table…  the protocol's own tag and status (SchemaError 400, …)
//	fingerprint mismatch                         SchemaDriftError 409 {expected (database), actual (request)}
//	schema not registered (new database)         SchemaDriftError 409 {expected: null, actual}
//	statement timeout                            StatementDeadlineError 504
//	statement or transaction failure             StorageError 500
//	request id used by a raw SQL write           SchemaError 400 at requestId (actor.ErrRecordKind)
//	panic, unexpected error                      Defect 500
//	shed, unavailable, expired, aborted, wrong_owner, fenced, ticket_invalid
//	                                             mansio's wire codes, statuses and Retry-After, as tags

// OpsPath is the document protocol's route.
const OpsPath = "/v1/ops"

// ErrSchemaUnknown: the registry has no schema with this fingerprint for the app (SchemaSource).
var ErrSchemaUnknown = errors.New("front: schema not registered for this app")

// SchemaSource is the schema registry port: it is asked only when a database has no schema yet (its first
// request), for the schema to create it with.
type SchemaSource interface {
	// Schema returns the fingerprint text app registered under fingerprint (spec §2).
	//
	//	post: err == nil ⇒ text hashes to fingerprint and is a valid schema in fingerprint form
	//	post: err != nil ⇒ errors.Is(err, ErrSchemaUnknown), errors.Is(err, ErrUnavailable), or ctx's error
	Schema(ctx context.Context, app, fingerprint string) (string, error)
}

// OpsConfig enables /v1/ops on a Server.
type OpsConfig struct {
	// Schemas creates new databases; nil: a database without a schema answers SchemaDriftError.
	Schemas SchemaSource
	// IDs draws server ids for inserts without a client id; default UUIDv7 on the server's clock and
	// det.SystemRand().
	IDs det.IDGen
	// SchemaCache bounds the decoded schemas kept (docexec.NewSchemas); default 256.
	SchemaCache int
	// Credentials, if set, accepts bearers that are not tickets (Credentials). Such a request reaches the
	// database credential-tenant ':' db.app at CredentialEpoch with read and write permission (single node:
	// static mode's default_epoch).
	Credentials     Credentials
	CredentialEpoch uint64
	// CredentialPlace, if set, replaces CredentialEpoch: the epoch and generation a credential request for db
	// runs at (single-node durable mode: the owner table's, claiming or creating db first). It is called after
	// the credential and the schema are checked. Errors are answered as they are (ErrUnknownDB, ErrUnavailable,
	// QuotaError, ...).
	CredentialPlace func(ctx context.Context, db string) (epoch, gen uint64, err error)
	// VerifySlots bounds credential requests inside their checks at once: Credentials.Authenticate, the schema
	// lookup and CredentialPlace may each call a remote store, and they run before admission. A request that
	// finds no free slot is shed at once, never queued (the edge's verify slots are the same shape). A warm
	// request holds its slot for microseconds. Default 64.
	VerifySlots int
	// Observer, if set, receives every /v1/ops request's trace from the HTTP adapter (opstrace.go).
	Observer OpsObserver
}

// ops is the enabled protocol's state.
type ops struct {
	creds    Credentials
	epoch    uint64
	place    func(ctx context.Context, db string) (uint64, uint64, error)
	observer OpsObserver
	schemas  SchemaSource
	ids      det.IDGen
	cache    *docexec.Schemas
	// verify is the credential path's pre-admission slots (OpsConfig.VerifySlots).
	verify *slots.Pool
}

// DefaultVerifySlots is OpsConfig.VerifySlots' default.
const DefaultVerifySlots = 64

// EnableOps turns on /v1/ops (without it the route is an unknown path).
//
//	pre: called once, before the server handles requests
func (s *Server) EnableOps(cfg OpsConfig) {
	assert.That(s.ops == nil, "front: EnableOps called twice")
	if cfg.IDs == nil {
		cfg.IDs = docexec.NewUUIDv7(s.clock, nil)
	}
	assert.That(cfg.Credentials == nil || cfg.CredentialEpoch > 0 || cfg.CredentialPlace != nil, "front: credentials need an epoch")
	if cfg.VerifySlots <= 0 {
		cfg.VerifySlots = DefaultVerifySlots
	}
	s.ops = &ops{creds: cfg.Credentials, epoch: cfg.CredentialEpoch, place: cfg.CredentialPlace, observer: cfg.Observer, schemas: cfg.Schemas, ids: cfg.IDs,
		cache: docexec.NewSchemas(cfg.SchemaCache), verify: slots.New(cfg.VerifySlots)}
}

// OpsVerify is the credential path's pre-admission slots, for metrics and the runtime limit; nil before
// EnableOps.
func (s *Server) OpsVerify() *slots.Pool {
	if s.ops == nil {
		return nil
	}
	return s.ops.verify
}

// OpsEnabled reports whether EnableOps was called.
func (s *Server) OpsEnabled() bool { return s.ops != nil }

// OpsOutput is a /v1/ops answer plus what the transport needs to log it.
type OpsOutput struct {
	Status     int
	RetryAfter int    // seconds; 0 = no header
	Body       []byte // the JSON body, without a trailing newline
	Code       Code   // the outcome as a pipeline code (Stats, logs): protocol 4xx are BadRequest, 5xx Internal
	Tag        string // the wire tag; "" when ok
	Ambiguous  bool   // expired mutation that may have committed
	RequestID  string
	Err        error  // the cause, for the log (never sent)
	Stack      string // set when a panic was recovered
	Tenant     string // from the verified ticket; "" before verification
	Sub        string
}

// HandleOps runs one /v1/ops request through the pipeline. It never panics.
//
//	post: every Admitter release and admission slot taken is released before HandleOps returns
//	post: the answer is counted in Stats (by Code, and its latency)
func (s *Server) HandleOps(ctx context.Context, in Input) OpsOutput {
	t0 := s.clock.Now()
	if in.Trace != nil {
		in.Trace.now = s.clock.Now
	}
	out := s.handleOps(ctx, in)
	took := det.Since(s.clock, t0)
	s.latency.Observe(took)
	if in.Trace != nil {
		in.Trace.update(func(t *OpsTraceSnapshot) { t.Pipeline = took })
	}
	amb := 0
	if out.Ambiguous {
		amb = 1
	}
	s.outcomes[out.Code.index()][amb].Add(1)
	return out
}

func (s *Server) handleOps(ctx context.Context, in Input) (out OpsOutput) {
	if !s.enter() {
		return opsAnswer(nil, nil, Reply{}, ErrDraining)
	}
	defer s.leave()
	var req *ir.Request
	defer func() {
		if p := panicx.From(recover()); p != nil {
			out = opsAnswer(req, nil, Reply{}, p)
			out.Stack = p.Stack
		}
	}()
	if in.Ticket == "" && s.ops.creds == nil {
		return opsAnswer(nil, nil, Reply{}, ticket.ErrMalformed)
	}
	req, e := ir.DecodeBody(in.Body)
	if e != nil {
		return opsAnswer(nil, nil, Reply{}, &docexec.Failure{Err: e})
	}
	if in.Trace != nil {
		in.Trace.update(func(t *OpsTraceSnapshot) { t.Kind, t.App = req.Kind, req.DB.App })
	}
	kind := KindRead
	if req.Kind == ir.Mutation {
		kind = KindWrite
	}
	now := s.clock.Now()
	deadline := now.Add(s.cfg.timeout(in.Timeout))
	ctx, cancel := det.WithDeadline(ctx, s.clock, deadline)
	defer cancel()

	claims, err := s.verifyOps(ctx, in, req, kind, now)
	if err != nil {
		return opsAnswer(req, nil, Reply{}, err)
	}
	db := claims.DBID // the placement key tenant ':' app (ADR 0011), from the ticket or the credential
	if in.Trace != nil {
		in.Trace.update(func(t *OpsTraceSnapshot) { t.Tenant = claims.TID })
	}
	release, err := s.admitter.Admit(db, claims.Epoch)
	if err != nil {
		return opsAnswer(req, &claims, Reply{}, err)
	}
	assert.That(release != nil, "Admitter.Admit returned no error and no release")
	defer release()
	slot, err := s.adm.Acquire(ctx, claims.TID, db)
	if err != nil {
		return opsAnswer(req, &claims, Reply{}, err)
	}
	defer slot()

	base := Call{Tenant: claims.TID, DB: db, Epoch: claims.Epoch, Gen: claims.Gen, Kind: kind, Deadline: deadline}
	run := func(c Call) (Reply, error) {
		c.StmtTimeout = min(s.cfg.StmtTimeout, max(deadline.Sub(s.clock.Now()), time.Millisecond))
		return s.exec.Exec(ctx, c)
	}
	call := func() (Reply, error) {
		dc := &docactor.Call{Req: req, Schemas: s.ops.cache, IDs: s.ops.ids}
		c := base
		c.Fn, c.Check = dc.Fn, dc.Check
		if kind == KindWrite {
			c.RequestID = req.RequestID // a query's request id is ignored (spec §7)
		}
		if in.Trace == nil {
			return run(c)
		}
		dc.Wrap = in.Trace.wrap
		submitted := s.clock.Now()
		in.Trace.update(func(t *OpsTraceSnapshot) { t.Submitted, t.Began = submitted, time.Time{} })
		reply, err := run(c)
		ended := s.clock.Now()
		in.Trace.update(func(t *OpsTraceSnapshot) { t.Ended = ended })
		return reply, err
	}
	reply, err := call()
	if errors.Is(err, docexec.ErrUninitialized) { // a new database: create it with the request's schema, then retry
		if err = s.initDB(ctx, req, base, run); err == nil {
			reply, err = call()
		}
	}
	return opsAnswer(req, &claims, reply, err)
}

// verifyOps authorizes a /v1/ops request: a ticket for db.tenant ':' db.app, or (with Credentials) a credential,
// whose tenant replaces db.tenant. A bearer that is not ticket-shaped at all goes to Credentials; a ticket that
// fails verification stays ticket_invalid.
func (s *Server) verifyOps(ctx context.Context, in Input, req *ir.Request, kind Kind, now time.Time) (ticket.Claims, error) {
	token := in.Ticket
	if token == "" && in.MalformedAuth && s.ops.creds != nil {
		return ticket.Claims{}, &AuthError{Reason: "malformed"}
	}
	if token != "" {
		claims, err := s.tickets.Verify(token, now, ticket.Want{DB: req.DB.Tenant + ":" + req.DB.App, Perm: kind.Perm()})
		if err == nil || s.ops.creds == nil || !(errors.Is(err, ticket.ErrMalformed) || errors.Is(err, ticket.ErrVersion)) {
			return claims, err
		}
	}
	// The steps below may each call a remote store (the tenancy store, the schema registry, the placement
	// authority) and run before admission: bound how many requests are inside them, and refuse the rest at once.
	if !s.ops.verify.TryAcquire() {
		return ticket.Claims{}, fmt.Errorf("%w: too many credential checks in flight; nothing ran", ErrShed)
	}
	defer s.ops.verify.Release()
	tenant, sub, err := s.ops.creds.Authenticate(ctx, token, req.DB.App)
	if err != nil {
		return ticket.Claims{}, err
	}
	// A credential request may only name a registered schema, checked before anything is admitted or created
	// (with create_dbs a database's file and actor would otherwise exist before its schema is looked up).
	if s.ops.schemas != nil {
		if _, err := s.ops.schemas.Schema(ctx, req.DB.App, req.Schema); err != nil {
			return ticket.Claims{}, schemaError(ctx, req, err)
		}
	}
	db := tenant + ":" + req.DB.App
	if e := CheckDBID(db); e != nil {
		return ticket.Claims{}, fmt.Errorf("front: the credentials port answered tenant %q, which makes no valid database id (%v)", tenant, e) // its bug: internal
	}
	epoch, gen := s.ops.epoch, uint64(0)
	if s.ops.place != nil {
		if epoch, gen, err = s.ops.place(ctx, db); err != nil {
			return ticket.Claims{}, err
		}
	}
	return ticket.Claims{TID: tenant, DBID: db, Epoch: epoch, Gen: gen, Sub: sub, Perms: ticket.Read | ticket.Write}, nil
}

// initDB creates a new database's tables from the registered schema req names (docexec.Init). Another
// request may initialize it first: Init then only compares.
func (s *Server) initDB(ctx context.Context, req *ir.Request, base Call, run func(Call) (Reply, error)) error {
	if s.ops.schemas == nil {
		return &docexec.Failure{Err: unregistered(req.Schema)}
	}
	text, err := s.ops.schemas.Schema(ctx, req.DB.App, req.Schema)
	if err != nil {
		return schemaError(ctx, req, err)
	}
	schema, e := s.ops.cache.Get(text)
	if e != nil || schema.Fingerprint != req.Schema {
		// SchemaSource broke its contract (it returns valid fingerprint texts): a bug, not the client's.
		return errors.New("front: the schema source answered a text that is not the schema asked for")
	}
	c := base
	c.Kind, c.Fn = KindWrite, docactor.InitFn(schema)
	if _, err = run(c); errors.Is(err, ErrNoReply) {
		// Only the idempotent creation may have committed; the request itself never ran: not ambiguous.
		return fmt.Errorf("%w: creating the database got no reply in time (%v); the request did not run", ErrExpired, err)
	}
	return err
}

// schemaError classifies a SchemaSource failure: an unregistered schema is drift, the rest unavailable, expired or
// passed on.
func schemaError(ctx context.Context, req *ir.Request, err error) error {
	switch {
	case errors.Is(err, ErrSchemaUnknown):
		return &docexec.Failure{Err: unregistered(req.Schema)}
	case errors.Is(err, ErrUnavailable):
		return err
	case ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		// The request's deadline passed (or the client left) while waiting for the registry: nothing ran.
		return fmt.Errorf("%w: waiting for the schema registry: %w", ErrExpired, err)
	}
	return err
}

// unregistered is the drift answer for a new database whose request names a schema the registry does not
// have for the app: there is no database fingerprint to report.
func unregistered(fingerprint string) *ir.Error {
	return ir.NewError(ir.TagSchemaDrift, "expected", nil, "actual", fingerprint, "detail", "no schema with this fingerprint is registered for the app")
}

// opsAnswer is the answer for reply or err (nil = ok). req and claims may be nil (not decoded or verified yet).
func opsAnswer(req *ir.Request, claims *ticket.Claims, reply Reply, err error) OpsOutput {
	var out OpsOutput
	if req != nil && req.Kind == ir.Mutation {
		out.RequestID = req.RequestID
	}
	if claims != nil {
		out.Tenant, out.Sub = claims.TID, claims.Sub
	}
	out.Err = err
	if err == nil {
		assert.That(reply.Body != nil, "front: a closure call answered ok without a body")
		body := make([]byte, 0, len(reply.Body)+32)
		body = append(append(body, `{"result":`...), reply.Body...)
		if reply.Duplicate {
			body = append(body, `,"duplicate":true`...)
		}
		out.Status, out.Code, out.Body = 200, OK, append(body, '}')
		return out
	}
	e, code := opsError(req, err)
	if e != nil {
		out.Status, out.Tag, out.Code = e.Status(), e.Tag, code
		if bad := (*BadRequestError)(nil); errors.As(err, &bad) && bad.Status != 0 {
			out.Status = bad.Status // 405, 408: the transport's refusals keep their status
		}
		out.Body = []byte(ir.Stringify(e.Wire()))
		return out
	}
	// One of mansio's wire codes, passed through (spec §8: the lowercase tags).
	out.Code, out.Tag, out.Status = code, string(code), Spec[code].Status
	out.RetryAfter = retryAfter(code, err)
	body := ir.NewObject("tag", string(code), "message", message[code])
	if code == Expired && errors.Is(err, ErrNoReply) && req != nil && req.Kind == ir.Mutation {
		out.Ambiguous = true
		body.Set("fields", ir.NewObject("ambiguous", true)) // a mutation may have committed: retry with the same id
	}
	out.Body = []byte(ir.Stringify(ir.NewObject("error", body)))
	return out
}

// opsError classifies err: the protocol error to answer (with its pipeline code), or nil and the mansio code.
func opsError(req *ir.Request, err error) (*ir.Error, Code) {
	var f *docexec.Failure
	isFailure := errors.As(err, &f)
	protocolCode := func(e *ir.Error) Code {
		if e.Status() >= 500 {
			return Internal
		}
		return BadRequest
	}
	withOp := func(e *ir.Error) *ir.Error {
		if isFailure && f.Err.HasOp {
			e.WithOp(f.Err.Op)
		}
		return e
	}
	var bad *BadRequestError
	var p *panicx.Panic
	var quota *QuotaError
	if errors.As(err, &quota) {
		return ir.NewError(ir.TagQuota, "scope", quota.Scope, "limit", float64(quota.Limit)), BadRequest
	}
	var auth *AuthError
	if errors.As(err, &auth) {
		app := ""
		if req != nil {
			app = req.DB.App
		}
		return ir.NewError(ir.TagAuth, "scope", "app", "appId", app, "reason", auth.Reason), TicketInvalid
	}
	switch code := Classify(err); {
	case errors.Is(err, ErrStmtTimeout):
		return withOp(ir.NewError(ir.TagStmtDeadline)), StmtTimeout
	case isFailure && f.Cause == nil:
		return f.Err, protocolCode(f.Err)
	case errors.As(err, &bad):
		return ir.SchemaErr(ir.ReasonTypeMismatch, "body", bad.Msg, ir.Undefined{}), BadRequest
	case errors.Is(err, actor.ErrRecordKind):
		return ir.SchemaErr(ir.ReasonTypeMismatch, "requestId", "an id no raw SQL write has used", requestID(req)), BadRequest
	case errors.As(err, &p):
		return ir.NewError(ir.TagDefect), Internal
	case code != Internal && code != BadRequest:
		return nil, code
	case isFailure:
		return f.Err, Internal // a StorageError at its operation; the cause goes to the log
	}
	return ir.NewError(ir.TagDefect), Internal
}

func requestID(req *ir.Request) any {
	if req == nil {
		return ir.Undefined{}
	}
	return req.RequestID
}
