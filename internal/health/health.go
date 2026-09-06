// Package health provides the process health mechanism: a concurrency-safe
// registry of named checks that backs the liveness and readiness endpoints.
//
// Phase 0 registers no checks, so readiness reports healthy as soon as the
// process is serving. Later phases register the dependencies they own
// (PostgreSQL, Redis, Kafka) without touching the transport layer.
package health

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Status is the outcome of a single check or of the registry as a whole.
type Status string

const (
	// StatusHealthy means every registered check succeeded.
	StatusHealthy Status = "healthy"
	// StatusUnhealthy means at least one registered check failed.
	StatusUnhealthy Status = "unhealthy"
)

// CheckFunc verifies one dependency. It must honour ctx cancellation and
// return nil when the dependency is usable.
type CheckFunc func(ctx context.Context) error

// Result is the outcome of one named check.
type Result struct {
	Status   Status        `json:"status"`
	Error    string        `json:"error,omitempty"`
	Duration time.Duration `json:"-"`
	// LatencyMS is the serialised form of Duration; milliseconds are the unit
	// operators read in dashboards.
	LatencyMS int64 `json:"latency_ms"`
}

// Report aggregates the results of every registered check.
type Report struct {
	Status Status            `json:"status"`
	Checks map[string]Result `json:"checks"`
}

// Healthy reports whether the aggregate status is healthy.
func (r Report) Healthy() bool { return r.Status == StatusHealthy }

// Failed returns the names of the checks that failed, sorted for stable
// logging and test assertions.
func (r Report) Failed() []string {
	var names []string
	for name, res := range r.Checks {
		if res.Status != StatusHealthy {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

type check struct {
	name string
	fn   CheckFunc
}

// Registry holds the set of dependency checks for the process. The zero value
// is not usable; call New. A Registry is safe for concurrent use: checks are
// typically registered during start-up while the HTTP server is already
// serving readiness probes.
type Registry struct {
	// timeout bounds the whole Check call so one wedged dependency cannot
	// stall a readiness probe indefinitely.
	timeout time.Duration

	mu     sync.RWMutex
	checks []check
}

// DefaultTimeout bounds a full readiness evaluation.
const DefaultTimeout = 2 * time.Second

// New returns an empty registry. A timeout of zero selects DefaultTimeout.
func New(timeout time.Duration) *Registry {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Registry{timeout: timeout}
}

// Register adds a check under name. Registering the same name twice replaces
// the previous check, which keeps start-up wiring idempotent.
func (r *Registry) Register(name string, fn CheckFunc) {
	if fn == nil {
		panic("health: Register called with a nil CheckFunc for " + name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for i := range r.checks {
		if r.checks[i].name == name {
			r.checks[i].fn = fn
			return
		}
	}
	r.checks = append(r.checks, check{name: name, fn: fn})
}

// Names returns the registered check names in registration order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.checks))
	for _, c := range r.checks {
		names = append(names, c.name)
	}
	return names
}

// Len returns the number of registered checks.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.checks)
}

// Check runs every registered check concurrently and aggregates the results.
// An empty registry is healthy. The returned report always contains an entry
// for every registered check, whether it passed or failed.
func (r *Registry) Check(ctx context.Context) Report {
	// Snapshot under the lock so a concurrent Register cannot race the run.
	r.mu.RLock()
	snapshot := make([]check, len(r.checks))
	copy(snapshot, r.checks)
	r.mu.RUnlock()

	report := Report{Status: StatusHealthy, Checks: make(map[string]Result, len(snapshot))}
	if len(snapshot) == 0 {
		return report
	}

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = make(map[string]Result, len(snapshot))
	)

	for _, c := range snapshot {
		wg.Add(1)
		go func(c check) {
			defer wg.Done()

			start := time.Now()
			err := safeRun(ctx, c.fn)
			elapsed := time.Since(start)

			res := Result{
				Status:    StatusHealthy,
				Duration:  elapsed,
				LatencyMS: elapsed.Milliseconds(),
			}
			if err != nil {
				res.Status = StatusUnhealthy
				res.Error = err.Error()
			}

			mu.Lock()
			results[c.name] = res
			mu.Unlock()
		}(c)
	}
	wg.Wait()

	report.Checks = results
	for _, res := range results {
		if res.Status != StatusHealthy {
			report.Status = StatusUnhealthy
			break
		}
	}
	return report
}

// safeRun converts a panicking check into an error so that one broken
// dependency probe cannot take the whole process down.
func safeRun(ctx context.Context, fn CheckFunc) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("check panicked: %v", r)
		}
	}()
	return fn(ctx)
}
