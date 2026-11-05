package actor

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Config applies to every actor. The zero value is valid: WithDefaults fills it in.
type Config struct {
	MailboxSize int           // bounded queue per database; default 256
	MaxBatch    int           // requests drained per batch; 1 disables group commit; default 64
	ReadConns   int           // read connections per database; 0 = reads share the write connection
	IdleClose   time.Duration // close the database after this much idle time (passivation); 0 = never
	JournalMode string        // SQLite journal_mode; default WAL (see JournalModes)
	Synchronous string        // SQLite synchronous; default NORMAL (see SynchronousModes)
	CacheKiB    int           // SQLite page cache per connection; default 256
	BusyTimeout time.Duration // default 5s
	StmtCache   int           // prepared statements kept per connection (least recently used evicted); default 64
	// RequestTTL is how long a database remembers an applied request id (CP-2): a retry within it is answered
	// from the record, a retry after it applies again. Default 24h. It must exceed the longest client retry
	// window, including a move.
	RequestTTL time.Duration
	// RequestSweep is the least time between retention sweeps of request ids; a sweep piggybacks on a write
	// batch. Default 1m.
	RequestSweep time.Duration
}

// JournalModes and SynchronousModes are the values Config accepts (upper case; input is case-insensitive).
var (
	JournalModes     = []string{"WAL", "DELETE", "TRUNCATE", "PERSIST", "MEMORY", "OFF"}
	SynchronousModes = []string{"OFF", "NORMAL", "FULL", "EXTRA"}
)

// WithDefaults returns c with every zero field set to its default and mode names upper-cased.
func (c Config) WithDefaults() Config {
	if c.MailboxSize <= 0 {
		c.MailboxSize = 256
	}
	if c.MaxBatch <= 0 {
		c.MaxBatch = 64
	}
	c.ReadConns = max(c.ReadConns, 0)
	c.JournalMode = strings.ToUpper(cmp.Or(c.JournalMode, "WAL"))
	c.Synchronous = strings.ToUpper(cmp.Or(c.Synchronous, "NORMAL"))
	if c.CacheKiB <= 0 {
		c.CacheKiB = 256
	}
	if c.BusyTimeout <= 0 {
		c.BusyTimeout = 5 * time.Second
	}
	if c.StmtCache <= 0 {
		c.StmtCache = 64
	}
	if c.RequestTTL <= 0 {
		c.RequestTTL = 24 * time.Hour
	}
	if c.RequestSweep <= 0 {
		c.RequestSweep = time.Minute
	}
	return c
}

// Validate reports settings SQLite would reject. It checks c.WithDefaults(), so zero fields are fine.
func (c Config) Validate() error {
	d := c.WithDefaults()
	if !slices.Contains(JournalModes, d.JournalMode) {
		return fmt.Errorf("journal_mode %q: want one of %v", c.JournalMode, JournalModes)
	}
	if !slices.Contains(SynchronousModes, d.Synchronous) {
		return fmt.Errorf("synchronous %q: want one of %v", c.Synchronous, SynchronousModes)
	}
	return nil
}

// Warnings lists legal but probably unintended settings.
func (c Config) Warnings() []string {
	d := c.WithDefaults()
	if d.ReadConns > 0 && d.JournalMode != "WAL" {
		return []string{fmt.Sprintf("read_conns=%d with journal_mode=%s: only WAL lets readers run alongside the writer; "+
			"in rollback modes readers and the writer block each other", d.ReadConns, d.JournalMode)}
	}
	return nil
}
