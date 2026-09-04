package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"multiderp/internal/verifier"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

var (
	ErrClosed     = errors.New("admission controller is closed")
	ErrOverloaded = errors.New("admission controller is overloaded")
)

type Limits struct {
	RequestTimeout          time.Duration
	PerVerifierTimeout      time.Duration
	MaxConcurrentAdmissions int
	MaxConcurrentQueries    int
	MaxQueuedJobs           int
}

func DefaultLimits() Limits {
	return Limits{
		RequestTimeout:          4 * time.Second,
		PerVerifierTimeout:      2 * time.Second,
		MaxConcurrentAdmissions: 64,
		MaxConcurrentQueries:    32,
		MaxQueuedJobs:           256,
	}
}

type poolEntry struct {
	name       string
	verifier   verifier.Verifier
	generation uint64
	active     bool
	inFlight   int
}

type Pool struct {
	mu      sync.Mutex
	cond    *sync.Cond
	entries map[string]*poolEntry
	next    uint64
}

func NewPool() *Pool {
	p := &Pool{entries: make(map[string]*poolEntry)}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *Pool) Upsert(name string, v verifier.Verifier) {
	if v == nil {
		return
	}
	p.mu.Lock()
	if old := p.entries[name]; old != nil && old.active && old.verifier == v {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	p.Remove(name)
	p.mu.Lock()
	p.next++
	p.entries[name] = &poolEntry{name: name, verifier: v, generation: p.next, active: true}
	p.mu.Unlock()
}

func (p *Pool) Remove(name string) {
	p.mu.Lock()
	entry := p.entries[name]
	if entry == nil {
		p.mu.Unlock()
		return
	}
	entry.active = false
	delete(p.entries, name)
	for entry.inFlight > 0 {
		p.cond.Wait()
	}
	p.mu.Unlock()
}

func (p *Pool) Clear() {
	p.mu.Lock()
	names := make([]string, 0, len(p.entries))
	for name := range p.entries {
		names = append(names, name)
	}
	p.mu.Unlock()
	for _, name := range names {
		p.Remove(name)
	}
}

func (p *Pool) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

func (p *Pool) Contains(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.entries[name]
	return entry != nil && entry.active
}

type snapshotEntry struct {
	entry *poolEntry
}

func (p *Pool) snapshot() []snapshotEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]snapshotEntry, 0, len(p.entries))
	for _, entry := range p.entries {
		if entry.active {
			result = append(result, snapshotEntry{entry: entry})
		}
	}
	return result
}

func (p *Pool) begin(entry *poolEntry) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !entry.active || p.entries[entry.name] != entry || p.entries[entry.name].generation != entry.generation {
		return false
	}
	entry.inFlight++
	return true
}

func (p *Pool) end(entry *poolEntry) {
	p.mu.Lock()
	if entry.inFlight > 0 {
		entry.inFlight--
	}
	p.cond.Broadcast()
	p.mu.Unlock()
}

func (p *Pool) current(entry *poolEntry) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return entry.active && p.entries[entry.name] == entry && p.entries[entry.name].generation == entry.generation
}

type queryJob struct {
	ctx       context.Context
	node      key.NodePublic
	candidate snapshotEntry
	result    chan<- queryResult
}

type queryResult struct {
	candidate snapshotEntry
	allow     bool
	err       error
}

type Controller struct {
	pool            *Pool
	limits          Limits
	jobs            chan queryJob
	stop            chan struct{}
	workers         sync.WaitGroup
	requestSlots    chan struct{}
	closed          atomic.Bool
	barrier         atomic.Bool
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
}

