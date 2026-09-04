package admission

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lsy223622/MultiDERP/internal/verifier"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

type fakeVerifier struct {
	mu       sync.Mutex
	name     string
	state    verifier.State
	hardened bool
	contains func(context.Context, key.NodePublic) (bool, error)
	closed   bool
}

func (f *fakeVerifier) Name() string { return f.name }

func (f *fakeVerifier) State() verifier.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeVerifier) ContainsNode(ctx context.Context, node key.NodePublic) (bool, error) {
	return f.contains(ctx, node)
}

func (f *fakeVerifier) Status(context.Context) verifier.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return verifier.Status{
		Name:              f.name,
		State:             f.state,
		HardeningVerified: f.hardened,
	}
}

func (f *fakeVerifier) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func newFake(name string, contains func(context.Context, key.NodePublic) (bool, error)) *fakeVerifier {
	return &fakeVerifier{name: name, state: verifier.StateConnected, hardened: true, contains: contains}
}

func testNode() key.NodePublic {
	return key.NewNode().Public()
}

func TestAdmitAllowsAnyExplicitMatch(t *testing.T) {
	pool := NewPool()
	pool.Upsert("alice", newFake("alice", func(context.Context, key.NodePublic) (bool, error) {
		return false, nil
	}))
	pool.Upsert("bob", newFake("bob", func(context.Context, key.NodePublic) (bool, error) {
		return true, nil
	}))
	c := NewController(pool, Limits{RequestTimeout: time.Second, PerVerifierTimeout: time.Second, MaxConcurrentQueries: 2, MaxQueuedJobs: 2})
	defer c.Close()

	allow, err := c.Admit(context.Background(), testNode(), keyAddr())
	if err != nil || !allow {
		t.Fatalf("Admit() = %v, %v; want true, nil", allow, err)
	}
}

func TestAdmitDeniesErrorsUnknownAndPanics(t *testing.T) {
	pool := NewPool()
	pool.Upsert("error", newFake("error", func(context.Context, key.NodePublic) (bool, error) {
		return false, errors.New("local API failed")
	}))
	pool.Upsert("panic", newFake("panic", func(context.Context, key.NodePublic) (bool, error) {
		panic("unexpected verifier failure")
	}))
	c := NewController(pool, Limits{RequestTimeout: time.Second, PerVerifierTimeout: time.Second, MaxConcurrentQueries: 2, MaxQueuedJobs: 2})
	defer c.Close()

	allow, err := c.Admit(context.Background(), testNode(), keyAddr())
	if err != nil || allow {
		t.Fatalf("Admit() = %v, %v; want false, nil", allow, err)
	}
}

