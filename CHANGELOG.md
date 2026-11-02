# Changelog

## 0.3.0

- Align dependencies with Go 1.26, cert-manager v1.21.3, and Kubernetes modules v0.36.2.
- Authenticate with Keystone user domain names or IDs and discover regional DNS endpoints.
- Normalize relative DNS names and preserve unrelated TXT records during cleanup.
- Add repeat-safe Present/CleanUp, bounded safe-method retries, and request timeouts.
- Add JSON lifecycle logs without credentials, TXT values, or API response bodies.
- Expose Prometheus metrics and configurable optional ServiceMonitor resources.
- Restrict Kubernetes credential access to a single Secret in the issuer namespace.
- Run as non-root on port 8443 behind a Service on port 443.
- Add tests for retries, errors, cancellation, lost responses, redaction, and metrics.
- Update the standalone Helm v2 chart and English installation examples.
