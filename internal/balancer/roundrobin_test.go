package balancer

import (
	"fmt"
	"sync"
	"testing"

	"github.com/manishkumawat24/proxyforge/internal/backend"
)

// mustBackends builds n throwaway backends named http://127.0.0.1:900{i}.
//
// t.Helper() marks this as a test helper, so when it calls t.Fatalf the failure
// is reported at the CALLER's line number rather than inside here. Without it
// every failure points at this function and tells you nothing.
func mustBackends(t *testing.T, n int) []*backend.Backend {
	t.Helper()
	bs := make([]*backend.Backend, 0, n)
	for i := 0; i < n; i++ {
		b, err := backend.New(fmt.Sprintf("http://127.0.0.1:900%d", i), 1)
		if err != nil {
			t.Fatalf("building backend %d: %v", i, err)
		}
		bs = append(bs, b)
	}
	return bs
}

// TestRoundRobinOrder proves the core contract: strict rotation, wrapping back
// to index 0, with no backend favoured or skipped.
//
// This is the table-driven style that is idiomatic in Go: the test cases are
// DATA in a slice of anonymous structs, and one loop body exercises all of
// them. Adding a case is adding a line, not copy-pasting a function.
//
// C++ contrast: this is what you would reach for TEST_P / INSTANTIATE_TEST_SUITE_P
// in GoogleTest to get. Go needs no framework because a slice literal and a
// range loop already do the job.
func TestRoundRobinOrder(t *testing.T) {
	tests := []struct {
		name      string
		backends  int
		picks     int
		wantOrder []int // expected backend INDEX for each successive pick
	}{
		{
			name:      "three backends wrap cleanly",
			backends:  3,
			picks:     7,
			wantOrder: []int{0, 1, 2, 0, 1, 2, 0},
		},
		{
			name:      "single backend always chosen",
			backends:  1,
			picks:     4,
			wantOrder: []int{0, 0, 0, 0},
		},
		{
			name:      "two backends alternate",
			backends:  2,
			picks:     5,
			wantOrder: []int{0, 1, 0, 1, 0},
		},
	}

	for _, tt := range tests {
		// t.Run creates a subtest, so a failure names the exact case and you
		// can re-run just it with -run 'TestRoundRobinOrder/two_backends'.
		t.Run(tt.name, func(t *testing.T) {
			bs := mustBackends(t, tt.backends)
			rr := &RoundRobin{} // zero value is usable -- no constructor

			for i, want := range tt.wantOrder {
				got := rr.Pick(bs)
				if got != bs[want] {
					t.Errorf("pick %d: got %s, want %s", i, got, bs[want])
				}
			}
		})
	}
}

// TestRoundRobinEmpty proves the nil contract. The proxy relies on this to
// return 503 rather than panicking when every backend is down -- which is
// exactly the moment you least want a panic.
func TestRoundRobinEmpty(t *testing.T) {
	rr := &RoundRobin{}
	if got := rr.Pick(nil); got != nil {
		t.Errorf("Pick(nil) = %v, want nil", got)
	}
	if got := rr.Pick([]*backend.Backend{}); got != nil {
		t.Errorf("Pick(empty) = %v, want nil", got)
	}
}

// TestRoundRobinConcurrent is the test that justifies the atomic counter.
//
// What it actually proves: across 100 goroutines making 100 picks each, the
// distribution is PERFECTLY even -- exactly 2500 requests to each of 4
// backends. That exactness is the point. A plain `n++` would lose increments to
// lost updates and the counts would come out lumpy.
//
// More importantly, run under `go test -race` this test fails LOUDLY on a
// non-atomic counter, because the race detector instruments every memory access
// and spots two goroutines touching the same word with no synchronization
// between them. Without -race it might well pass by luck, which is precisely
// why "it worked when I ran it" is worthless evidence for concurrent code.
func TestRoundRobinConcurrent(t *testing.T) {
	const (
		goroutines   = 100
		picksEach    = 100
		backendCount = 4
	)

	bs := mustBackends(t, backendCount)
	rr := &RoundRobin{}

	// Each goroutine tallies into its OWN slice, so the test harness itself
	// introduces no shared mutable state. If we incremented a shared map here
	// we would be testing our own broken test instead of the strategy.
	counts := make([][]int, goroutines)

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		counts[g] = make([]int, backendCount)
		wg.Add(1) // MUST be before `go`, not inside it -- otherwise Wait can
		// observe a zero counter and return before anything started.
		go func(g int) {
			defer wg.Done()
			for i := 0; i < picksEach; i++ {
				got := rr.Pick(bs)
				for idx, b := range bs {
					if b == got {
						counts[g][idx]++
						break
					}
				}
			}
		}(g)
	}
	wg.Wait()

	total := make([]int, backendCount)
	for g := range counts {
		for idx, c := range counts[g] {
			total[idx] += c
		}
	}

	want := goroutines * picksEach / backendCount
	for idx, got := range total {
		if got != want {
			t.Errorf("backend %d served %d requests, want exactly %d "+
				"(uneven distribution means increments were lost)", idx, got, want)
		}
	}
}
