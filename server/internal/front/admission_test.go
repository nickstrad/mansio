package front

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Under concurrency the caps hold: never more than DBInFlight running per database, and every caller ends in
// run, shed or expired with nothing leaked.
func TestAdmissionConcurrentCapsHold(t *testing.T) {
	a := NewAdmission(AdmissionLimits{TenantInFlight: 4, TenantQueue: 8, DBInFlight: 2, DBQueue: 4})
	var running [3]atomic.Int32
	var ok, shedN, expired atomic.Int32
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Go(func() {
			db := []string{"a", "b", "c"}[i%3]
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(1+i%20)*time.Millisecond)
			defer cancel()
			rel, err := a.Acquire(ctx, "t", db)
			switch {
			case errors.Is(err, ErrShed):
				shedN.Add(1)
				return
			case errors.Is(err, ErrExpired):
				expired.Add(1)
				return
			case err != nil:
				t.Error(err)
				return
			}
			ok.Add(1)
			if n := running[i%3].Add(1); n > 2 {
				t.Errorf("db %s: %d running, cap 2", db, n)
			}
			time.Sleep(time.Millisecond)
			running[i%3].Add(-1)
			rel()
			rel() // idempotent
		})
	}
	wg.Wait()
	if s := a.Stats(); !s.Idle() {
		t.Fatalf("state left: %+v", s)
	}
	if ok.Load()+shedN.Load()+expired.Load() != 200 || ok.Load() == 0 || shedN.Load() == 0 {
		t.Fatalf("ok %d shed %d expired %d", ok.Load(), shedN.Load(), expired.Load())
	}
}

// A waiter whose deadline passes leaves the queue (expired), and a later release does not hand it a slot.
func TestAdmissionWaiterExpires(t *testing.T) {
	a := NewAdmission(AdmissionLimits{TenantInFlight: 1, TenantQueue: 1, DBInFlight: 1, DBQueue: 1})
	rel, err := a.Acquire(context.Background(), "t", "d")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := a.Acquire(ctx, "t", "d"); !errors.Is(err, ErrExpired) {
		t.Fatalf("err %v", err)
	}
	if s := a.Stats(); s.Queued != 0 || s.InFlight != 1 {
		t.Fatalf("%+v", s)
	}
	rel()
	if s := a.Stats(); !s.Idle() {
		t.Fatalf("%+v", s)
	}
}

// Granted at the same moment the waiter gives up: the slot goes straight back.
func TestAdmissionGrantRacingCancelReturnsTheSlot(t *testing.T) {
	a := NewAdmission(AdmissionLimits{TenantInFlight: 1, TenantQueue: 1, DBInFlight: 1, DBQueue: 1})
	for range 200 {
		rel, _ := a.Acquire(context.Background(), "t", "d")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error)
		go func() {
			r2, err := a.Acquire(ctx, "t", "d")
			if err == nil {
				r2()
			}
			done <- err
		}()
		for a.Stats().Queued == 0 {
			time.Sleep(10 * time.Microsecond)
		}
		go cancel()
		rel()
		<-done
		if s := a.Stats(); !s.Idle() {
			t.Fatalf("slot leaked: %+v", s)
		}
	}
}
