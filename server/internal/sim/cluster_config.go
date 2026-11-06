package sim

import (
	"fmt"
	"time"

	"github.com/nickstrad/mansio/server/internal/vactor"
)

// ClusterConfig is one cluster run (DST-2). Zero fields take values drawn from the seed; Swarm (DST-3) varies
// the fault probabilities per run on top of that.
type ClusterConfig struct {
	Seed     uint64
	Dir      string        `json:"-"` // scratch directory (created)
	Duration time.Duration // simulated time to run; default 5 min
	VMs      int           // agents; 0: 2..4 from the seed
	DBs      int           // databases created at boot; 0: 1..3 from the seed
	Clients  int           // concurrent clients per database; 0: 1..4 from the seed
	Policy   *vactor.Policy
	Tick     time.Duration
	Faults   *ClusterFaults // nil: drawn from the seed
	// RPO is the loss window the checker allows for a kill -9 (the open decision of plan §9, parameterized):
	// an acknowledged write lost at a crash is a violation iff a fault-free stretch at least RPO long fits
	// between its acknowledgement and the crash. 0: Policy.WriteBackEvery + Tick + 1 s (knowledge/writeback).
	// Losses across a graceful move are never allowed.
	RPO time.Duration
	// Swarm, if set, multiplies each fault rate by a per-run factor in [0, 2] drawn from the seed (and turns
	// each class off with probability 1/3): swarm testing (Groce et al.), so runs differ in which faults
	// dominate.
	Swarm bool
	// Scripted replaces the drawn nemesis schedule (create, move, kill, …) with Script, the op script a shrink
	// cuts down (ClusterResult.Script is the schedule a run drew; an empty Script means no operations).
	Scripted bool
	Script   []Op
	// KeepGoing runs to the end after a violation (default: stop at the first one).
	KeepGoing bool `json:"-"`
}

// ClusterFaults are the cluster run's fault rates. Probabilities are per message or per call; Every fields are
// mean gaps of exponentially distributed nemesis operations (0: never).
type ClusterFaults struct {
	// Network, per message (each direction of each call).
	Drop      float64       // a message is lost
	Duplicate float64       // a request is delivered twice (the second copy's answer is dropped)
	Spike     float64       // a message takes up to SpikeMax instead of the usual 0.2–5 ms (reordering)
	SpikeMax  time.Duration //
	// S3 (durabletest, per call), as in DST-1.
	Stall     float64
	StallMax  time.Duration
	FailCall  float64
	LoseReply float64
	// Nemesis operations (mean gaps).
	PartitionEvery time.Duration // cut one node off from some others for up to PartitionMax
	PartitionMax   time.Duration
	KillEvery      time.Duration // kill -9 an agent; it restarts after up to RestartMax, or is attested dead
	RestartMax     time.Duration
	AttestDead     float64       // the share of kills an operator attests dead and restores elsewhere (the VM stays down)
	MoveEvery      time.Duration // move a database (controller.Move, policy-picked target)
	CreateEvery    time.Duration // create a new database (its clients start once it is set up)
	RotateEvery    time.Duration // the issuer's rotation period (a new signing key this often)
	CacheLossEvery time.Duration // the locator loses its cache (the placement listener reconnects: Reset)
	StormEvery     time.Duration // an S3 503 storm
	StormMax       time.Duration
	PGDownEvery    time.Duration // the placement authority (Postgres) is unreachable for up to PGDownMax
	PGDownMax      time.Duration
	Notify         float64 // a placement notification to the locator is lost (the cache stays stale: wrong_owner reroutes)
}

// clusterSettings is a ClusterConfig with every field decided.
type clusterSettings struct {
	ClusterConfig
	faults    ClusterFaults
	policy    vactor.Policy
	think     time.Duration
	readConns int
	keyFetch  time.Duration // how often agents and the edge fetch the issuer's public keys
	ticketTTL time.Duration
}