func NewController(pool *Pool, limits Limits) *Controller {
	if pool == nil {
		pool = NewPool()
	}
	defaults := DefaultLimits()
	if limits.RequestTimeout <= 0 {
		limits.RequestTimeout = defaults.RequestTimeout
	}
	if limits.PerVerifierTimeout <= 0 {
		limits.PerVerifierTimeout = defaults.PerVerifierTimeout
	}
	if limits.MaxConcurrentAdmissions <= 0 {
		limits.MaxConcurrentAdmissions = defaults.MaxConcurrentAdmissions
	}
	if limits.MaxConcurrentQueries <= 0 {
		limits.MaxConcurrentQueries = defaults.MaxConcurrentQueries
	}
	if limits.MaxQueuedJobs <= 0 {
		limits.MaxQueuedJobs = defaults.MaxQueuedJobs
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	c := &Controller{
		pool:            pool,
		limits:          limits,
		jobs:            make(chan queryJob, limits.MaxQueuedJobs),
		stop:            make(chan struct{}),
		requestSlots:    make(chan struct{}, limits.MaxConcurrentAdmissions),
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
	}
	c.barrier.Store(true)
	for i := 0; i < limits.MaxConcurrentQueries; i++ {
		c.workers.Add(1)
		go c.worker()
	}
	return c
}

func (c *Controller) Pool() *Pool {
	return c.pool
}

func (c *Controller) SetBarrier(allow bool) {
	c.barrier.Store(allow)
}

func (c *Controller) Running() bool {
	return !c.closed.Load()
}

func (c *Controller) Close() {
	if c.closed.Swap(true) {
		return
	}
	c.lifecycleCancel()
	close(c.stop)
	c.workers.Wait()
}

func (c *Controller) worker() {
	defer c.workers.Done()
	for {
		select {
		case <-c.stop:
			c.drainCanceledJobs()
			return
		case job := <-c.jobs:
			c.runJob(job)
		}
	}
}

func (c *Controller) drainCanceledJobs() {
	for {
		select {
		case job := <-c.jobs:
			job.result <- queryResult{candidate: job.candidate, err: ErrClosed}
		default:
			return
		}
	}
}

func (c *Controller) runJob(job queryJob) {
	if job.ctx.Err() != nil {
		job.result <- queryResult{candidate: job.candidate, err: job.ctx.Err()}
		return
	}
	if !c.pool.begin(job.candidate.entry) {
		job.result <- queryResult{candidate: job.candidate, err: errors.New("verifier is no longer eligible")}
		return
	}
	defer c.pool.end(job.candidate.entry)

	queryTimeout := c.limits.PerVerifierTimeout
	if deadline, ok := job.ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < queryTimeout {
			queryTimeout = remaining
		}
	}
	if queryTimeout <= 0 {
		job.result <- queryResult{candidate: job.candidate, err: context.DeadlineExceeded}
		return
	}
	queryCtx, cancel := context.WithTimeout(job.ctx, queryTimeout)
	defer cancel()
	allow, err := containsNode(queryCtx, job.candidate.entry.verifier, job.node)
	if err != nil || !allow {
		allow = false
	}
	job.result <- queryResult{candidate: job.candidate, allow: allow, err: err}
}

func containsNode(ctx context.Context, v verifier.Verifier, node key.NodePublic) (allow bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			allow = false
			err = fmt.Errorf("verifier panic recovered: %v", recovered)
		}
	}()
	return v.ContainsNode(ctx, node)
}

func (c *Controller) Admit(ctx context.Context, node key.NodePublic, _ netip.Addr) (bool, error) {
	if c.closed.Load() {
		return false, ErrClosed
	}
	if !c.barrier.Load() {
		return false, nil
	}
	select {
	case c.requestSlots <- struct{}{}:
		defer func() { <-c.requestSlots }()
	default:
		return false, ErrOverloaded
	}

	requestCtx, cancel := context.WithTimeout(ctx, c.limits.RequestTimeout)
	stopRequest := context.AfterFunc(c.lifecycleCtx, cancel)
	defer stopRequest()
	defer cancel()
	candidates := c.pool.snapshot()
	if len(candidates) == 0 {
		return false, nil
	}
	results := make(chan queryResult, len(candidates))
	submitted := 0
	closed := false
	for _, candidate := range candidates {
		job := queryJob{ctx: requestCtx, node: node, candidate: candidate, result: results}
		select {
		case c.jobs <- job:
			submitted++
		case <-requestCtx.Done():
			break
		case <-c.stop:
			closed = true
		}
		if closed || requestCtx.Err() != nil {
			break
		}
	}
	if submitted == 0 {
		if closed || c.closed.Load() {
			return false, ErrClosed
		}
		if err := requestCtx.Err(); err != nil {
			return false, err
		}
		return false, ErrOverloaded
	}

	allowed := false
	winners := make([]*poolEntry, 0, 1)
	for i := 0; i < submitted; i++ {
		result := <-results
		if result.err == nil && result.allow && c.pool.current(result.candidate.entry) {
			allowed = true
			winners = append(winners, result.candidate.entry)
			cancel()
		}
	}
	if c.closed.Load() {
		return false, ErrClosed
	}
	if !c.barrier.Load() {
		return false, nil
	}
	if allowed {
		for _, winner := range winners {
			if c.pool.current(winner) {
				return true, nil
			}
		}
		return false, nil
	}
	return allowed, nil
}

func jsonDecoder(r io.Reader) *json.Decoder {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	return decoder
}

func writeJSON(w io.Writer, value any) error {
	return json.NewEncoder(w).Encode(value)
}

func (c *Controller) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admit" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		const maxRequestBody = 4 << 10
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		var request tailcfg.DERPAdmitClientRequest
		decoder := jsonDecoder(r.Body)
		if err := decoder.Decode(&request); err != nil {
			http.Error(w, "invalid admission request", http.StatusBadRequest)
			return
		}
		var extra struct{}
		if err := decoder.Decode(&extra); err != io.EOF {
			http.Error(w, "invalid admission request", http.StatusBadRequest)
			return
		}
		allow, err := c.Admit(r.Context(), request.NodePublic, request.Source)
		if err != nil && !errors.Is(err, ErrOverloaded) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			http.Error(w, "admission unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = writeJSON(w, tailcfg.DERPAdmitClientResponse{Allow: allow})
	})
}
