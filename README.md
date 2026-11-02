# VK Cloud DNS webhook for cert-manager

Version **0.3.0**. Maintained fork of
[drone076/cert-manager-webhook-vkcloud](https://github.com/drone076/cert-manager-webhook-vkcloud).

This DNS01 solver creates and removes ACME challenge TXT records through the
VK Cloud Public DNS API. cert-manager handles ACME orders, DNS propagation checks,
certificate issuance, and renewal. This webhook does not read or write Vault.

## Requirements

- Go 1.26 for building or testing the source.
- cert-manager installed in Kubernetes before installing this chart. Dependencies
  are aligned with cert-manager v1.21.3 and Kubernetes Go modules v0.36.2.
- A public DNS zone delegated to VK Cloud, and credentials with access to read
  zones and read/create/delete TXT records in its project.
- A container registry accessible to Kubernetes nodes.

## Build the image

Set your actual Docker registry endpoint. This repository does not assume that a
public image is published. The Helm default image repository is a placeholder.

```bash
export WEBHOOK_IMAGE=registry.example.com/infra/cert-manager-webhook-vkcloud:0.3.0
docker login registry.example.com
docker buildx build --platform linux/amd64 --tag "$WEBHOOK_IMAGE" --push .
```

Use linux/arm64 or a multi-platform build for ARM nodes. The final image is
non-root, contains a statically linked binary, and loads credentials at runtime.
The embedded image label is cert-manager-webhook-vkcloud:0.3.0.

## Install the chart

Replace the placeholder registry with the repository used above:

```bash
helm upgrade --install vkcloud-webhook deploy/cert-manager-webhook-vkcloud \
  --namespace cert-manager --create-namespace \
  --set-string image.repository=registry.example.com/infra/cert-manager-webhook-vkcloud \
  --set-string image.tag=0.3.0 \
  --wait --timeout 10m
```

The chart installs Deployment, Service, ServiceAccount/RBAC, APIService, and
Issuer/Certificate resources for serving TLS. The process listens on HTTPS 8443,
behind a Service on 443. Names are stable across release names by default;
change fullnameOverride if intentionally deploying another instance.
For a private registry, set imagePullSecrets to a list of Kubernetes Secret names.

## Configure credentials and ACME

The files under data/ are examples. Replace all placeholders and example.com with
a domain you control. Do not commit real credentials.

1. Provision the credential Secret using your existing secret management workflow;
   data/vkcloud-secret.yaml describes the expected fields.
2. Apply data/clusterissuer.yaml after replacing the email and DNS zone.
3. Apply data/certificate.yaml to request both the ordinary name and wildcard.

The Secret is named vkcloud-dns-credentials by default. It must match
credentialsSecretName in chart values and secretRef.name in the Issuer.
Credentials are read from the Issuer resource namespace. A reference to another
namespace is rejected. For ClusterIssuer, cert-manager normally uses namespace
cert-manager; adjust its cluster resource namespace and the webhook configuration
consistently if using a different setup. RBAC grants get only on that one Secret.

| Secret field | Required | Description |
| --- | --- | --- |
| os_username | Yes | VK Cloud username, typically a dedicated service account |
| os_password | Yes | Account password |
| os_project_id | Yes | Keystone project ID that owns the public DNS zone |
| os_auth_url | No | Identity v3 URL; defaults to https://msk.cloud.vk.ru/infra/identity/v3/ |
| os_domain_name | No | User domain name; default users, typically service-users for service accounts |
| os_domain_id | No | User domain ID; takes precedence over os_domain_name |

Auth URLs may end with /v3/ or /v3/auth/tokens. The client adds /auth/tokens when
needed. The DNS project can differ from the Kubernetes cluster project.

Issuer solver configuration:

```yaml
dns01:
  webhook:
    groupName: acme.example.com
    solverName: cert-manager-webhook-vkcloud
    config:
      secretRef:
        name: vkcloud-dns-credentials
      region: RegionOne
      # zoneID: YOUR_ZONE_UUID
      # dnsAPIURL: https://actual-endpoint/public-dns/v2
```

The chart groupName, GROUP_NAME environment variable, APIService, and Issuer
webhook.groupName must agree. Choose a group name under a domain you control.

The client obtains the publicdns endpoint for the configured region from the
Keystone service catalog. An explicit dnsAPIURL overrides discovery and must
include the v2 base path without /dns/. A configured zoneID bypasses zone listing;
otherwise the client matches the zone by its resolved name.

Start with Let's Encrypt staging, as shown in the examples. After validating
issuance, TXT cleanup, and renewal, configure the production ACME URL and a
separate account Secret. Staging certificates are not browser-trusted.

## DNS record lifecycle

- Present normalizes the FQDN into a name relative to its DNS zone and checks
  existing records by both name and TXT value before creating.
- CleanUp removes only matching records. Other values at the same name are kept;
  an already missing record or HTTP 404 during deletion is successful cleanup.
- The client accepts TXT list responses as either an array or a txt_records object.
- TXT records are created with TTL 60 seconds. cert-manager checks propagation.
- GET and DELETE retry transport failures and HTTP 429/500/502/503/504. Retries
  use exponential backoff with jitter and honour Retry-After up to 30 seconds.
- POST is not retried blindly. If creation succeeds but the response is lost,
  the next cert-manager reconciliation reads existing records before creating.

## Logging and runtime settings

Application logs use JSON. The chart also enables JSON logging for the embedded
cert-manager API server. Lifecycle events include client creation, authentication,
endpoint/zone selection, record creation/deletion, completion, retries, and errors.
Passwords, authentication tokens, TXT values, and response bodies are not logged.

| Helm value | Environment variable | Default |
| --- | --- | --- |
| logging.level | LOG_LEVEL | info |
| runtime.httpTimeout | HTTP_TIMEOUT | 30s |
| runtime.operationTimeout | OPERATION_TIMEOUT | 2m |
| runtime.retryMaxAttempts | RETRY_MAX_ATTEMPTS | 3 |
| runtime.retryInitialDelay | RETRY_INITIAL_DELAY | 1s |
| metrics.enabled / metrics.port | METRICS_ADDRESS | :9402 |

Use debug for HTTP method/status/attempt/duration logs. Valid levels are debug,
info, warn, and error. retryMaxAttempts includes the initial attempt; 1 disables
retries, and the accepted range is 1 through 10. Durations must be positive.
An empty METRICS_ADDRESS disables the metrics listener.

## Prometheus and VictoriaMetrics

Metrics are enabled at HTTP :9402/metrics on a separate ClusterIP Service.
ServiceMonitor creation is **disabled by default** because its CRD is optional.
Enable metrics.serviceMonitor.enabled only when the Prometheus Operator CRD exists,
and set metrics.serviceMonitor.labels to match your Prometheus selector.

Native VictoriaMetrics VMPodScrape can select the webhook pods using the app label
cert-manager-webhook-vkcloud and the named port metrics. Avoid scraping the same
endpoint through both native discovery and a converted ServiceMonitor.

| Metric | Labels | Description |
| --- | --- | --- |
| vkcloud_webhook_operations_total | operation, result | Completed present/cleanup operations |
| vkcloud_webhook_operation_duration_seconds | operation, result | Operation duration histogram |
| vkcloud_webhook_http_requests_total | method, status | API attempts; status 0 means a transport failure |
| vkcloud_webhook_http_retries_total | method | Retries of safe API requests |

Go runtime and process metrics are also exposed. Labels do not contain domains,
TXT values, account names, or credentials.

## Verify and troubleshoot

```bash
go test -race ./...
go vet ./...
helm lint deploy/cert-manager-webhook-vkcloud
kubectl -n cert-manager logs deployment/cert-manager-webhook-vkcloud --since=10m
kubectl -n cert-manager get certificate,certificaterequest,order,challenge
kubectl get apiservice v1alpha1.acme.example.com
```

An APIService should report Available=True. A successful Present only means that
the provider accepted the record; DNS propagation and ACME authorization follow.
Check TXT publication on the authoritative nameservers if a Challenge remains
pending. Check user domain, DNS project ID, region, and permissions for HTTP 401/403.
A missing publicdns catalog endpoint can be handled with an explicit dnsAPIURL.

Tests cover authentication/domain mapping, TXT normalization, repeat-safe creation
and cleanup, retries, cancellation, lost create responses, JSON log redaction,
metrics exposition, and invalid runtime options. Live VK DNS and ACME staging
verification is still required for a new cloud/project configuration.

## License

GPL-3.0, inherited from upstream. See LICENSE.
