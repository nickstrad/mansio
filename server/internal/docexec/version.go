package docexec

import (
	"fmt"

	"github.com/nickstrad/mansio/server/internal/doccompile"
	"github.com/nickstrad/mansio/server/internal/ir"
)

// The version floor (spec §6.3). A document's _version is 1 on insert and one more with each patch, but never
// below the floor of the database's incarnation: a write sets max(that, floor).
//
// Why: a database restored from an older generation (a kill -9 inside the RPO) has lost its last writes, and
// counting on from the restored numbers would issue the lost writes' versions again, for other contents. A
// request that read a document before the crash would then pass its expect {version} against a different
// document: silent corruption (sim swarm seed 2761, testdata/cluster/version-reuse-after-rollback.json). With
// the floor, every version a restored database issues is above every version any earlier incarnation issued,
// so a version names one content of a document for good.
//
// The incarnation is the manifest's seal at activation (durable.Manifest.Seal, through vactor.Opener and
// actor.Tx.Incarnation): every stamp raises it, an owner stamps before it serves a restored database, and a
// new generation keeps it. The open-issues note proposed max(prev+1, seal<<32); that is what this is, after
// one change it needed: the seal used to restart at 0 with each generation, so two incarnations could share
// one (restore G, stamp to 1, publish G+1, crash, restore G+1, stamp to 1). Generation and epoch cannot stand
// in for it: packed beside the seal they leave no room for the stride in 53 bits.
//
// The numbers: versions are JSON numbers, so they end at 2^53 - 1. The stride of 2^32 leaves 2^21 stamps
// (restarts and moves) in a database's life; past that, and past 2^53 - 1 by any road, a write fails with a
// StorageError instead of issuing a version a client cannot hold. An incarnation would have to patch one
// document 2^32 times, and then lose the last of them, to reach the next one's floor.
//
// Cost: one more UPDATE for the first write to a document in an incarnation that is not the database's first
// (the compiled insert and replace are unchanged: core/compile stays the oracle, ADR 0010). With incarnation 0
// (no object store: the file is never rolled back) there is no floor and nothing changes.
const versionStride = 32

// VersionFloor is the lowest _version a database under incarnation issues.
func VersionFloor(incarnation uint64) float64 {
	if incarnation >= 1<<(53-versionStride) {
		return ir.MaxSafeInteger + 1 // spent: version refuses every write
	}
	return float64(incarnation << versionStride)
}

// version settles the _version of the document a write just left at v (1 after an insert, the old version
// plus 1 after a patch): v, or the floor when that is higher, which it then writes.
func (r *run) version(table, id string, v float64) (float64, error) {
	if v >= r.st.Floor {
		if v > ir.MaxSafeInteger {
			return 0, storage(fmt.Errorf("docexec: %s %s is at the last _version (2^53 - 1); it cannot be written again", table, id))
		}
		return v, nil
	}
	if r.st.Floor > ir.MaxSafeInteger {
		return 0, storage(fmt.Errorf("docexec: the database has been stamped 2^%d times: no _version is left to issue; nothing written", 53-versionStride))
	}
	target, e := doccompile.Quote(table)
	if e != nil {
		return 0, protocol(e)
	}
	if _, err := r.tx.Exec("UPDATE "+target+` SET "_version" = ? WHERE "_id" = ?`, r.st.Floor, id); err != nil {
		return 0, storage(err)
	}
	return r.st.Floor, nil
}
