package backend

import (
	"testing"
)

// TestRecordResultStateMachine is the table-driven test for the health state
// machine -- the piece of this project most likely to be subtly wrong.
//
// What it proves, case by case:
//   - a live backend survives fewer than FailThreshold failures
//   - it is ejected on exactly the Nth consecutive failure, not earlier
//   - ONE success anywhere in a failure run resets the count (this is the
//     hysteresis that stops flapping)
//   - a dead backend is not readmitted until PassThreshold consecutive
//     successes
//   - "changed" is reported only on an actual transition, so callers can log
//     transitions rather than one line per backend per interval forever
func TestRecordResultStateMachine(t *testing.T) {
	tests := []struct {
		name          string
		failThreshold int
		passThreshold int
		startAlive    bool
		results       []bool // sequence of probe outcomes
		wantAlive     []bool // expected alive state after each result
		wantChanged   []bool // expected "did it flip" after each result
	}{
		{
			name:          "stays alive below the failure threshold",
			failThreshold: 3,
			passThreshold: 2,
			startAlive:    true,
			results:       []bool{false, false},
			wantAlive:     []bool{true, true},
			wantChanged:   []bool{false, false},
		},
		{
			name:          "ejected on exactly the third consecutive failure",
			failThreshold: 3,
			passThreshold: 2,
			startAlive:    true,
			results:       []bool{false, false, false},
			wantAlive:     []bool{true, true, false},
			wantChanged:   []bool{false, false, true},
		},
		{
			// The anti-flapping case. Without the counter reset, a backend
			// failing every other check would accumulate failures forever and
			// eventually be ejected despite being half healthy.
			name:          "one success resets the failure run",
			failThreshold: 3,
			passThreshold: 1,
			startAlive:    true,
			results:       []bool{false, false, true, false, false},
			wantAlive:     []bool{true, true, true, true, true},
			wantChanged:   []bool{false, false, false, false, false},
		},
		{
			name:          "dead backend needs the full pass threshold",
			failThreshold: 1,
			passThreshold: 3,
			startAlive:    true,
			results:       []bool{false, true, true, true},
			wantAlive:     []bool{false, false, false, true},
			wantChanged:   []bool{true, false, false, true},
		},
		{
			// Symmetric to the flapping case above: a single failure during
			// recovery sends you back to the start of the count.
			name:          "one failure resets the recovery run",
			failThreshold: 1,
			passThreshold: 3,
			startAlive:    true,
			results:       []bool{false, true, true, false, true, true, true},
			wantAlive:     []bool{false, false, false, false, false, false, true},
			wantChanged:   []bool{true, false, false, false, false, false, true},
		},
		{
			name:          "threshold of one flips immediately in both directions",
			failThreshold: 1,
			passThreshold: 1,
			startAlive:    true,
			results:       []bool{false, true, false, true},
			wantAlive:     []bool{false, true, false, true},
			wantChanged:   []bool{true, true, true, true},
		},
		{
			// A healthy backend that keeps passing must not report a
			// transition on every tick, or the logs become useless.
			name:          "repeated successes on a live backend report no change",
			failThreshold: 2,
			passThreshold: 2,
			startAlive:    true,
			results:       []bool{true, true, true},
			wantAlive:     []bool{true, true, true},
			wantChanged:   []bool{false, false, false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New("http://127.0.0.1:9001", 1)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			b.SetAlive(tt.startAlive)

			for i, res := range tt.results {
				changed := b.RecordResult(res, tt.failThreshold, tt.passThreshold)

				if got := b.Alive(); got != tt.wantAlive[i] {
					t.Errorf("after result %d (%v): Alive() = %v, want %v",
						i, res, got, tt.wantAlive[i])
				}
				if changed != tt.wantChanged[i] {
					t.Errorf("after result %d (%v): changed = %v, want %v",
						i, res, changed, tt.wantChanged[i])
				}
			}
		})
	}
}

// TestPoolHealthyFiltersDeadBackends proves the pool hands strategies only
// eligible candidates, so no Strategy ever has to know about health.
func TestPoolHealthyFiltersDeadBackends(t *testing.T) {
	b1, _ := New("http://127.0.0.1:9001", 1)
	b2, _ := New("http://127.0.0.1:9002", 1)
	b3, _ := New("http://127.0.0.1:9003", 1)
	p := NewPool(b1, b2, b3)

	if got := len(p.Healthy()); got != 3 {
		t.Fatalf("all backends start alive: Healthy() = %d, want 3", got)
	}

	b2.SetAlive(false)
	healthy := p.Healthy()
	if len(healthy) != 2 {
		t.Fatalf("Healthy() = %d, want 2", len(healthy))
	}
	for _, b := range healthy {
		if b == b2 {
			t.Error("Healthy() returned a backend marked dead")
		}
	}

	// All() must still report it -- the status command needs to show dead
	// backends, not pretend they vanished.
	if got := len(p.All()); got != 3 {
		t.Errorf("All() = %d, want 3 (dead backends must still be listed)", got)
	}
}

// TestPoolAddRemove covers the admin-API surface, including the error cases
// that the CLI will surface to a human.
func TestPoolAddRemove(t *testing.T) {
	p := NewPool()

	b1, _ := New("http://127.0.0.1:9001", 1)
	if err := p.Add(b1); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Duplicate URL must be rejected, or round robin would silently send
	// double traffic to one backend.
	dup, _ := New("http://127.0.0.1:9001", 5)
	if err := p.Add(dup); err == nil {
		t.Error("Add accepted a duplicate URL")
	}

	if err := p.Remove("http://127.0.0.1:9999"); err == nil {
		t.Error("Remove accepted a URL that is not in the pool")
	}
	if err := p.Remove("http://127.0.0.1:9001"); err != nil {
		t.Errorf("Remove: %v", err)
	}
	if got := p.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0", got)
	}
}

// TestPoolAllReturnsCopy proves the snapshot guarantee that everything else
// depends on. If All() leaked the internal slice, a caller ranging over it
// while the health checker added a backend would be reading a slice whose
// backing array had been reallocated underneath them.
func TestPoolAllReturnsCopy(t *testing.T) {
	b1, _ := New("http://127.0.0.1:9001", 1)
	p := NewPool(b1)

	snapshot := p.All()
	b2, _ := New("http://127.0.0.1:9002", 1)
	if err := p.Add(b2); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if len(snapshot) != 1 {
		t.Errorf("snapshot grew to %d after Add; All() leaked the internal slice", len(snapshot))
	}
	if p.Len() != 2 {
		t.Errorf("pool Len() = %d, want 2", p.Len())
	}
}

// TestInFlightCounter covers the counter Least Connections depends on.
func TestInFlightCounter(t *testing.T) {
	b, _ := New("http://127.0.0.1:9001", 1)

	if got := b.InFlight(); got != 0 {
		t.Fatalf("fresh backend InFlight() = %d, want 0", got)
	}
	b.Acquire()
	b.Acquire()
	if got := b.InFlight(); got != 2 {
		t.Errorf("after 2 Acquire: InFlight() = %d, want 2", got)
	}
	b.Release()
	if got := b.InFlight(); got != 1 {
		t.Errorf("after 1 Release: InFlight() = %d, want 1", got)
	}
}
