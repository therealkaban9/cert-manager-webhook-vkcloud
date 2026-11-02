package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	acme "github.com/cert-manager/cert-manager/pkg/acme/webhook/apis/acme/v1alpha1"
	"github.com/cert-manager/cert-manager/pkg/acme/webhook/cmd"
	corev1 "k8s.io/api/core/v1"
	extapi "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	level := new(slog.LevelVar)
	if err := level.UnmarshalText([]byte(envOr("LOG_LEVEL", "info"))); err != nil {
		panic(err)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
	if err := validateRuntimeOptions(); err != nil {
		slog.Error("invalid_runtime_options", "error", err)
		os.Exit(1)
	}
	stop, err := startMetricsServer(envOr("METRICS_ADDRESS", ":9402"))
	if err != nil {
		slog.Error("metrics_start_failed", "error", err)
		os.Exit(1)
	}
	defer stop()
	slog.Info("webhook_starting", "version", "0.3.0", "metrics_address", envOr("METRICS_ADDRESS", ":9402"))
	group := os.Getenv("GROUP_NAME")
	if group == "" {
		panic("GROUP_NAME must be specified")
	}
	cmd.RunWebhookServer(group, &vkcloudDNSSolver{})
}

type vkcloudDNSSolver struct{ client kubernetes.Interface }
type vkcloudDNSConfig struct {
	SecretRef corev1.SecretReference `json:"secretRef"`
	Region    string                 `json:"region,omitempty"`
	DNSAPIURL string                 `json:"dnsAPIURL,omitempty"`
	ZoneID    string                 `json:"zoneID,omitempty"`
}

func (s *vkcloudDNSSolver) Name() string { return "cert-manager-webhook-vkcloud" }
func (s *vkcloudDNSSolver) Initialize(cfg *rest.Config, stopCh <-chan struct{}) error {
	client, err := kubernetes.NewForConfig(cfg)
	s.client = client
	return err
}
func loadConfig(raw *extapi.JSON) (vkcloudDNSConfig, error) {
	cfg := vkcloudDNSConfig{Region: "RegionOne"}
	if raw == nil {
		return cfg, fmt.Errorf("solver config is required")
	}
	if err := json.Unmarshal(raw.Raw, &cfg); err != nil {
		return cfg, err
	}
	if cfg.SecretRef.Name == "" {
		return cfg, fmt.Errorf("secretRef.name is required")
	}
	return cfg, nil
}
func (s *vkcloudDNSSolver) solve(ch *acme.ChallengeRequest, cleanup bool) (result error) {
	operation := "present"
	if cleanup {
		operation = "cleanup"
	}
	logger := slog.Default().With("operation", operation, "domain", ch.DNSName, "namespace", ch.ResourceNamespace)
	started := time.Now()
	logger.Info("challenge_started")
	defer func() {
		outcome := "success"
		if result != nil {
			outcome = "error"
			logger.Error("challenge_failed", "error", result)
		} else {
			logger.Info("challenge_completed")
		}
		metrics.operations.WithLabelValues(operation, outcome).Inc()
		metrics.duration.WithLabelValues(operation, outcome).Observe(time.Since(started).Seconds())
	}()

	cfg, err := loadConfig(ch.Config)
	if err != nil {
		return err
	}
	// Credentials must belong to the issuer's resource namespace.
	if cfg.SecretRef.Namespace != "" && cfg.SecretRef.Namespace != ch.ResourceNamespace {
		return fmt.Errorf("cross-namespace credential reference is forbidden")
	}
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), loggerContextKey{}, logger), durationEnv("OPERATION_TIMEOUT", operationTimeout))
	defer cancel()
	logger.Debug("credentials_read", "secret_name", cfg.SecretRef.Name)
	secret, err := s.client.CoreV1().Secrets(ch.ResourceNamespace).Get(ctx, cfg.SecretRef.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read credentials: %w", err)
	}
	client, err := newDNSClient(ctx, secret.Data, cfg)
	if err != nil {
		return err
	}
	zone, err := client.zone(ctx, ch.ResolvedZone, cfg.ZoneID)
	if err != nil {
		return err
	}
	name, err := relativeName(ch.ResolvedFQDN, ch.ResolvedZone)
	if err != nil {
		return err
	}
	return client.reconcileTXT(ctx, zone, name, ch.Key, cleanup)
}
func (s *vkcloudDNSSolver) Present(ch *acme.ChallengeRequest) error { return s.solve(ch, false) }
func (s *vkcloudDNSSolver) CleanUp(ch *acme.ChallengeRequest) error { return s.solve(ch, true) }
