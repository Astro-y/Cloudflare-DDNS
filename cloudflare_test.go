package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testCF(t *testing.T, handler http.HandlerFunc) (*CFClient, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := NewCFClient("private-token-should-never-appear")
	client.BaseURL = server.URL
	return client, server
}

func TestCFTokenAndPagination(t *testing.T) {
	var pages []string
	client, _ := testCF(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-token-should-never-appear" {
			t.Error("missing bearer token")
		}
		if r.URL.Path == "/user/tokens/verify" {
			fmt.Fprint(w, `{"success":true,"result":{"status":"active"}}`)
			return
		}
		if r.URL.Path != "/zones" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("account.id") != "" {
			t.Error("user token unexpectedly filtered by account")
		}
		pages = append(pages, r.URL.Query().Get("page"))
		fmt.Fprintf(w, `{"success":true,"result":[{"id":"z%s","name":"example%s.com"}],"result_info":{"total_pages":2}}`, pages[len(pages)-1], pages[len(pages)-1])
	})
	if err := client.VerifyToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	zones, err := client.ListZones(context.Background())
	if err != nil || len(zones) != 2 || strings.Join(pages, ",") != "1,2" {
		t.Fatalf("zones=%v pages=%v err=%v", zones, pages, err)
	}
}

func TestCFAccountTokenVerifyAndZonePagination(t *testing.T) {
	const accountID = "0123456789abcdef0123456789ABCDEF"
	var pages []string
	client, _ := testCF(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer private-token-should-never-appear" {
			t.Error("account request lost method or bearer authentication")
		}
		if r.URL.Path == "/accounts/"+accountID+"/tokens/verify" {
			fmt.Fprint(w, `{"success":true,"result":{"status":"active"}}`)
			return
		}
		if r.URL.Path != "/zones" || r.URL.Query().Get("account.id") != accountID || r.URL.Query().Get("per_page") != "50" {
			t.Errorf("unexpected account request: %s", r.URL.RequestURI())
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		fmt.Fprintf(w, `{"success":true,"result":[{"id":"z%s","name":"example%s.com"}],"result_info":{"total_pages":2}}`, page, page)
	})
	client.AccountID = accountID
	if err := client.VerifyToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	zones, err := client.ListZones(context.Background())
	if err != nil || len(zones) != 2 || strings.Join(pages, ",") != "1,2" {
		t.Fatalf("zones=%v pages=%v err=%v", zones, pages, err)
	}
	constructed := NewCFClient("account-test-token", accountID)
	if constructed.AccountID != accountID {
		t.Fatal("constructor did not preserve explicit account ID")
	}
}

func TestCFAccountTokenRequiresActiveStatus(t *testing.T) {
	const accountID = "0123456789abcdef0123456789abcdef"
	for _, status := range []string{"active", "disabled", "expired", ""} {
		t.Run(status, func(t *testing.T) {
			client, _ := testCF(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/accounts/"+accountID+"/tokens/verify" {
					t.Errorf("wrong verification endpoint: %s", r.URL.Path)
				}
				fmt.Fprintf(w, `{"success":true,"result":{"status":%q}}`, status)
			})
			client.AccountID = accountID
			err := client.VerifyToken(context.Background())
			if (err == nil) != (status == "active") {
				t.Fatalf("status=%q err=%v", status, err)
			}
		})
	}
}

