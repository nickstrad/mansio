package durable

// Request accounting (OS-08): a Store that can count the requests it sends implements RequestCounter, and
// RequestCost turns the counts into dollars at S3 list prices (object-storage guide: report cost with
// performance). The counts are attempts, retries included: what the provider bills.

// Operation names every counting Store uses. One name per kind of request; a write-back is
// OpPutGen + OpHead + OpGetManifest + OpPublish (four requests).
const (
	OpGetManifest = "get_manifest" // GET manifest.json
	OpFetch       = "fetch"        // GET a generation
	OpPutGen      = "put_gen"      // PUT a generation (create-only)
	OpPublish     = "publish"      // PUT manifest.json (conditional)
	OpHead        = "head"         // HEAD a generation
	OpList        = "list"         // one listing page
	OpDelete      = "delete"       // one batch delete
)

// RequestCounter is implemented by a Store that counts its requests.
type RequestCounter interface {
	// RequestCounts returns requests sent since the Store was created, by operation name (Op* or an
	// adapter's own names). Ops with no requests may be missing. A new map each call.
	RequestCounts() map[string]int64
}

// Prices are per-request prices in US dollars.
type Prices struct {
	ClassA float64 // PUT, COPY, POST, LIST
	ClassB float64 // GET, HEAD and everything else that is not free
}

// S3Standard is AWS S3 Standard list pricing (us-east-1): $0.005 per 1000 class-A requests, $0.0004 per 1000
// class-B. DELETE (including the multi-object POST delete) is free.
var S3Standard = Prices{ClassA: 0.005 / 1000, ClassB: 0.0004 / 1000}

// classA are the operations billed as class A; OpDelete is free; any other name is class B.
var classA = map[string]bool{OpPutGen: true, OpPublish: true, OpList: true, "put": true} // "put": objstore.OpPut

// RequestCost is the price of counts at p.
func RequestCost(counts map[string]int64, p Prices) float64 {
	var usd float64
	for op, n := range counts {
		switch {
		case op == OpDelete:
		case classA[op]:
			usd += float64(n) * p.ClassA
		default:
			usd += float64(n) * p.ClassB
		}
	}
	return usd
}

// Requests sums counts.
func Requests(counts map[string]int64) int64 {
	var n int64
	for _, c := range counts {
		n += c
	}
	return n
}

// RequestCounts forwards to the guarded store, if it counts; otherwise nil.
func (g *guarded) RequestCounts() map[string]int64 {
	if c, ok := g.s.(RequestCounter); ok {
		return c.RequestCounts()
	}
	return nil
}
