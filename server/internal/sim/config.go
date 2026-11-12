package sim

import (
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"time"

	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/vactor"
)

// Config is one run. Zero fields take values drawn from the seed (a small swarm: each seed turns fault classes
// on or off and picks their rates), so a seed alone names a run.
type Config struct {
	Seed     uint64
	Dir      string        // scratch directory (created; cache, owner state, oracle files)
	Duration time.Duration // simulated time to run; default 10 min
	Keys     int           // databases; 0: 1..3 from the seed
	Workload Workload      // nil: SQL{}
	Faults   *Faults       // nil: drawn from the seed
	Policy   *vactor.Policy
	Tick     time.Duration // maintenance interval; 0: from the seed
	// RPO is the stated loss window the checker allows (see Model): a write acknowledged at Ta and lost at a
	// crash at Tc is a violation iff [Ta, Tc] holds a fault-free stretch at least RPO long. 0: the bound
	// docs/notes/writeback states, Policy.WriteBackEvery + Tick, plus 1 s of slack for same-instant ordering.
	// What the agent should promise while S3 is down is an open decision (control-plane plan); here a
	// faulty stretch simply does not count.
	RPO time.Duration
	// KeepGoing runs to the end after a violation (default: stop at the first one).
	KeepGoing bool
	// NoZombies drops a late publish (Faults.LoseReply) whose process crashed before it landed. By default it
	// still lands, as S3 may complete a killed process's PUT; the restart's reopen stamp must fence it.
	NoZombies bool
}

// Faults are the fault rates. Probabilities are per call or per client request; Every fields are mean gaps of
// exponentially distributed events (0: never).
type Faults struct {
	Stall      float64       // an S3 call stalls (uniform up to StallMax) before it runs
	StallMax   time.Duration //
	FailCall   float64       // an S3 call fails without applying (FailRead/FailUpload/FailFetch/FailPublish/FailDelete)
	LoseReply  float64       // an S3 write applies and its reply is lost (LoseUploadReply, LosePublishReply, ApplyThenStale)
	StormEvery time.Duration // 503 storms: every call fails while one lasts
	StormMax   time.Duration //
	CrashEvery time.Duration // process crashes
	RestartMax time.Duration // time from a crash to the restart (uniform from 0)
	ClientLose float64       // the client never sees an answer the agent sent (it retries with the same id)
	// LoseReply also makes a publish land late (durabletest LandLate: no reply, applied up to 10 s later).
}

// Mix is the clients' request mix (weights) and pacing.
type Mix struct {
	Write, BadWrite, Read float64
	Passivate             float64       // an operator passivates the client's database (agentd's admin control)
	Reuse                 float64       // after a failed write, the next write reuses its request id
	Think                 time.Duration // mean pause between a client's requests
}

// settings is a Config with every field decided.
type settings struct {
	Config
	faults    Faults
	policy    vactor.Policy
	mix       Mix
	genCache  int64
	readConns int
}

// settle decides every zero field of c from the seed, with its own stream so the run's streams are unaffected.
// Every knob is drawn whether or not c sets it, so setting one (as Shrink does) leaves the others' draws alone.
func (c Config) settle() settings {
	r := stream(c.Seed, "config")
	pick := func(ds ...time.Duration) time.Duration { return ds[r.IntN(len(ds))] }
	rate := func(ps ...float64) float64 { return ps[r.IntN(len(ps))] }
	on := func() bool { return r.IntN(2) == 0 }

	keys, tick := 1+r.IntN(3), pick(time.Second, 5*time.Second, 15*time.Second)
	policy := vactor.Policy{WriteBackEvery: pick(5*time.Second, 30*time.Second, 2*time.Minute)}
	if commits := uint64(5 + r.IntN(40)); on() {
		policy.WriteBackCommits = commits
	}
	if idle := pick(20*time.Second, time.Minute, 3*time.Minute); on() {
		policy.IdleAfter = idle
	}
	if active := 1 + r.IntN(2); on() {
		policy.MaxActive = active
	}
	var f Faults
	if p, d := rate(0.01, 0.05, 0.2), pick(500*time.Millisecond, 5*time.Second, 40*time.Second); on() {
		f.Stall, f.StallMax = p, d
	}
	if p := rate(0.01, 0.05, 0.2); on() {
		f.FailCall = p
	}
	if p := rate(0.01, 0.05, 0.2); on() {
		f.LoseReply = p
	}
	if every, d := pick(time.Minute, 5*time.Minute), pick(10*time.Second, time.Minute); on() {
		f.StormEvery, f.StormMax = every, d
	}
	if every, d := pick(time.Minute, 4*time.Minute), pick(time.Second, 20*time.Second); on() {
		f.CrashEvery, f.RestartMax = every, d
	}
	if p := rate(0.02, 0.1); on() {
		f.ClientLose = p
	}
	think := pick(200*time.Millisecond, 2*time.Second, 10*time.Second)
	genCache := int64(0)
	if on() {
		genCache = 64 << 20
	}
	readConns := r.IntN(2)

	s := settings{Config: c, faults: f, policy: policy, genCache: genCache, readConns: readConns,
		mix: Mix{Write: 10, BadWrite: 1, Read: 2, Passivate: 0.5, Reuse: 0.5, Think: think}}
	if s.Duration <= 0 {
		s.Duration = 10 * time.Minute
	}
	if s.Keys <= 0 {
		s.Keys = keys
	}
	if s.Workload == nil {
		s.Workload = SQL{}
	}
	if s.Tick <= 0 {
		s.Tick = tick
	}
	if c.Policy != nil {
		s.policy = *c.Policy
	}
	if c.Faults != nil {
		s.faults = *c.Faults
	}
	if s.RPO <= 0 {
		s.RPO = s.policy.WriteBackEvery + s.Tick + time.Second
	}
	return s
}

// explicit is s as a Config with every knob set, so a change to one field changes nothing else.
func (s settings) explicit() Config {
	c := s.Config
	c.Keys, c.Tick, c.Policy, c.Faults = s.Keys, s.Tick, &s.policy, &s.faults
	return c
}

// String is the run's knobs, the first line of every trace.
func (s settings) String() string {
	return fmt.Sprintf("seed=%d keys=%d duration=%v tick=%v policy=%+v rpo=%v faults=%+v mix=%+v gencache=%d readconns=%d workload=%s",
		s.Seed, s.Keys, s.Duration, s.Tick, s.policy, s.RPO, s.faults, s.mix, s.genCache, s.readConns, s.Workload.Name())
}

// stream is the seed's named random stream. Streams are independent of each other and of the order they are
// created in, so a change in how one part draws does not move another part's draws.
func stream(seed uint64, name string) *rand.Rand { return rand.New(det.NewRand(subSeed(seed, name))) }

func subSeed(seed uint64, name string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(name))
	return splitmix(seed ^ h.Sum64())
}

func splitmix(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	return x ^ (x >> 31)
}
