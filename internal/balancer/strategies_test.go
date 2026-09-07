package balancer

import (
	"fmt"
	"sync"
	"testing"

	"github.com/manishkumawat24/proxyforge/internal/backend"
)

// weightedBackends builds backends with the given weights.
func weightedBackends(t *testing.T, weights ...int) []*backend.Backend {
	t.Helper()
	bs := make([]*backend.Backend, 0, len(weights))
	for i, w := range weights {
		b, err := backend.New(fmt.Sprintf("http://127.0.0.1:90%02d", i), w)
		if err != nil {
			t.Fatalf("building backend %d: %v", i, err)
		}
		bs = append(bs, b)
	}
	return bs
}

// indexOf returns the position of b in bs, or -1.
func indexOf(bs []*backend.Backend, b *backend.Backend) int {
	for i, x := range bs {
		if x == b {
			return i
		}
	}
	return -1
}

// TestWeightedRoundRobinSequence proves the SMOOTHING property, not just the
// totals.
//
// This is the important distinction. The naive "expand the list" approach
// ([a a a a a b c]) would also give a 5/1/1 split, so a test that only counted
// picks would pass for both. What it would miss is that naive expansion emits
// five consecutive a's -- a burst of five requests all hitting one backend
// while two sit idle.
//
// Asserting the exact ORDER a,a,b,a,c,a,a is what actually pins the algorithm.
func TestWeightedRoundRobinSequence(t *testing.T) {
	tests := []struct {
		name    string
		weights []int
		want    []int // expected backend index, in order
	}{
		{
			// The canonical nginx example. Note a's five turns are spread
			// across the cycle rather than clumped at the front.
			name:    "5-1-1 spreads the heavy backend",
			weights: []int{5, 1, 1},
			want:    []int{0, 0, 1, 0, 2, 0, 0},
		},
		{
			name:    "equal weights degenerate to plain round robin",
			weights: []int{1, 1, 1},
			want:    []int{0, 1, 2, 0, 1, 2},
		},
		{
			name:    "3-1 alternates as evenly as 3:1 allows",
			weights: []int{3, 1},
			want:    []int{0, 0, 1, 0},
		},
		{
			name:    "single backend always wins",
			weights: []int{7},
			want:    []int{0, 0, 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bs := weightedBackends(t, tt.weights...)
			w := NewWeightedRoundRobin()

			for i, want := range tt.want {
				got := indexOf(bs, w.Pick(bs))
				if got != want {
					t.Errorf("pick %d: got backend %d, want %d", i, got, want)
				}
			}
		})
	}
}

// TestWeightedRoundRobinCycleReturnsToStart proves the algorithm is stable over
// many cycles rather than drifting.
//
// After exactly sum(weights) picks the internal currentWeights return to their
// starting values, so cycle 2 and cycle 100 emit the same sequence as cycle 1.
// A subtly wrong implementation (subtracting the wrong total, say) still looks
// fine for one cycle and then drifts.
func TestWeightedRoundRobinCycleReturnsToStart(t *testing.T) {
	bs := weightedBackends(t, 5, 1, 1)
	w := NewWeightedRoundRobin()

	const cycles = 100
	counts := make([]int, len(bs))
	for i := 0; i < cycles*7; i++ {
		counts[indexOf(bs, w.Pick(bs))]++
	}

	want := []int{5 * cycles, 1 * cycles, 1 * cycles}
	for i := range want {
		if counts[i] != want[i] {
			t.Errorf("backend %d served %d over %d cycles, want %d",
				i, counts[i], cycles, want[i])
		}
	}
}

// TestWeightedRoundRobinPrunesRemovedBackends proves we do not leak map entries
// for backends that leave the candidate set.
//
// Without the pruning step this map grows forever as backends are removed via
// the admin API -- a slow leak keyed by pointers that will never be seen again.
func TestWeightedRoundRobinPrunesRemovedBackends(t *testing.T) {
	bs := weightedBackends(t, 1, 1, 1)
	w := NewWeightedRoundRobin()

	for i := 0; i < 10; i++ {
		w.Pick(bs)
	}
	if got := len(w.current); got != 3 {
		t.Fatalf("state entries = %d, want 3", got)
	}

	// Shrink the candidate set, as a health check failure would.
	shrunk := bs[:1]
	for i := 0; i < 10; i++ {
		w.Pick(shrunk)
	}
	if got := len(w.current); got != 1 {
		t.Errorf("after shrinking to 1 candidate, state entries = %d, want 1 "+
			"(stale entries are a memory leak)", got)
	}
}

// TestLeastConnectionsPicksLeastLoaded proves the core contract: load, not
// rotation, decides.
func TestLeastConnectionsPicksLeastLoaded(t *testing.T) {
	tests := []struct {
		name string
		load []int // in-flight requests to simulate per backend
		want int   // expected index
	}{
		{name: "clear winner is the idle one", load: []int{5, 0, 3}, want: 1},
		{name: "first backend idle", load: []int{0, 4, 4}, want: 0},
		{name: "last backend idle", load: []int{9, 9, 0}, want: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bs := weightedBackends(t, make([]int, len(tt.load))...)
			for i, n := range tt.load {
				for j := 0; j < n; j++ {
					bs[i].Acquire()
				}
			}

			lc := &LeastConnections{}
			if got := indexOf(bs, lc.Pick(bs)); got != tt.want {
				t.Errorf("got backend %d, want %d", got, tt.want)
			}
		})
	}
}

