package phpfpm

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/fcgi"
	"sync/atomic"
	"testing"
	"time"
)

func statusServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() { _ = fcgi.Serve(listener, handler); close(finished) }()
	t.Cleanup(func() { _ = listener.Close(); <-finished })
	return "tcp://" + listener.Addr().String()
}

func TestReadStatusDoesNotRunPHPEnrichmentOrRewriteWorkerCounts(t *testing.T) {
	var requests atomic.Int32
	address := statusServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/status" || r.URL.RawQuery != "json&full" {
			t.Errorf("unexpected probe: %s", r.URL)
			http.Error(w, "unexpected probe", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"pool":"www","active processes":2,"idle processes":0,"total processes":2,"listen queue":1,"processes":[{"pid":10,"state":"Running","request uri":"/status?json&full"},{"pid":11,"state":"Finishing","script":"/app/after-response.php"}]}`)
	}))
	pool, err := ReadStatus(context.Background(), Target{Name: "www", Socket: address, StatusPath: "/status", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("expected one native status request, got %d", requests.Load())
	}
	if pool.ActiveProcesses != 2 || pool.TotalProcesses != 2 || pool.IdleProcesses != 0 || pool.ListenQueue != 1 {
		t.Fatalf("native counters were changed: %+v", pool)
	}
	if len(pool.Processes) != 2 || pool.Processes[1].State != "Finishing" || pool.Processes[1].Script != "/app/after-response.php" {
		t.Fatalf("native process state was lost: %+v", pool.Processes)
	}
	if pool.OpcacheObserved || pool.Config != nil || pool.ProcessesCpu != nil {
		t.Fatal("status-only result contains enrichment")
	}
}

func TestReadStatusRejectsInvalidAndForeignResponses(t *testing.T) {
	for _, body := range []string{`{}`, `{"pool":"foreign","active processes":0,"idle processes":0,"total processes":0,"listen queue":0,"processes":[]}`, `not json`} {
		t.Run(body, func(t *testing.T) {
			address := statusServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
			if _, err := ReadStatus(context.Background(), Target{Name: "www", Socket: address, StatusPath: "/status"}); err == nil {
				t.Fatal("invalid status accepted")
			}
		})
	}
}

func TestReadStatusHonorsDeadline(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	address := statusServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { <-release }))
	started := time.Now()
	if _, err := ReadStatus(context.Background(), Target{Name: "www", Socket: address, StatusPath: "/status", Timeout: 50 * time.Millisecond}); err == nil {
		t.Fatal("stalled status succeeded")
	}
	if time.Since(started) > time.Second {
		t.Fatal("status exchange exceeded its deadline")
	}
}

func TestReadStatusRequiresObservedQueueAndWorkerFields(t *testing.T) {
	for _, field := range []string{"active processes", "idle processes", "total processes", "listen queue", "processes"} {
		for _, null := range []bool{false, true} {
			name := field + " missing"
			if null {
				name = field + " null"
			}
			t.Run(name, func(t *testing.T) {
				document := map[string]any{"pool": "www", "active processes": 1, "idle processes": 0, "total processes": 1, "listen queue": 0, "processes": []map[string]any{{"pid": 10, "state": "Running", "request method": "GET", "request uri": "/status?json&full"}}}
				if null {
					document[field] = nil
				} else {
					delete(document, field)
				}
				address := statusServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if err := json.NewEncoder(w).Encode(document); err != nil {
						t.Error(err)
					}
				}))
				if _, err := ReadStatus(context.Background(), Target{Name: "www", Socket: address, StatusPath: "/status"}); err == nil {
					t.Fatal("missing observation was accepted as a measured value")
				}
			})
		}
	}
	// A real zero queue and a fully idle native snapshot are valid observations;
	// the checkpoint consumer separately decides whether this status model fits.
	address := statusServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"pool":"www","active processes":0,"idle processes":0,"total processes":0,"listen queue":0,"processes":[]}`)
	}))
	pool, err := ReadStatus(context.Background(), Target{Name: "www", Socket: address, StatusPath: "/status"})
	if err != nil || pool.ListenQueue != 0 || pool.Processes == nil {
		t.Fatalf("explicit empty observation rejected: %+v, %v", pool, err)
	}
}