func TestCFRejectsInvalidAccountIDBeforeRequests(t *testing.T) {
	var requests atomic.Int32
	client, _ := testCF(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"success":true,"result":{"status":"active"}}`)
	})
	for _, id := range []string{
		"too-short", strings.Repeat("a", 33), strings.Repeat("g", 32),
		"../" + strings.Repeat("a", 29), strings.Repeat("a", 31) + "/",
		strings.Repeat("a", 31) + "?", strings.Repeat("a", 31) + "\n",
		" " + strings.Repeat("a", 32), strings.Repeat("界", 32),
	} {
		client.AccountID = id
		if err := client.VerifyToken(context.Background()); err == nil {
			t.Fatal("invalid account ID accepted for token verification")
		}
		if _, err := client.ListZones(context.Background()); err == nil {
			t.Fatal("invalid account ID accepted for zone listing")
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid account IDs made %d requests", requests.Load())
	}
	for _, id := range []string{"", strings.Repeat("a", 32), strings.Repeat("F", 32)} {
		if err := ValidateAccountID(id); err != nil {
			t.Fatalf("valid account ID rejected: %v", err)
		}
	}
}

func TestCFVerificationAuthenticationErrorsExplainTokenKind(t *testing.T) {
	for _, accountID := range []string{"", "0123456789abcdef0123456789abcdef"} {
		t.Run(accountID, func(t *testing.T) {
			client, _ := testCF(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"success":false,"errors":[{"code":1000,"message":"private-token-should-never-appear"}]}`)
			})
			client.AccountID = accountID
			err := client.VerifyToken(context.Background())
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized || strings.Contains(err.Error(), "private-token") || !strings.Contains(err.Error(), "Account ID") {
				t.Fatalf("verification error lost safe context: %v", err)
			}
			kind := "用户级 Token"
			if accountID != "" {
				kind = "账户级 Token"
			}
			if !strings.Contains(err.Error(), kind) {
				t.Fatalf("error missing token kind: %v", err)
			}
		})
	}
}

func TestCFFindRecordsPaginationAndEscaping(t *testing.T) {
	requests := 0
	client, _ := testCF(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Query().Get("name") != "name+tag.example.com" {
			t.Error("query escaping broken")
		}
		fmt.Fprintf(w, `{"success":true,"result":[{"id":"r%d","type":"A","name":"name+tag.example.com"}],"result_info":{"total_pages":2}}`, requests)
	})
	records, err := client.FindRecords(context.Background(), "zone", "name+tag.example.com")
	if err != nil || len(records) != 2 || requests != 2 {
		t.Fatalf("records=%v requests=%d err=%v", records, requests, err)
	}
	if _, err = client.FindRecords(context.Background(), "../zone", "x"); err == nil {
		t.Fatal("unsafe zone id accepted")
	}
}

func TestCFMinimalPatchAndDNSOnlyCreate(t *testing.T) {
	client, _ := testCF(t, func(w http.ResponseWriter, r *http.Request) {
		var fields map[string]any
		if json.NewDecoder(r.Body).Decode(&fields) != nil {
			t.Error("invalid json")
		}
		if r.Method == http.MethodPatch {
			if len(fields) != 1 || fields["ttl"] != float64(60) {
				t.Errorf("unexpected patch %v", fields)
			}
		} else if r.Method == http.MethodPost {
			if fields["proxied"] != false || fields["comment"] != nil || len(fields) != 5 {
				t.Errorf("unexpected create %v", fields)
			}
		} else {
			t.Errorf("unexpected method %s", r.Method)
		}
		fmt.Fprint(w, `{"success":true,"result":{"id":"r1","type":"A","name":"example.com","content":"1.1.1.1","ttl":60,"proxied":false}}`)
	})
	if _, err := client.PatchRecord(context.Background(), "zone", "r1", map[string]any{"ttl": 60}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateRecord(context.Background(), "zone", DNSRecord{Type: "A", Name: "example.com", Content: "1.1.1.1", TTL: 60, Proxied: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PatchRecord(context.Background(), "zone", "r1", map[string]any{"proxied": true}); err == nil {
		t.Error("proxy enable accepted")
	}
	if _, err := client.PatchRecord(context.Background(), "zone", "r1", map[string]any{"comment": "overwrite"}); err == nil {
		t.Error("unrelated field accepted")
	}
}

func TestCFRateLimitAndTransientRetry(t *testing.T) {
	var requests atomic.Int32
	client, _ := testCF(t, func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			fmt.Fprint(w, `{"success":false}`)
			return
		}
		fmt.Fprint(w, `{"success":true,"result":{"status":"active"}}`)
	})
	if err := client.VerifyToken(context.Background()); err != nil || requests.Load() != 2 {
		t.Fatalf("requests=%d err=%v", requests.Load(), err)
	}
	if got := retryAfter("3", time.Now()); got != 3*time.Second {
		t.Fatal(got)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := retryAfter(now.Add(5*time.Second).Format(http.TimeFormat), now); got != 5*time.Second {
		t.Fatal(got)
	}
}

func TestCFNoBlindPOSTRetry(t *testing.T) {
	requests := 0
	client, _ := testCF(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(503)
		fmt.Fprint(w, `{"success":false,"errors":[{"code":1000}]}`)
	})
	_, err := client.CreateRecord(context.Background(), "zone", DNSRecord{Type: "A", Name: "example.com", Content: "1.1.1.1", TTL: 1})
	if err == nil || requests != 1 {
		t.Fatalf("requests=%d err=%v", requests, err)
	}
}

func TestCFTransientServerErrorRetriesGET(t *testing.T) {
	requests := 0
	client, _ := testCF(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, `{"success":true,"result":{"status":"active"}}`)
	})
	if err := client.VerifyToken(context.Background()); err != nil || requests != 2 {
		t.Fatalf("requests=%d err=%v", requests, err)
	}
}