func TestLateSuccessAfterRemovalIsDenied(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	v := newFake("alice", func(ctx context.Context, _ key.NodePublic) (bool, error) {
		close(entered)
		select {
		case <-release:
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	})
	pool := NewPool()
	pool.Upsert("alice", v)
	c := NewController(pool, Limits{RequestTimeout: time.Second, PerVerifierTimeout: time.Second, MaxConcurrentQueries: 1, MaxQueuedJobs: 1})
	defer c.Close()

	type result struct {
		allow bool
		err   error
	}
	resultCh := make(chan result, 1)
	go func() {
		allow, err := c.Admit(context.Background(), testNode(), keyAddr())
		resultCh <- result{allow: allow, err: err}
	}()
	<-entered
	removed := make(chan struct{})
	go func() {
		pool.Remove("alice")
		close(removed)
	}()
	select {
	case <-removed:
		t.Fatal("pool removal completed before the in-flight query finished")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-removed:
	case <-time.After(time.Second):
		t.Fatal("pool removal did not join the in-flight query")
	}
	got := <-resultCh
	if got.err != nil || got.allow {
		t.Fatalf("late admission result = %v, %v; want false, nil", got.allow, got.err)
	}
}

func TestOtherCurrentSuccessSurvivesWinnerRemoval(t *testing.T) {
	aliceRelease := make(chan struct{})
	secondEntered := make(chan struct{})
	releaseSecond := make(chan struct{})
	pool := NewPool()
	pool.Upsert("alice", newFake("alice", func(context.Context, key.NodePublic) (bool, error) {
		<-aliceRelease
		return true, nil
	}))
	pool.Upsert("bob", newFake("bob", func(context.Context, key.NodePublic) (bool, error) {
		close(secondEntered)
		<-releaseSecond
		return true, nil
	}))
	c := NewController(pool, Limits{RequestTimeout: time.Second, PerVerifierTimeout: time.Second, MaxConcurrentQueries: 2, MaxQueuedJobs: 2})
	defer c.Close()

	resultCh := make(chan resultValue, 1)
	go func() {
		allow, err := c.Admit(context.Background(), testNode(), keyAddr())
		resultCh <- resultValue{allow: allow, err: err}
	}()
	<-secondEntered
	close(aliceRelease)
	pool.Remove("alice")
	close(releaseSecond)
	got := <-resultCh
	if got.err != nil || !got.allow {
		t.Fatalf("admission result after first winner removal = %v, %v; want true, nil", got.allow, got.err)
	}
}

func TestSuccessAfterAdmissionBarrierFallsIsDenied(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	pool := NewPool()
	pool.Upsert("alice", newFake("alice", func(ctx context.Context, _ key.NodePublic) (bool, error) {
		close(entered)
		select {
		case <-release:
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}))
	c := NewController(pool, Limits{RequestTimeout: time.Second, PerVerifierTimeout: time.Second, MaxConcurrentQueries: 1, MaxQueuedJobs: 1})
	defer c.Close()

	resultCh := make(chan resultValue, 1)
	go func() {
		allow, err := c.Admit(context.Background(), testNode(), keyAddr())
		resultCh <- resultValue{allow: allow, err: err}
	}()
	<-entered
	c.SetBarrier(false)
	close(release)
	got := <-resultCh
	if got.err != nil || got.allow {
		t.Fatalf("barrier-raced admission result = %v, %v; want false, nil", got.allow, got.err)
	}
}

func TestAdmissionRequestOverloadFailsClosed(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	v := newFake("alice", func(ctx context.Context, _ key.NodePublic) (bool, error) {
		close(entered)
		select {
		case <-release:
			return false, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	})
	pool := NewPool()
	pool.Upsert("alice", v)
	c := NewController(pool, Limits{
		RequestTimeout:          time.Second,
		PerVerifierTimeout:      time.Second,
		MaxConcurrentAdmissions: 1,
		MaxConcurrentQueries:    1,
		MaxQueuedJobs:           1,
	})
	defer c.Close()

	first := make(chan resultValue, 1)
	go func() {
		allow, err := c.Admit(context.Background(), testNode(), keyAddr())
		first <- resultValue{allow: allow, err: err}
	}()
	<-entered
	allow, err := c.Admit(context.Background(), testNode(), keyAddr())
	if allow || !errors.Is(err, ErrOverloaded) {
		t.Fatalf("overloaded Admit() = %v, %v; want false, ErrOverloaded", allow, err)
	}
	close(release)
	if result := <-first; result.err != nil || result.allow {
		t.Fatalf("first admission result = %v, %v; want false, nil", result.allow, result.err)
	}
}

func TestConcurrentAdmissions(t *testing.T) {
	pool := NewPool()
	var containsCalls atomic.Int32
	for _, name := range []string{"alice", "bob", "carol", "dave"} {
		name := name
		pool.Upsert(name, newFake(name, func(ctx context.Context, _ key.NodePublic) (bool, error) {
			containsCalls.Add(1)
			timer := time.NewTimer(time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
				return false, nil
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}))
	}
	c := NewController(pool, Limits{
		RequestTimeout:          2 * time.Second,
		PerVerifierTimeout:      time.Second,
		MaxConcurrentAdmissions: 64,
		MaxConcurrentQueries:    4,
		MaxQueuedJobs:           256,
	})
	defer c.Close()

	const admissions = 64
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, admissions)
	for i := 0; i < admissions; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			allow, err := c.Admit(context.Background(), testNode(), keyAddr())
			if err != nil {
				errs <- err
			}
			if allow {
				errs <- errors.New("non-matching concurrent admission was allowed")
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent admission error: %v", err)
	}
	if containsCalls.Load() == 0 {
		t.Fatal("concurrent admissions did not reach a verifier")
	}
}

func TestCloseCancelsInFlightAdmission(t *testing.T) {
	entered := make(chan struct{})
	var enteredOnce sync.Once
	v := newFake("alice", func(ctx context.Context, _ key.NodePublic) (bool, error) {
		enteredOnce.Do(func() { close(entered) })
		<-ctx.Done()
		return false, ctx.Err()
	})
	pool := NewPool()
	pool.Upsert("alice", v)
	c := NewController(pool, Limits{
		RequestTimeout:       time.Minute,
		PerVerifierTimeout:   time.Minute,
		MaxConcurrentQueries: 1,
		MaxQueuedJobs:        1,
	})

	resultCh := make(chan resultValue, 1)
	go func() {
		allow, err := c.Admit(context.Background(), testNode(), keyAddr())
		resultCh <- resultValue{allow: allow, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("admission did not reach the in-flight verifier")
	}

	closed := make(chan struct{})
	go func() {
		c.Close()
		close(closed)
	}()
	select {
	case result := <-resultCh:
		if result.allow {
			t.Fatalf("closed controller allowed in-flight admission: %#v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight admission was not canceled by controller close")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("controller Close() did not finish after canceling admission")
	}
}

type resultValue struct {
	allow bool
	err   error
}

func TestAdmissionHandlerProtocol(t *testing.T) {
	c := NewController(NewPool(), DefaultLimits())
	defer c.Close()
	handler := c.Handler()

	requestBytes, err := json.Marshal(tailcfg.DERPAdmitClientRequest{NodePublic: testNode(), Source: keyAddr()})
	if err != nil {
		t.Fatalf("encode admission request: %v", err)
	}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admit", strings.NewReader(string(requestBytes)))
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("empty-pool response status = %d, want 200", recorder.Code)
	}
	var response tailcfg.DERPAdmitClientResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode admission response: %v", err)
	}
	if response.Allow {
		t.Fatal("empty pool unexpectedly allowed admission")
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admit", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET response status = %d, want 405", recorder.Code)
	}
}

func TestAdmissionHandlerQueryTimeoutReturnsDeny(t *testing.T) {
	pool := NewPool()
	pool.Upsert("slow", newFake("slow", func(ctx context.Context, _ key.NodePublic) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}))
	c := NewController(pool, Limits{
		RequestTimeout:          20 * time.Millisecond,
		PerVerifierTimeout:      time.Second,
		MaxConcurrentQueries:    1,
		MaxConcurrentAdmissions: 1,
		MaxQueuedJobs:           1,
	})
	defer c.Close()

	requestBytes, err := json.Marshal(tailcfg.DERPAdmitClientRequest{NodePublic: testNode(), Source: keyAddr()})
	if err != nil {
		t.Fatalf("encode admission request: %v", err)
	}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admit", strings.NewReader(string(requestBytes)))
	c.Handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("query-timeout response status = %d, want 200", recorder.Code)
	}
	var response tailcfg.DERPAdmitClientResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode query-timeout response: %v", err)
	}
	if response.Allow {
		t.Fatal("query timeout unexpectedly allowed admission")
	}
}

func keyAddr() netip.Addr {
	return netip.MustParseAddr("192.0.2.1")
}
