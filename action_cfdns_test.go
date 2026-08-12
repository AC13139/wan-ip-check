package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestCFDNSExecuteFetchesMissingZoneName reproduces the production bug:
// when an action's config has zone_id but no zone_name, the wildcard
// record "*" canonicalizes to "" and fails with "empty name". The fix
// is to fetch the zone name from Cloudflare the first time Execute runs,
// same as TestConfig already does.
func TestCFDNSExecuteFetchesMissingZoneName(t *testing.T) {
	// Mock Cloudflare API server. Routes:
	//   GET /client/v4/zones/{zoneID}                        → returns zone name
	//   GET /client/v4/zones/{zoneID}/dns_records?type=A     → returns existing records
	//   PATCH /client/v4/zones/{zoneID}/dns_records/{recID} → update (existing)
	//   POST   /client/v4/zones/{zoneID}/dns_records         → create (new)
	var patchCount, postCount atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/client/v4/zones/zone123", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"result":{"id":"zone123","name":"example.com"}}`)
	})
	mux.HandleFunc("/client/v4/zones/zone123/dns_records", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			// Existing wildcard record at *.example.com
			fmt.Fprint(w, `{"success":true,"result":[{"id":"rec-wild","type":"A","name":"*.example.com","content":"1.1.1.1","proxied":true}],"result_info":{"page":1,"total_pages":1}}`)
		case http.MethodPost:
			postCount.Add(1)
			fmt.Fprint(w, `{"success":true,"result":{"id":"rec-new"}}`)
		}
	})
	mux.HandleFunc("/client/v4/zones/zone123/dns_records/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patchCount.Add(1)
			fmt.Fprint(w, `{"success":true,"result":{"id":"rec-wild"}}`)
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	exec := &CFDNSExecutor{client: srv.Client()}
	// point the base URL at our mock by overriding cfAPIBase would require a var;
	// instead, run a tiny proxy that rewrites the path. Simpler: rely on the
	// production cfAPIBase being a constant — so test against the real one is
	// what we'd want. But to keep the test hermetic, we use a custom client
	// whose Transport rewrites https://api.cloudflare.com → srv.URL.
	exec.client = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &rewriteTransport{base: srv.URL},
	}

	// Config WITHOUT zone_name (the production state).
	cfg := cfDNSConfig{
		APIToken: "fake",
		ZoneID:   "zone123",
		Records: []cfDNSRecord{
			{Name: "*", Proxied: true},
		},
	}
	raw, _ := json.Marshal(cfg)

	err := exec.Execute(context.Background(), IPEvent{
		WANInterfaceID: 1,
		EventType:      "ip_changed",
		OldIP:          "1.1.1.1",
		NewIP:          "2.2.2.2",
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
	}, raw)

	if err != nil {
		// Either the network path skipped and we get the production error,
		// OR the production path was used. We expect success after the fix.
		// Before the fix, the error is exactly: `updated 0/1 records, failed: [*=empty name]`
		if strings.Contains(err.Error(), "empty name") {
			t.Fatalf("BUG REPRODUCED — wildcard failed with empty name: %v", err)
		}
		t.Fatalf("unexpected error: %v", err)
	}
	if patchCount.Load() != 1 {
		t.Errorf("expected 1 PATCH to update the wildcard record, got %d", patchCount.Load())
	}
	if postCount.Load() != 0 {
		t.Errorf("expected 0 POSTs (record already exists), got %d", postCount.Load())
	}
}

// rewriteTransport rewrites requests for the production cfAPIBase host to the
// test server URL. Only used by tests.
type rewriteTransport struct{ base string }

func (t *rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// Clone and rewrite
	r2 := r.Clone(r.Context())
	r2.URL.Scheme = "http"
	r2.URL.Host = strings.TrimPrefix(t.base, "http://")
	return http.DefaultTransport.RoundTrip(r2)
}
