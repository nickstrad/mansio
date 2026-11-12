package control

import (
	"cmp"
	"slices"
	"time"
)

// Policy is the placement policy (mansio's open decision M8, made here; docs/notes/front/controld.md):
// put a new or moving database on the least-loaded live VM, by active databases and bytes, under a cap.
//
//   - Live: heartbeat younger than MaxHeartbeatAge (and not attested dead: Load leaves those out).
//   - Under the cap: ActiveDBs < MaxDatabases and Bytes < MaxBytes (a zero cap is no cap).
//   - Least loaded: the smallest dominant share max(ActiveDBs/MaxDatabases, Bytes/MaxBytes) over the caps
//     that are set, then fewer databases, then fewer bytes, then the VM id (deterministic).
//
// Databases are the primary measure because a warm database costs a VM memory, file handles and maintenance
// whatever its size; bytes catch the few large ones. No randomness: two controllers given the same load pick
// the same VM, which keeps tests and reasoning simple; the CAS on the authority keeps racing creates safe.
type Policy struct {
	MaxDatabases    int64
	MaxBytes        int64
	MaxHeartbeatAge time.Duration // 0: every VM Load returns counts as live
}

// Pick returns the VM to place on, never one in exclude. ErrNoCapacity if none qualifies.
//
//	post: err == nil ⇒ the result is the minimum of the eligible VMs under the order above
func (p Policy) Pick(loads []VMLoad, exclude ...string) (string, error) {
	var best *VMLoad
	var bestShare float64
	for i := range loads {
		l := &loads[i]
		if !p.eligible(*l) || slices.Contains(exclude, l.VM) {
			continue
		}
		s := p.share(*l)
		if best == nil || p.less(s, *l, bestShare, *best) {
			best, bestShare = l, s
		}
	}
	if best == nil {
		return "", ErrNoCapacity
	}
	return best.VM, nil
}

func (p Policy) eligible(l VMLoad) bool {
	switch {
	case p.MaxHeartbeatAge > 0 && l.HeartbeatAge > p.MaxHeartbeatAge:
		return false
	case p.MaxDatabases > 0 && l.ActiveDBs >= p.MaxDatabases:
		return false
	case p.MaxBytes > 0 && l.Bytes >= p.MaxBytes:
		return false
	}
	return true
}

// share is l's dominant share of the caps that are set (0 when none is).
func (p Policy) share(l VMLoad) float64 {
	s := 0.0
	if p.MaxDatabases > 0 {
		s = float64(l.ActiveDBs) / float64(p.MaxDatabases)
	}
	if p.MaxBytes > 0 {
		s = max(s, float64(l.Bytes)/float64(p.MaxBytes))
	}
	return s
}

func (p Policy) less(sa float64, a VMLoad, sb float64, b VMLoad) bool {
	return cmp.Or(cmp.Compare(sa, sb), cmp.Compare(a.ActiveDBs, b.ActiveDBs), cmp.Compare(a.Bytes, b.Bytes), cmp.Compare(a.VM, b.VM)) < 0
}
