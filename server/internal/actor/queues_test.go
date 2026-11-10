package actor

import (
	"context"
	"errors"
	"testing"
	"time"
)

func waitCount(q *Queues) uint64 {
	_, counts, _ := q.MailboxWait().Snapshot()
	var n uint64
	for _, c := range counts {
		n += c
	}
	return n
}

// A full mailbox is counted where it is refused; every request a batch picks up adds one mailbox wait.
func TestQueuesCountMailboxRefusalsAndWaits(t *testing.T) {
	q := NewQueues()
	a := fakeActor(t, Config{MailboxSize: 2, Queues: q}, &faultExec{}, nil)
	var accepted []*Request
	for range 5 { // not started: the mailbox (2) fills
		r := &Request{SQL: "r"}
		if err := a.Submit(r); err == nil {
			accepted = append(accepted, r)
		} else if !errors.Is(err, ErrMailboxFull) {
			t.Fatal(err)
		}
	}
	if got := q.MailboxFull(); got != 3 || len(accepted) != 2 {
		t.Fatalf("mailbox full counted %d with %d accepted, want 3 and 2", got, len(accepted))
	}
	if err := a.Start(true); err != nil {
		t.Fatal(err)
	}
	for _, r := range accepted {
		if res := r.Wait(context.Background()); res.Err != nil {
			t.Fatal(res.Err)
		}
	}
	if got := waitCount(q); got != 2 {
		t.Fatalf("%d mailbox waits observed, want 2", got)
	}
	if q.ReadFull() != 0 {
		t.Fatal("read refusals counted without a read pool")
	}
}

// A read the read pool's queue has no room for is counted as a read refusal, not a mailbox one.
func TestQueuesCountReadQueueRefusals(t *testing.T) {
	q := NewQueues()
	a := fakeActor(t, Config{ReadConns: 1, MailboxSize: 2, Queues: q}, &faultExec{}, nil)
	if err := a.Start(true); err != nil {
		t.Fatal(err)
	}
	a.Submit(&Request{SQL: "SLOW", StmtTimeout: 300 * time.Millisecond})
	time.Sleep(20 * time.Millisecond) // the only reader is busy
	var shed int64
	var queued []*Request
	for range 10 {
		r := &Request{SQL: "r"}
		if a.Submit(r) == nil {
			queued = append(queued, r)
		}
	}
	for _, r := range queued {
		if res := r.Wait(context.Background()); errors.Is(res.Err, ErrMailboxFull) {
			shed++
		}
	}
	if shed == 0 || q.ReadFull() != shed {
		t.Fatalf("read queue shed %d, counted %d", shed, q.ReadFull())
	}
}

// Actors without Queues (the zero Config) count nothing and do not fail.
func TestNilQueuesCountNothing(t *testing.T) {
	a := fakeActor(t, Config{MailboxSize: 1}, &faultExec{}, nil)
	a.Submit(&Request{SQL: "r"})
	if err := a.Submit(&Request{SQL: "r"}); !errors.Is(err, ErrMailboxFull) {
		t.Fatalf("err %v", err)
	}
}
