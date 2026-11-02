package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type loggerContextKey struct{}

func logFor(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerContextKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}
func apiHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "invalid"
	}
	return u.Host
}
func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
func durationEnv(key string, fallback time.Duration) time.Duration {
	v, err := time.ParseDuration(os.Getenv(key))
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}
func intEnv(key string, fallback int) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil || v < 1 {
		return fallback
	}
	return v
}
func validateRuntimeOptions() error {
	for _, key := range []string{"HTTP_TIMEOUT", "OPERATION_TIMEOUT", "RETRY_INITIAL_DELAY"} {
		if v := os.Getenv(key); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return fmt.Errorf("%s must be a positive duration", key)
			}
		}
	}
	if v := os.Getenv("RETRY_MAX_ATTEMPTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 10 {
			return fmt.Errorf("RETRY_MAX_ATTEMPTS must be between 1 and 10")
		}
	}
	return nil
}

type webhookMetrics struct {
	registry     *prometheus.Registry
	operations   *prometheus.CounterVec
	duration     *prometheus.HistogramVec
	httpRequests *prometheus.CounterVec
	retries      *prometheus.CounterVec
}

func newWebhookMetrics() *webhookMetrics {
	m := &webhookMetrics{
		registry:     prometheus.NewRegistry(),
		operations:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "vkcloud_webhook_operations_total", Help: "Completed DNS challenge operations."}, []string{"operation", "result"}),
		duration:     prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "vkcloud_webhook_operation_duration_seconds", Help: "DNS operation duration.", Buckets: prometheus.DefBuckets}, []string{"operation", "result"}),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "vkcloud_webhook_http_requests_total", Help: "VK API requests; status 0 denotes a transport failure."}, []string{"method", "status"}),
		retries:      prometheus.NewCounterVec(prometheus.CounterOpts{Name: "vkcloud_webhook_http_retries_total", Help: "Retries of safe VK API requests."}, []string{"method"}),
	}
	m.registry.MustRegister(m.operations, m.duration, m.httpRequests, m.retries, prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	return m
}

var metrics = newWebhookMetrics()

func startMetricsServer(address string) (func(), error) {
	if address == "" {
		return func() {}, nil
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(metrics.registry, promhttp.HandlerOpts{}))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("metrics_server_failed", "error", err)
		}
	}()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}, nil
}
