package actor

import (
	"sync/atomic"
	"time"

	"github.com/nickstrad/mansio/server/internal/metrics"
)

// Queues counts what the actors sharing it (Config.Queues) refused and how long requests waited in a mailbox;
// a process keeps one for /metrics. A nil *Queues counts nothing.
type Queues struct {
	mailboxFull, readFull atomic.Int64
	wait                  *metrics.LatencyHistogram
}

func NewQueues() *Queues { return &Queues{wait: metrics.NewLatency(nil)} }

// MailboxFull is how many requests Submit refused because a mailbox was full.
func (q *Queues) MailboxFull() int64 { return q.mailboxFull.Load() }

// ReadFull is how many reads were refused because a read pool's queue was full.
func (q *Queues) ReadFull() int64 { return q.readFull.Load() }

// MailboxWait is the time from Submit to the batch that picked the request up (expired requests included).
func (q *Queues) MailboxWait() *metrics.LatencyHistogram { return q.wait }

func (q *Queues) refusedMailbox() {
	if q != nil {
		q.mailboxFull.Add(1)
	}
}

func (q *Queues) refusedRead() {
	if q != nil {
		q.readFull.Add(1)
	}
}

func (q *Queues) pickedUp(batch []*Request, now time.Time) {
	if q == nil {
		return
	}
	for _, r := range batch {
		q.wait.Observe(now.Sub(r.enqueued))
	}
}
