package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRelativeName(t *testing.T) {
	got, err := relativeName("_acme-challenge.K8S.example.org.", "example.org.")
	if err != nil || got != "_acme-challenge.k8s" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := relativeName("_acme-challenge.badexample.org", "example.org"); err == nil {
		t.Fatal("accepted record outside zone")
	}
}
func TestAuthenticationUsesDomainNameAndCatalog(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/auth/tokens" {
			t.Errorf("auth path: %s", r.URL.Path)
		}
		var body struct {
			Auth struct {
				Identity struct {
					Password struct {
						User struct{ Domain map[string]string }
					}
				}
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Auth.Identity.Password.User.Domain["name"] != "users" {
			t.Error("users must be a domain name, not ID")
		}
		w.Header().Set("X-Subject-Token", "token")
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"token":{"catalog":[{"type":"publicdns","endpoints":[{"interface":"public","region":"RegionOne","url":%q}]}]}}`, srv.URL+"/public-dns/")
	}))
	defer srv.Close()
	cfg := vkcloudDNSConfig{Region: "RegionOne"}
	creds := map[string][]byte{"os_auth_url": []byte(srv.URL + "/v3/"), "os_username": []byte("u"), "os_password": []byte("p"), "os_project_id": []byte("project")}
	c, err := newDNSClientWithHTTP(context.Background(), creds, cfg, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if c.base != srv.URL+"/public-dns/v2" || c.token != "token" {
		t.Fatalf("incorrect catalog endpoint: %s", c.base)
	}
}
func TestTXTRepeatAndCleanupPreserveOtherRecords(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(fmt.Sprint(wrapped), func(t *testing.T) {
			records := []dnsRecord{{UUID: "other", Name: "_acme-challenge.k8s", Content: "other-value"}}
			posts, deletes := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Auth-Token") != "token" {
					t.Error("missing authentication")
				}
				switch r.Method {
				case "GET":
					if wrapped {
						json.NewEncoder(w).Encode(map[string]any{"txt_records": records})
					} else {
						json.NewEncoder(w).Encode(records)
					}
				case "POST":
					var record dnsRecord
					json.NewDecoder(r.Body).Decode(&record)
					if record.Name != "_acme-challenge.k8s" {
						t.Errorf("unexpected name: %s", record.Name)
					}
					record.UUID = "ours"
					records = append(records, record)
					posts++
					w.WriteHeader(201)
				case "DELETE":
					if !strings.HasSuffix(r.URL.Path, "/ours") {
						t.Errorf("deleted someone else's record: %s", r.URL.Path)
					}
					records = records[:1]
					deletes++
					w.WriteHeader(204)
				}
			}))
			defer srv.Close()
			c := &dnsClient{http: srv.Client(), base: srv.URL, token: "token"}
			for _, cleanup := range []bool{false, false, true, true} {
				if err := c.reconcileTXT(context.Background(), "zone", "_acme-challenge.k8s", "ours", cleanup); err != nil {
					t.Fatal(err)
				}
			}
			if posts != 1 || deletes != 1 || len(records) != 1 || records[0].UUID != "other" {
				t.Fatalf("posts=%d deletes=%d records=%v", posts, deletes, records)
			}
		})
	}
}
func TestAPIErrorDoesNotExposeResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403); fmt.Fprint(w, "sensitive-token") }))
	defer srv.Close()
	c := &dnsClient{http: srv.Client()}
	_, err := c.request(context.Background(), "GET", srv.URL, nil, nil, 200)
	if err == nil || strings.Contains(err.Error(), "sensitive-token") {
		t.Fatalf("unsafe error: %v", err)
	}
}
