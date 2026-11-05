// Package faultnet is a fault-injecting TCP proxy for tests (Release It ch5 §5.7, guide H4). It sits between a
// client and a real service and breaks the transport the way networks do: refused, silent, black-holed,
// reset mid-stream, slow, throttled, half-open. Interface-level fakes cannot produce these failures, and they
// are what find a missing socket timeout.
//
// Test harness, not production code: it may log nothing and has no error module of its own (errors are plain).
package faultnet

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Kind names one transport failure.
type Kind int

const (
	Pass              Kind = iota // forward unchanged
	Refuse                        // accept, then reset at once: the client fails within one round trip
	AcceptThenSilence             // accept, never read, never reply
	Blackhole                     // accept, read and discard, never reply
	ResetAfter                    // forward until Bytes bytes went upstream → client, then reset both sides
	Latency                       // forward, sleeping Delay before each chunk in each direction
	Throttle                      // forward at most Rate bytes/s in each direction
	HalfOpen                      // stop forwarding and keep the sockets open; also freezes live connections
)

func (k Kind) String() string {
	return [...]string{"pass", "refuse", "accept_then_silence", "blackhole", "reset_after", "latency",
		"throttle", "half_open"}[k]
}

// Fault is a Kind with its parameter. Build one with the constructors below.
type Fault struct {
	Kind  Kind
	Bytes int64         // ResetAfter
	Delay time.Duration // Latency
	Rate  int64         // Throttle, bytes per second
}

func Healthy() Fault                    { return Fault{Kind: Pass} }
func RefuseConn() Fault                 { return Fault{Kind: Refuse} }
func Silence() Fault                    { return Fault{Kind: AcceptThenSilence} }
func Hole() Fault                       { return Fault{Kind: Blackhole} }
func ResetAfterBytes(n int64) Fault     { return Fault{Kind: ResetAfter, Bytes: n} }
func Delayed(d time.Duration) Fault     { return Fault{Kind: Latency, Delay: d} }
func Throttled(bytesPerSec int64) Fault { return Fault{Kind: Throttle, Rate: max(bytesPerSec, 1)} }
func Frozen() Fault                     { return Fault{Kind: HalfOpen} }

// Proxy forwards connections from Addr to an upstream address, applying the current fault to each new
// connection. Safe for concurrent use.
type Proxy struct {
	upstream string
	ln       net.Listener

	mu    sync.Mutex
	fault Fault
	left  int // new connections that still get fault; 0 = every one until Heal
	links map[*link]struct{}

	accepted atomic.Int64
	wg       sync.WaitGroup
	closed   chan struct{}
}

// New listens on a free loopback port and forwards to upstream (host:port).
func New(upstream string) (*Proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{upstream: upstream, ln: ln, links: map[*link]struct{}{}, closed: make(chan struct{})}
	p.wg.Add(1)
	go p.serve()
	return p, nil
}

// Addr is the proxy's host:port; point the client here.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// Accepted is how many connections the proxy has accepted.
func (p *Proxy) Accepted() int64 { return p.accepted.Load() }

// Set makes the next times new connections (every new one if times <= 0, until Heal) suffer f. Setting
// HalfOpen also freezes every live connection, now and for good: that is a NAT or load balancer forgetting an
// idle flow. Heal right after Set(Frozen(), 0) to freeze only the connections already open.
func (p *Proxy) Set(f Fault, times int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fault, p.left = f, max(times, 0)
	if f.Kind == HalfOpen {
		for l := range p.links {
			l.frozen.Store(true)
		}
	}
}

// Heal makes new connections forward normally. Frozen connections stay frozen.
func (p *Proxy) Heal() { p.Set(Healthy(), 0) }

// Close stops the proxy and closes every connection.
func (p *Proxy) Close() error {
	select {
	case <-p.closed:
		return nil
	default:
	}
	close(p.closed)
	err := p.ln.Close()
	p.mu.Lock()
	for l := range p.links {
		l.close()
	}
	p.mu.Unlock()
	p.wg.Wait()
	return err
}

