package front

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/docexec"
	"github.com/nickstrad/mansio/server/internal/ir"
	"github.com/nickstrad/mansio/server/internal/panicx"
	"github.com/nickstrad/mansio/server/internal/ticket"
)

// The /v1/ops error mapping (spec §8), one row per outcome class.
func TestOpsAnswerMapping(t *testing.T) {
	mutation := &ir.Request{Kind: ir.Mutation, RequestID: "r1"}
	query := &ir.Request{Kind: ir.Query}
	storageAt2 := &docexec.Failure{Err: ir.NewError(ir.TagStorage).WithOp(2), Cause: errors.New("disk I/O error")}
	tests := []struct {
		name       string
		req        *ir.Request
		err        error
		status     int
		code       Code
		body       string
		retryAfter bool
	}{
		{"protocol 4xx", mutation, &docexec.Failure{Err: ir.NewError(ir.TagNotFound, "table", "t", "id", "x").WithOp(1)}, 404, BadRequest,
			`{"error":{"tag":"NotFoundError","message":"NotFoundError: table=\"t\" id=\"x\"","op":1,"fields":{"table":"t","id":"x"}}}`, false},
		{"protocol 5xx carries no fields", query, &docexec.Failure{Err: ir.NewError(ir.TagDecode, "reason", "x")}, 500, Internal,
			`{"error":{"tag":"DecodeError","message":"DecodeError"}}`, false},
		{"statement timeout", mutation, fmt.Errorf("%w: %w", ErrStmtTimeout, storageAt2), 504, StmtTimeout,
			`{"error":{"tag":"StatementDeadlineError","message":"StatementDeadlineError","op":2}}`, false},
		{"statement failure", mutation, storageAt2, 500, Internal, `{"error":{"tag":"StorageError","message":"StorageError","op":2}}`, false},
		{"panic", mutation, panicx.From("boom"), 500, Internal, `{"error":{"tag":"Defect","message":"Defect"}}`, false},
		{"unexpected", mutation, errors.New("?"), 500, Internal, `{"error":{"tag":"Defect","message":"Defect"}}`, false},
		{"request id of another kind", mutation, actor.ErrRecordKind, 400, BadRequest, "", false},
		{"transport refusal", nil, &BadRequestError{Msg: "a body received in time", Status: 408}, 408, BadRequest, "", false},
		{"shed", mutation, ErrShed, 503, Shed, `{"error":{"tag":"shed","message":"not accepted now; nothing ran"}}`, true},
		{"draining", nil, ErrDraining, 503, Shed, "", true},
		{"unavailable", query, WithRetryAfter(ErrUnavailable, 3*time.Second), 503, Unavailable, "", true},
		{"expired", mutation, ErrExpired, 504, Expired, `{"error":{"tag":"expired","message":"deadline passed before an answer"}}`, false},
		{"ambiguous", mutation, ErrNoReply, 504, Expired,
			`{"error":{"tag":"expired","message":"deadline passed before an answer","fields":{"ambiguous":true}}}`, false},
		{"query without reply is not ambiguous", query, ErrNoReply, 504, Expired, `{"error":{"tag":"expired","message":"deadline passed before an answer"}}`, false},
		{"aborted", mutation, ErrAborted, 409, Aborted, "", false},
		{"wrong owner", query, ErrWrongOwner, 421, WrongOwner, "", false},
		{"unknown db", query, ErrUnknownDB, 421, WrongOwner, "", false},
		{"fenced", mutation, ErrFenced, 421, Fenced, "", true},
		{"ticket", query, ticket.ErrExpired, 401, TicketInvalid, `{"error":{"tag":"ticket_invalid","message":"ticket invalid"}}`, false},
	}
	for _, tc := range tests {
		out := opsAnswer(tc.req, nil, Reply{}, tc.err)
		if out.Status != tc.status || out.Code != tc.code || (tc.body != "" && string(out.Body) != tc.body) {
			t.Errorf("%s: %d %s %s, want %d %s %s", tc.name, out.Status, out.Code, out.Body, tc.status, tc.code, tc.body)
		}
		if (out.RetryAfter > 0) != tc.retryAfter {
			t.Errorf("%s: Retry-After %d", tc.name, out.RetryAfter)
		}
		if v, e := ir.Parse(string(out.Body)); e != nil || !strings.HasPrefix(string(out.Body), `{"error":{"tag":`) {
			t.Errorf("%s: body %s (%v)", tc.name, out.Body, v)
		}
		if out.Ambiguous != (tc.name == "ambiguous") {
			t.Errorf("%s: ambiguous %v", tc.name, out.Ambiguous)
		}
	}
	ok := opsAnswer(mutation, nil, Reply{Body: []byte(`["x"]`), Duplicate: true}, nil)
	if ok.Status != 200 || string(ok.Body) != `{"result":["x"],"duplicate":true}` || ok.RequestID != "r1" {
		t.Errorf("ok: %d %s", ok.Status, ok.Body)
	}
}
