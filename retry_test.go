package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func TestSafeRequestRetriesAndRetryAfter(t *testing.T) {
	for _, method := range []string{"GET", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			attempts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts++
				w.Header().Set("Retry-After", "0")
				if attempts == 1 {
					w.WriteHeader(429)
					return
				}
				if attempts == 2 {
					w.WriteHeader(503)
					return
				}
				w.WriteHeader(204)
			}))
			defer server.Close()
			c := &dnsClient{http: server.Client(), retryAttempts: 3, retryDelay: time.Millisecond}
			if _, err := c.request(context.Background(), method, server.URL, nil, nil, 204); err != nil {
				t.Fatal(err)
			}
			if attempts != 3 {
				t.Fatalf("attempts=%d", attempts)
			}
		})
	}
}
func TestNoRetryOnForbiddenOrAmbiguousCreate(t *testing.T) {
	for _, tc := range []struct {
		method string
		status int
	}{{"GET", 403}, {"POST", 503}, {"DELETE", 404}} {
		t.Run(fmt.Sprintf("%s-%d", tc.method, tc.status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(tc.status) }))
			defer server.Close()
			c := &dnsClient{http: server.Client(), retryAttempts: 3, retryDelay: time.Millisecond}
			allowed := []int{204}
			if tc.status == 404 {
				allowed = append(allowed, 404)
			}
			_, err := c.request(context.Background(), tc.method, server.URL, nil, nil, allowed...)
			if calls != 1 {
				t.Fatalf("unexpected retry: %d calls", calls)
			}
			if tc.status == 404 && err != nil {
				t.Fatal(err)
			}
			if tc.status != 404 && err == nil {
				t.Fatal("expected failure")
			}
		})
	}
}
func TestRetriesExhausted(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(502)
	}))
	defer server.Close()
	c := &dnsClient{http: server.Client(), retryAttempts: 3}
	_, err := c.request(context.Background(), "GET", server.URL, nil, nil, 200)
	if err == nil || calls != 3 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
func TestRetryBackoffRespectsContext(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(503) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	c := &dnsClient{http: server.Client(), retryAttempts: 3, retryDelay: time.Second}
	_, err := c.request(ctx, "GET", server.URL, nil, nil, 200)
	if err == nil || ctx.Err() == nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
func TestLifecycleJSONLogsNeverContainTXTValueOrAuthToken(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := context.WithValue(context.Background(), loggerContextKey{}, logger)
	created := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			if created {
				fmt.Fprint(w, `[{"uuid":"ours","name":"_acme-challenge.example","content":"SECRET_CHALLENGE_VALUE"}]`)
			} else {
				fmt.Fprint(w, `[]`)
			}
			return
		}
		if r.Method == "DELETE" {
			created = false
			w.WriteHeader(204)
			return
		}
		created = true
		w.WriteHeader(201)
	}))
	defer server.Close()
	c := &dnsClient{http: server.Client(), base: server.URL, token: "SECRET_AUTH_TOKEN"}
	if err := c.reconcileTXT(ctx, "zone", "_acme-challenge.example", "SECRET_CHALLENGE_VALUE", false); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileTXT(ctx, "zone", "_acme-challenge.example", "SECRET_CHALLENGE_VALUE", true); err != nil {
		t.Fatal(err)
	}
	logs := output.String()
	for _, secret := range []string{"SECRET_AUTH_TOKEN", "SECRET_CHALLENGE_VALUE"} {
		if strings.Contains(logs, secret) {
			t.Fatal("secret leaked to logs")
		}
	}
	messages := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("non-JSON log: %s", line)
		}
		messages[event["msg"].(string)] = true
	}
	if !messages["dns_record_created"] || !messages["dns_record_deleted"] || !messages["http_request_completed"] {
		t.Fatalf("missing lifecycle logs: %v", messages)
	}
}
func TestMetricsCanBeScraped(t *testing.T) {
	m := newWebhookMetrics()
	m.operations.WithLabelValues("present", "success").Inc()
	m.retries.WithLabelValues("GET").Inc()
	recorder := httptest.NewRecorder()
	promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}).ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	for _, name := range []string{"vkcloud_webhook_operations_total", "vkcloud_webhook_http_retries_total", "go_goroutines"} {
		if !strings.Contains(recorder.Body.String(), name) {
			t.Fatalf("missing metric %s", name)
		}
	}
}
func TestInvalidRuntimeOptions(t *testing.T) {
	t.Setenv("RETRY_MAX_ATTEMPTS", "0")
	if validateRuntimeOptions() == nil {
		t.Fatal("invalid retry count accepted")
	}
	t.Setenv("RETRY_MAX_ATTEMPTS", "3")
	t.Setenv("HTTP_TIMEOUT", "invalid")
	if validateRuntimeOptions() == nil {
		t.Fatal("invalid timeout accepted")
	}
}

// The API can create a record and lose the response. A later Present must discover
// that record rather than blindly send a duplicate POST.
func TestLostCreateResponseDoesNotDuplicateTXT(t *testing.T) {
	created := false
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			if created {
				fmt.Fprint(w, `[{"uuid":"ours","name":"_acme-challenge.example","content":"challenge"}]`)
			} else {
				fmt.Fprint(w, `[]`)
			}
			return
		}
		posts++
		created = true
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer server.Close()
	c := &dnsClient{http: server.Client(), base: server.URL, retryAttempts: 3}
	if err := c.reconcileTXT(context.Background(), "zone", "_acme-challenge.example", "challenge", false); err == nil {
		t.Fatal("expected lost-response failure")
	}
	if err := c.reconcileTXT(context.Background(), "zone", "_acme-challenge.example", "challenge", false); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatalf("duplicate POSTs: %d", posts)
	}
}