func (p *Proxy) serve() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return // listener closed
		}
		p.accepted.Add(1)
		l := &link{client: c, done: make(chan struct{})}
		p.mu.Lock()
		f := p.fault
		if p.left > 0 {
			p.left--
			if p.left == 0 {
				p.fault = Healthy()
			}
		}
		select {
		case <-p.closed:
			p.mu.Unlock()
			c.Close()
			return
		default:
		}
		p.links[l] = struct{}{}
		p.wg.Add(1)
		p.mu.Unlock()
		go func() {
			defer p.wg.Done()
			p.handle(l, f)
			p.mu.Lock()
			delete(p.links, l)
			p.mu.Unlock()
		}()
	}
}

// link is one client connection and, once dialed, its upstream connection.
type link struct {
	client   net.Conn
	mu       sync.Mutex
	upstream net.Conn
	frozen   atomic.Bool
	once     sync.Once
	done     chan struct{}
}

func (l *link) close() {
	l.once.Do(func() {
		close(l.done)
		l.client.Close()
		l.mu.Lock()
		if l.upstream != nil {
			l.upstream.Close()
		}
		l.mu.Unlock()
	})
}

// reset closes both sides with an RST instead of a FIN.
func (l *link) reset() {
	for _, c := range []net.Conn{l.client, l.up()} {
		if tc, ok := c.(*net.TCPConn); ok {
			tc.SetLinger(0)
		}
	}
	l.close()
}

func (l *link) up() net.Conn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.upstream
}

func (p *Proxy) handle(l *link, f Fault) {
	defer l.close()
	switch f.Kind {
	case Refuse:
		l.reset()
		return
	case AcceptThenSilence:
		<-l.done
		return
	case Blackhole:
		io.Copy(io.Discard, l.client) // returns when the client goes away
		return
	case HalfOpen:
		l.frozen.Store(true)
		<-l.done
		return
	}
	up, err := net.DialTimeout("tcp", p.upstream, 5*time.Second)
	if err != nil {
		l.reset()
		return
	}
	l.mu.Lock()
	l.upstream = up
	l.mu.Unlock()
	select {
	case <-l.done: // closed while dialing
		up.Close()
		return
	default:
	}
	var budget atomic.Int64 // bytes left before ResetAfter fires
	budget.Store(f.Bytes)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); l.pipe(up, l.client, f, nil) }()
	go func() {
		defer wg.Done()
		if f.Kind == ResetAfter {
			l.pipe(l.client, up, f, &budget)
		} else {
			l.pipe(l.client, up, f, nil)
		}
	}()
	wg.Wait()
}

// pipe copies src → dst chunk by chunk, applying f. budget (ResetAfter only) counts bytes left before the
// link is reset.
func (l *link) pipe(dst, src net.Conn, f Fault, budget *atomic.Int64) {
	chunk := 32 << 10
	if f.Kind == Throttle {
		chunk = int(min(int64(chunk), max(f.Rate/20, 1))) // ~20 writes a second keeps the rate smooth
	}
	buf := make([]byte, chunk)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if l.frozen.Load() {
				<-l.done
				return
			}
			b := buf[:n]
			switch f.Kind {
			case Latency:
				if !l.sleep(f.Delay) {
					return
				}
			case Throttle:
				if !l.sleep(time.Duration(int64(n) * int64(time.Second) / f.Rate)) {
					return
				}
			}
			if budget != nil {
				left := budget.Load()
				if int64(n) >= left {
					if left > 0 {
						dst.Write(b[:left])
					}
					time.Sleep(10 * time.Millisecond) // let the bytes leave before the RST discards them
					l.reset()
					return
				}
				budget.Add(-int64(n))
			}
			if _, werr := dst.Write(b); werr != nil {
				l.close()
				return
			}
		}
		if err != nil {
			if tc, ok := dst.(*net.TCPConn); ok && errors.Is(err, io.EOF) {
				tc.CloseWrite() // pass the half-close on; the other direction may still be busy
				return
			}
			l.close()
			return
		}
	}
}

func (l *link) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-l.done:
		return false
	}
}