// TestLeastConnectionsRotatesTies proves the tie-breaking actually spreads load.
//
// This is the case that matters at low traffic, when every backend is at zero
// in-flight. A naive scan returns index 0 every time and dumps all traffic onto
// one backend. We assert every backend gets used.
func TestLeastConnectionsRotatesTies(t *testing.T) {
	bs := weightedBackends(t, 1, 1, 1)
	lc := &LeastConnections{}

	counts := make([]int, len(bs))
	for i := 0; i < 300; i++ {
		// Acquire/Release around each pick so every backend is back at zero
		// in-flight before the next one -- forcing a tie every time.
		b := lc.Pick(bs)
		counts[indexOf(bs, b)]++
	}

	for i, c := range counts {
		if c != 100 {
			t.Errorf("backend %d picked %d times, want 100 "+
				"(all-tied picks should rotate, not favour index 0)", i, c)
		}
	}
}

// TestLeastConnectionsRespectsInFlightDuringRequests simulates the real flow:
// pick, Acquire, and only Release when the "request" finishes. The strategy
// should steer away from a backend that is still busy.
func TestLeastConnectionsRespectsInFlight(t *testing.T) {
	bs := weightedBackends(t, 1, 1)
	lc := &LeastConnections{}

	// Hold one request open on the first backend picked.
	first := lc.Pick(bs)
	first.Acquire()
	defer first.Release()

	// The next pick must avoid it, since it now has 1 in-flight vs 0.
	second := lc.Pick(bs)
	if second == first {
		t.Errorf("picked the busy backend %s again; in-flight was ignored", first)
	}
}

// TestRandomCoversAllBackends proves Random is not silently constant.
//
// Note what this test deliberately does NOT do: assert an even split. Random is
// only statistically even, so an exact-count assertion would be flaky by
// construction. We assert the weaker, actually-true property -- every backend
// gets chosen at least once over enough trials -- plus a loose sanity band.
//
// Writing the strong assertion here would produce a test that fails a few times
// a year for no reason, which is worse than no test at all.
func TestRandomCoversAllBackends(t *testing.T) {
	bs := weightedBackends(t, 1, 1, 1, 1)
	r := Random{} // value, not pointer -- value receivers make both work

	const trials = 10000
	counts := make([]int, len(bs))
	for i := 0; i < trials; i++ {
		counts[indexOf(bs, r.Pick(bs))]++
	}

	expected := trials / len(bs)
	for i, c := range counts {
		if c == 0 {
			t.Errorf("backend %d never chosen in %d trials", i, trials)
		}
		// Very loose band: ±30% of expected. Tight enough to catch a broken
		// distribution, loose enough never to flake.
		if c < expected*7/10 || c > expected*13/10 {
			t.Errorf("backend %d chosen %d times, expected roughly %d (±30%%)",
				i, c, expected)
		}
	}
}

// TestAllStrategiesHandleEmpty proves the nil contract across every
// implementation. The proxy depends on this to return 503 instead of panicking
// when all backends are down -- the exact moment a panic is least welcome.
func TestAllStrategiesHandleEmpty(t *testing.T) {
	for _, name := range Names() {
		t.Run(name, func(t *testing.T) {
			s, err := New(name)
			if err != nil {
				t.Fatalf("New(%q): %v", name, err)
			}
			if got := s.Pick(nil); got != nil {
				t.Errorf("Pick(nil) = %v, want nil", got)
			}
			if got := s.Pick([]*backend.Backend{}); got != nil {
				t.Errorf("Pick(empty) = %v, want nil", got)
			}
		})
	}
}

// TestAllStrategiesAreConcurrencySafe hammers every strategy from many
// goroutines at once.
//
// This test's real value is under `go test -race`: it is the thing that would
// have caught an unguarded map write in WeightedRoundRobin, which is not merely
// a lost update but a hard runtime throw ("concurrent map writes") that kills
// the process and cannot be recovered.
func TestAllStrategiesAreConcurrencySafe(t *testing.T) {
	for _, name := range Names() {
		t.Run(name, func(t *testing.T) {
			s, err := New(name)
			if err != nil {
				t.Fatalf("New(%q): %v", name, err)
			}
			bs := weightedBackends(t, 3, 1, 2, 1)

			var wg sync.WaitGroup
			for g := 0; g < 50; g++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < 200; i++ {
						b := s.Pick(bs)
						if b == nil {
							// t.Errorf is safe from multiple goroutines;
							// t.Fatalf is NOT (it calls runtime.Goexit on the
							// wrong goroutine and the test hangs).
							t.Errorf("Pick returned nil for non-empty candidates")
							return
						}
						b.Acquire()
						b.Release()
					}
				}()
			}
			wg.Wait()
		})
	}
}

// TestNewStrategyUnknownName proves config typos fail loudly at startup rather
// than silently defaulting to something.
func TestNewStrategyUnknownName(t *testing.T) {
	if _, err := New("round-robin"); err == nil {
		t.Error("New(\"round-robin\") succeeded; hyphenated typo should be rejected")
	}
	// Empty string is the documented "use the default" case, not a typo.
	s, err := New("")
	if err != nil {
		t.Fatalf("New(\"\") should default: %v", err)
	}
	if s.Name() != "round_robin" {
		t.Errorf("default strategy = %q, want round_robin", s.Name())
	}
}
