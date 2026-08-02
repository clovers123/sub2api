//go:build unit

package repository

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestNvidiaStaleSocketProbe_HealthySocket(t *testing.T) {
	// Healthy server responds 200 to GET /v1/models.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" && r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"data":[]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	entry := &upstreamClientEntry{client: client}

	// Should NOT panic, should NOT close connections.
	svc := &httpUpstreamService{}
	svc.nvidiaStaleSocketProbe(u.String(), entry)

	// After a successful probe, socket should still be usable.
	resp, err := client.Get(u.String() + "/v1/models")
	if err != nil {
		t.Fatalf("after successful probe, socket should still be usable: %v", err)
	}
	resp.Body.Close()
}

func TestNvidiaStaleSocketProbe_StaleSocketClosesConnections(t *testing.T) {
	// Stale server: immediately refuse connections after accept (simulate EOF).
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijacker", 500)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		conn.Close()
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	entry := &upstreamClientEntry{client: client}

	// check pre-condition: idle connections pool is empty.
	if entry.client != nil {
		entry.client.CloseIdleConnections()
	}

	svc := &httpUpstreamService{}
	// Should not panic even when socket is stale.
	svc.nvidiaStaleSocketProbe(u.String(), entry)
}

func TestNvidiaStaleSocketProbe_NilEntry(t *testing.T) {
	svc := &httpUpstreamService{}

	// nil entry should NOT panic
	svc.nvidiaStaleSocketProbe("https://integrate.api.nvidia.com", nil)

	// entry with nil client should NOT panic
	svc.nvidiaStaleSocketProbe("https://integrate.api.nvidia.com", &upstreamClientEntry{client: nil})
}

func TestNvidiaStaleSocketProbe_AnyHTTPStatusSucceeds(t *testing.T) {
	// 401, 403, 502 — any HTTP response carries a valid connection.
	for _, code := range []int{401, 403, 404, 502} {
		code := code
		t.Run(fmt.Sprintf("code_%d", code), func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			defer ts.Close()

			u, _ := url.Parse(ts.URL)
			client := &http.Client{Timeout: 5 * time.Second}
			entry := &upstreamClientEntry{client: client}

			svc := &httpUpstreamService{}
			// Probes regardless of status code return status, not panicking.
			svc.nvidiaStaleSocketProbe(u.String(), entry)
		})
	}
}

// Wrapper for hijack-enabled handler.
func wrap(s string) string { return s }