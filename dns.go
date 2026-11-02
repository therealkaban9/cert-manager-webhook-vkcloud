package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const operationTimeout = 2 * time.Minute

type dnsClient struct {
	http          *http.Client
	base, token   string
	retryAttempts int
	retryDelay    time.Duration
}
type dnsRecord struct {
	UUID    string `json:"uuid"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

func normalized(s string) string { return strings.ToLower(strings.TrimSuffix(s, ".")) }
func relativeName(fqdn, zone string) (string, error) {
	name, suffix := normalized(fqdn), normalized(zone)
	if name == suffix {
		return "", nil
	}
	if !strings.HasSuffix(name, "."+suffix) {
		return "", fmt.Errorf("record %q is outside zone %q", fqdn, zone)
	}
	return strings.TrimSuffix(name, "."+suffix), nil
}
func httpsURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("API URL must be an absolute HTTPS URL without credentials, query or fragment")
	}
	return strings.TrimRight(raw, "/"), nil
}
func newDNSClient(ctx context.Context, data map[string][]byte, cfg vkcloudDNSConfig) (*dnsClient, error) {
	return newDNSClientWithHTTP(ctx, data, cfg, &http.Client{Timeout: durationEnv("HTTP_TIMEOUT", 30*time.Second), CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }})
}
func newDNSClientWithHTTP(ctx context.Context, data map[string][]byte, cfg vkcloudDNSConfig, httpClient *http.Client) (*dnsClient, error) {
	for _, key := range []string{"os_username", "os_password", "os_project_id"} {
		if len(data[key]) == 0 {
			return nil, fmt.Errorf("credential key %s is required", key)
		}
	}
	authURL := string(data["os_auth_url"])
	if authURL == "" {
		authURL = "https://msk.cloud.vk.ru/infra/identity/v3/"
	}
	authURL, err := httpsURL(authURL)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(authURL, "/auth/tokens") {
		authURL += "/auth/tokens"
	}
	domain := map[string]string{"name": "users"}
	if name := string(data["os_domain_name"]); name != "" {
		domain = map[string]string{"name": name}
	}
	if id := string(data["os_domain_id"]); id != "" {
		domain = map[string]string{"id": id}
	}
	body := map[string]any{"auth": map[string]any{
		"identity": map[string]any{"methods": []string{"password"}, "password": map[string]any{"user": map[string]any{
			"name": string(data["os_username"]), "password": string(data["os_password"]), "domain": domain,
		}}}, "scope": map[string]any{"project": map[string]string{"id": string(data["os_project_id"])}},
	}}
	c := &dnsClient{http: httpClient, retryAttempts: intEnv("RETRY_MAX_ATTEMPTS", 3), retryDelay: durationEnv("RETRY_INITIAL_DELAY", time.Second)}
	logFor(ctx).Info("client_created", "region", cfg.Region, "http_timeout", httpClient.Timeout.String())
	var result struct {
		Token struct {
			Catalog []struct {
				Type      string `json:"type"`
				Endpoints []struct {
					Interface, Region, URL string
					RegionID               string `json:"region_id"`
				} `json:"endpoints"`
			} `json:"catalog"`
		} `json:"token"`
	}
	logFor(ctx).Info("authentication_started", "host", apiHost(authURL))
	headers, err := c.request(ctx, "POST", authURL, body, &result, 201)
	if err != nil {
		return nil, fmt.Errorf("Keystone authentication: %w", err)
	}
	c.token = headers.Get("X-Subject-Token")
	if c.token == "" {
		return nil, fmt.Errorf("Keystone returned no X-Subject-Token")
	}
	logFor(ctx).Info("authentication_completed")
	endpoint := cfg.DNSAPIURL
	if endpoint == "" {
		for _, service := range result.Token.Catalog {
			if service.Type != "publicdns" {
				continue
			}
			for _, ep := range service.Endpoints {
				if ep.Interface == "public" && (cfg.Region == "" || ep.Region == cfg.Region || ep.RegionID == cfg.Region) {
					endpoint = strings.TrimRight(ep.URL, "/") + "/v2"
					break
				}
			}
		}
	}
	if endpoint == "" {
		return nil, fmt.Errorf("publicdns endpoint not found for region %q; configure dnsAPIURL explicitly", cfg.Region)
	}
	c.base, err = httpsURL(endpoint)
	if err == nil {
		logFor(ctx).Info("dns_endpoint_selected", "host", apiHost(c.base), "region", cfg.Region, "override", cfg.DNSAPIURL != "")
	}
	return c, err
}

// Never put response bodies in errors: identity responses can contain credentials/tokens.
func (c *dnsClient) request(ctx context.Context, method, endpoint string, body, out any, allowed ...int) (http.Header, error) {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	attempts := c.retryAttempts
	if attempts < 1 {
		attempts = 1
	}
	// A POST may have reached the server even if the response was lost. Never retry blindly.
	if method != http.MethodGet && method != http.MethodDelete {
		attempts = 1
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.token != "" {
			req.Header.Set("X-Auth-Token", c.token)
		}
		started := time.Now()
		resp, requestErr := c.http.Do(req)
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		metrics.httpRequests.WithLabelValues(method, strconv.Itoa(status)).Inc()
		logFor(ctx).Debug("http_request_completed", "method", method, "host", apiHost(endpoint), "status", status, "attempt", attempt, "duration_ms", time.Since(started).Milliseconds())
		// Transport error text can contain URL/userinfo: do not propagate or log it.
		if requestErr != nil {
			err = fmt.Errorf("%s API transport failure", method)
		} else {
			ok := false
			for _, code := range allowed {
				if status == code {
					ok = true
					break
				}
			}
			if ok {
				if out != nil && status != 204 {
					err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
				}
				resp.Body.Close()
				if err != nil {
					logFor(ctx).Error("http_decode_failed", "method", method, "status", status)
					return nil, fmt.Errorf("decode %s API response failed", method)
				}
				return resp.Header, nil
			}
			err = fmt.Errorf("%s API returned HTTP %d (request ID %s)", method, status, resp.Header.Get("X-Openstack-Request-Id"))
		}
		retryable := requestErr != nil || status == 429 || status == 500 || status == 502 || status == 503 || status == 504
		if !retryable || attempt == attempts || ctx.Err() != nil {
			if resp != nil {
				resp.Body.Close()
			}
			logFor(ctx).Error("http_request_failed", "method", method, "host", apiHost(endpoint), "status", status, "attempt", attempt, "retryable", retryable, "error", err)
			return nil, err
		}
		delay := c.retryDelay * time.Duration(1<<min(attempt-1, 5))
		if delay > 0 {
			delay += time.Duration(rand.Int64N(int64(delay/4) + 1))
		}
		if resp != nil {
			if seconds, parseErr := strconv.Atoi(resp.Header.Get("Retry-After")); parseErr == nil && seconds >= 0 {
				delay = time.Duration(min(seconds, 30)) * time.Second
			} else if when, parseErr := http.ParseTime(resp.Header.Get("Retry-After")); parseErr == nil {
				delay = min(max(time.Until(when), 0), 30*time.Second)
			}
			resp.Body.Close()
		}
		metrics.retries.WithLabelValues(method).Inc()
		logFor(ctx).Warn("http_request_retry", "method", method, "status", status, "attempt", attempt, "delay_ms", delay.Milliseconds())
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, fmt.Errorf("API request exhausted attempts")
}
func (c *dnsClient) zone(ctx context.Context, name, id string) (string, error) {
	if id != "" {
		logFor(ctx).Info("dns_zone_selected", "zone_id", id, "zone", name, "source", "config")
		return id, nil
	}
	logFor(ctx).Info("dns_zones_list_started", "zone", name)
	var zones []struct {
		UUID string `json:"uuid"`
		Zone string `json:"zone"`
	}
	_, err := c.request(ctx, "GET", c.base+"/dns/", nil, &zones, 200)
	if err != nil {
		return "", err
	}
	for _, zone := range zones {
		if normalized(zone.Zone) == normalized(name) {
			logFor(ctx).Info("dns_zone_selected", "zone_id", zone.UUID, "zone", name, "source", "discovery")
			return zone.UUID, nil
		}
	}
	return "", fmt.Errorf("DNS zone %q not found", name)
}
func (c *dnsClient) reconcileTXT(ctx context.Context, zone, name, key string, cleanup bool) error {
	logFor(ctx).Info("dns_records_list_started", "zone_id", zone, "record_name", name, "cleanup", cleanup)
	endpoint := c.base + "/dns/" + url.PathEscape(zone) + "/txt/"
	var raw json.RawMessage
	_, err := c.request(ctx, "GET", endpoint, nil, &raw, 200)
	if err != nil {
		return err
	}
	var records []dnsRecord
	if err := json.Unmarshal(raw, &records); err != nil {
		var wrapped struct {
			Records []dnsRecord `json:"txt_records"`
		}
		if err := json.Unmarshal(raw, &wrapped); err != nil || wrapped.Records == nil {
			return fmt.Errorf("unrecognized TXT list response")
		}
		records = wrapped.Records
	}
	found := false
	for _, record := range records {
		if normalized(record.Name) != normalized(name) || record.Content != key {
			continue
		}
		found = true
		if cleanup {
			if record.UUID == "" {
				return fmt.Errorf("TXT record has no UUID")
			}
			logFor(ctx).Info("dns_record_delete_started", "zone_id", zone, "record_name", name, "record_id", record.UUID)
			if _, err := c.request(ctx, "DELETE", endpoint+url.PathEscape(record.UUID), nil, nil, 200, 202, 204, 404); err != nil {
				return err
			}
			logFor(ctx).Info("dns_record_deleted", "zone_id", zone, "record_name", name, "record_id", record.UUID)
		}
	}
	if cleanup || found {
		logFor(ctx).Info("dns_records_reconciled", "record_name", name, "cleanup", cleanup, "found", found)
		return nil
	}
	logFor(ctx).Info("dns_record_create_started", "zone_id", zone, "record_name", name)
	_, err = c.request(ctx, "POST", endpoint, map[string]any{"name": name, "content": key, "ttl": 60}, nil, 200, 201, 202)
	if err == nil {
		logFor(ctx).Info("dns_record_created", "zone_id", zone, "record_name", name)
	}
	return err
}