func TestCFErrorRedactionAndPermission(t *testing.T) {
	client, _ := testCF(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprint(w, `{"success":false,"errors":[{"code":9109,"message":"private-token-should-never-appear"}]}`)
	})
	err := client.VerifyToken(context.Background())
	var api *APIError
	if !errors.As(err, &api) || api.StatusCode != 403 || !strings.Contains(err.Error(), "权限") || strings.Contains(err.Error(), "private-token") {
		t.Fatal(err)
	}
	if !IsNotFound(&APIError{StatusCode: 400, Codes: []int{81044}}) || IsNotFound(err) {
		t.Error("not-found detection")
	}
}

func TestCFResponseLimit(t *testing.T) {
	client, _ := testCF(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Repeat("x", maxCFResponse+1)) })
	if err := client.VerifyToken(context.Background()); err == nil || !strings.Contains(err.Error(), "2 MiB") {
		t.Fatal(err)
	}
}

func TestCFRedirectBlocked(t *testing.T) {
	reached := false
	client, _ := testCF(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/leak" {
			reached = true
			fmt.Fprint(w, `{"success":true}`)
			return
		}
		http.Redirect(w, r, "/leak", 302)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := client.VerifyToken(ctx); err == nil || reached {
		t.Fatalf("redirect reached=%v err=%v", reached, err)
	}
}

func TestCFServerCertificateValidation(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":true,"result":{"status":"active"}}`)
	}))
	defer server.Close()
	client := NewCFClient("secret")
	client.BaseURL = server.URL
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := client.VerifyToken(ctx); err == nil {
		t.Fatal("untrusted self-signed server accepted")
	}
	client.HTTPClient = server.Client()
	if err := client.VerifyToken(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCF429HonorsCancellation(t *testing.T) {
	requests := 0
	client, _ := testCF(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Retry-After", "300")
		w.WriteHeader(429)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := client.VerifyToken(ctx); err == nil || requests != 1 || time.Since(start) > time.Second {
		t.Fatalf("requests=%d err=%v", requests, err)
	}
	// The daemon creates a fresh client next round; it must preserve Retry-After.
	next := NewCFClient("private-token-should-never-appear")
	next.BaseURL = client.BaseURL
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel2()
	if err := next.VerifyToken(ctx2); err == nil || requests != 1 {
		t.Fatalf("fresh client bypassed rate limit, requests=%d err=%v", requests, err)
	}
}
