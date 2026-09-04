package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthEndpointsReturnStructuredStatus(t *testing.T) {
	want := Snapshot{
		Live:              true,
		Ready:             false,
		Startup:           true,
		DerperUsable:      false,
		EligibleVerifiers: 0,
		RequiredFailures:  1,
	}
	handler := NewServer(func() Snapshot { return want }).Handler()

	for path, statusCode := range map[string]int{
		"/health/live":    http.StatusOK,
		"/health/ready":   http.StatusServiceUnavailable,
		"/health/startup": http.StatusOK,
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != statusCode {
			t.Errorf("GET %s status = %d, want %d", path, recorder.Code, statusCode)
		}
		if contentType := recorder.Header().Get("Content-Type"); contentType != "application/json" {
			t.Errorf("GET %s Content-Type = %q, want application/json", path, contentType)
		}
		var got Snapshot
		if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
			t.Errorf("GET %s response JSON error = %v", path, err)
		} else if got != want {
			t.Errorf("GET %s response = %#v, want %#v", path, got, want)
		}
	}
}

func TestHealthEndpointsRejectOtherMethodsAndPaths(t *testing.T) {
	handler := NewServer(func() Snapshot { return Snapshot{Live: true} }).Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/health/live", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /health/live status = %d, want 405", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("GET /metrics status = %d, want 404", recorder.Code)
	}
}

func TestHealthLiveFailureIs503(t *testing.T) {
	handler := NewServer(func() Snapshot { return Snapshot{} }).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /health/live status = %d, want 503", recorder.Code)
	}
}
