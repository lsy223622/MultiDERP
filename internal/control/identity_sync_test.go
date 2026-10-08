package control

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdentitySyncBoundedAndStops(t *testing.T) {
	s, _, admin := credentialTestStore(t)
	ids := []string{}
	for i := 0; i < 6; i++ {
		tn, err := s.AddTailnet(t.Context(), admin, fmt.Sprint(i), fmt.Sprintf("Tsync%d", i), OAuthCredential{"id", "secret"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, tn.ID)
	}
	if _, err := s.db.Exec("UPDATE identity_snapshots SET last_success=1"); err != nil {
		t.Fatal(err)
	}
	var active, maxActive atomic.Int32
	entered := make(chan struct{}, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		n := active.Add(1)
		defer active.Add(-1)
		for old := maxActive.Load(); n > old && !maxActive.CompareAndSwap(old, n); old = maxActive.Load() {
		}
		entered <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	s.apiBase = server.URL
	s.apiClient = server.Client()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.RunIdentitySync(ctx) }()
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("workers did not start")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("sync did not stop")
	}
	if maxActive.Load() > 4 {
		t.Fatal("unbounded requests")
	}
	for _, id := range ids {
		snap, err := s.IdentitySnapshot(t.Context(), id)
		if err != nil || snap.LastSuccess.Unix() != 1 {
			t.Fatal("failed poll renewed identity")
		}
	}
}

func TestIdentityRetryDelayIsBounded(t *testing.T) {
	if identityRetryDelay(0) != time.Minute || identityRetryDelay(1) != 2*time.Minute || identityRetryDelay(100) != time.Hour {
		t.Fatal("unbounded backoff")
	}
}