// settle decides every zero field of c from the seed (stream "cluster-config"); every knob is drawn whether or
// not c sets it, so setting one leaves the others' draws alone.
func (c ClusterConfig) settle() clusterSettings {
	r := stream(c.Seed, "cluster-config")
	pick := func(ds ...time.Duration) time.Duration { return ds[r.IntN(len(ds))] }
	rate := func(ps ...float64) float64 { return ps[r.IntN(len(ps))] }
	on := func() bool { return r.IntN(3) != 0 }

	vms, dbs, clients := 2+r.IntN(3), 1+r.IntN(3), 1+r.IntN(4)
	tick := pick(time.Second, 5*time.Second)
	policy := vactor.Policy{WriteBackEvery: pick(2*time.Second, 10*time.Second, 30*time.Second)}
	if idle := pick(20*time.Second, time.Minute); r.IntN(2) == 0 {
		policy.IdleAfter = idle
	}
	var f ClusterFaults
	if p := rate(0.001, 0.01, 0.05); on() {
		f.Drop = p
	}
	if p := rate(0.001, 0.01, 0.05); on() {
		f.Duplicate = p
	}
	if p, d := rate(0.01, 0.05), pick(200*time.Millisecond, 2*time.Second); on() {
		f.Spike, f.SpikeMax = p, d
	}
	if p, d := rate(0.01, 0.05), pick(500*time.Millisecond, 5*time.Second); on() {
		f.Stall, f.StallMax = p, d
	}
	if p := rate(0.01, 0.05); on() {
		f.FailCall = p
	}
	if p := rate(0.01, 0.05); on() {
		f.LoseReply = p
	}
	if every, d := pick(20*time.Second, time.Minute), pick(2*time.Second, 15*time.Second); on() {
		f.PartitionEvery, f.PartitionMax = every, d
	}
	if every, d := pick(30*time.Second, 2*time.Minute), pick(time.Second, 15*time.Second); on() {
		f.KillEvery, f.RestartMax, f.AttestDead = every, d, rate(0, 0.3)
	}
	if every := pick(10*time.Second, 40*time.Second); on() {
		f.MoveEvery = every
	}
	if every := pick(time.Minute, 3*time.Minute); on() {
		f.CreateEvery = every
	}
	f.RotateEvery = pick(time.Minute, 3*time.Minute)
	if every := pick(20*time.Second, time.Minute); on() {
		f.CacheLossEvery = every
	}
	if every, d := pick(time.Minute, 3*time.Minute), pick(5*time.Second, 20*time.Second); on() {
		f.StormEvery, f.StormMax = every, d
	}
	if every, d := pick(time.Minute, 3*time.Minute), pick(2*time.Second, 10*time.Second); on() {
		f.PGDownEvery, f.PGDownMax = every, d
	}
	if p := rate(0.05, 0.3); on() {
		f.Notify = p
	}
	think := pick(100*time.Millisecond, 500*time.Millisecond, 2*time.Second)
	readConns := r.IntN(2)
	swarm := [17]float64{}
	for i := range swarm {
		swarm[i] = r.Float64() * 2
		if r.IntN(3) == 0 {
			swarm[i] = 0
		}
	}

	s := clusterSettings{ClusterConfig: c, faults: f, policy: policy, think: think, readConns: readConns,
		keyFetch: 10 * time.Second, ticketTTL: 60 * time.Second}
	if s.Duration <= 0 {
		s.Duration = 5 * time.Minute
	}
	if s.VMs <= 0 {
		s.VMs = vms
	}
	if s.DBs <= 0 {
		s.DBs = dbs
	}
	if s.Clients <= 0 {
		s.Clients = clients
	}
	if s.Tick <= 0 {
		s.Tick = tick
	}
	if c.Policy != nil {
		s.policy = *c.Policy
	}
	switch {
	case c.Faults != nil:
		s.faults = *c.Faults
	case c.Swarm:
		s.faults = s.faults.scaled(swarm)
	}
	if s.RPO <= 0 {
		s.RPO = s.policy.WriteBackEvery + s.Tick + time.Second
	}
	return s
}

// scaled is f with each class's rate (or frequency) multiplied by k[i]; 0 turns a class off.
func (f ClusterFaults) scaled(k [17]float64) ClusterFaults {
	p := func(v *float64, i int) { *v = min(*v*k[i], 0.5) }
	every := func(v *time.Duration, i int) {
		if k[i] == 0 {
			*v = 0
		} else if *v > 0 {
			*v = time.Duration(float64(*v) / k[i])
		}
	}
	p(&f.Drop, 0)
	p(&f.Duplicate, 1)
	p(&f.Spike, 2)
	p(&f.Stall, 3)
	p(&f.FailCall, 4)
	p(&f.LoseReply, 5)
	every(&f.PartitionEvery, 6)
	every(&f.KillEvery, 7)
	p(&f.AttestDead, 8)
	every(&f.MoveEvery, 9)
	every(&f.CreateEvery, 10)
	every(&f.CacheLossEvery, 11)
	every(&f.StormEvery, 12)
	every(&f.PGDownEvery, 13)
	p(&f.Notify, 14)
	// The rotation period is a schedule, not a fault: a faster one only (k[15] in [0, 2]).
	if k[15] > 1 {
		f.RotateEvery = time.Duration(float64(f.RotateEvery) / k[15])
	}
	return f
}

// explicit is s as a ClusterConfig with every knob set, so a change to one field changes nothing else.
func (s clusterSettings) explicit() ClusterConfig {
	c := s.ClusterConfig
	c.VMs, c.DBs, c.Clients, c.Tick, c.Policy, c.Faults, c.RPO = s.VMs, s.DBs, s.Clients, s.Tick, &s.policy, &s.faults, s.RPO
	c.Swarm = false
	return c
}

// String is the run's knobs, the first line of every trace.
func (s clusterSettings) String() string {
	return fmt.Sprintf("cluster seed=%d vms=%d dbs=%d clients=%d duration=%v tick=%v policy=%+v rpo=%v faults=%+v think=%v readconns=%d script=%d",
		s.Seed, s.VMs, s.DBs, s.Clients, s.Duration, s.Tick, s.policy, s.RPO, s.faults, s.think, s.readConns, len(s.Script))
}
