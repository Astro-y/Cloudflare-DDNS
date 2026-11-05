package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPublicAddressFiltering(t *testing.T) {
	for _, value := range []string{"1.1.1.1", "8.8.4.4", "223.5.5.5", "2606:4700:4700::1111", "240e:1234::1"} {
		if !IsPublicIP(value) {
			t.Errorf("public address rejected: %s", value)
		}
	}
	for _, value := range []string{
		"", "localhost", "0.0.0.0", "10.2.3.4", "100.64.1.2", "127.0.0.1", "169.254.1.1", "172.31.1.1", "192.168.1.1",
		"192.0.2.1", "198.18.1.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "255.255.255.255", "::", "::1",
		"::ffff:8.8.8.8", "64:ff9b::808:808", "fc00::1", "fe80::1", "ff02::1", "2001:db8::1", "2001:20::1", "2002:808:808::1", "3fff::1", "2606:4700::1%eth0",
	} {
		if IsPublicIP(value) {
			t.Errorf("non-public address accepted: %s", value)
		}
	}
}

func TestSourceValidation(t *testing.T) {
	for _, source := range []IPSource{{Mode: "external"}, {Mode: "external", URL: "https://example.com/ip?format=text"}, {Mode: "interface", Interface: "eth0"}} {
		if err := ValidateSource(source); err != nil {
			t.Fatalf("valid source %v: %v", source, err)
		}
	}
	for _, source := range []IPSource{{}, {Mode: "external", URL: "http://example.com"}, {Mode: "external", URL: "https://user:pass@example.com"}, {Mode: "external", URL: "https://example.com/#x"}, {Mode: "external", URL: "https:///"}, {Mode: "interface"}} {
		if err := ValidateSource(source); err == nil {
			t.Errorf("accepted invalid source: %v", source)
		}
	}
}

func TestStableInterfaceSelectionAndPrefixChange(t *testing.T) {
	candidates := []addressCandidate{
		{Address: netip.MustParseAddr("2606:4700::1"), Temporary: true},
		{Address: netip.MustParseAddr("2606:4700::3")},
		{Address: netip.MustParseAddr("2606:4700::2")},
		{Address: netip.MustParseAddr("fe80::1")},
	}
	address, err := selectAddress(candidates, "AAAA", "")
	if err != nil || address != "2606:4700::2" {
		t.Fatalf("stable address: %s %v", address, err)
	}
	address, err = selectAddress(candidates, "AAAA", "2606:4700::1")
	if err != nil || address != "2606:4700::1" {
		t.Fatalf("existing eligible address should be kept: %s %v", address, err)
	}
	address, err = selectAddress(candidates, "AAAA", "240e:abcd::1")
	if err != nil || address != "2606:4700::2" {
		t.Fatalf("removed previous prefix should select current address: %s %v", address, err)
	}
	if _, err := selectAddress(candidates, "A", ""); err == nil {
		t.Fatal("IPv6 addresses must not satisfy IPv4")
	}
}

func trustedIPTestClient(t *testing.T, server *httptest.Server, network string) *http.Client {
	t.Helper()
	client := newIPHTTPClient(network, func(ctx context.Context, got, address string) (net.Conn, error) {
		if got != network {
			t.Errorf("dial network = %s, want %s", got, network)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	})
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func TestExternalFallbackProtocolAndTrace(t *testing.T) {
	var count atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		count.Add(1)
		if req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
			t.Error("IP lookup carried credentials")
		}
		switch req.URL.Path {
		case "/fail":
			http.Error(w, "failure", http.StatusServiceUnavailable)
		case "/v4":
			_, _ = w.Write([]byte("8.8.8.8\n"))
		case "/v6":
			_, _ = w.Write([]byte("fl=123\nip=2606:4700:4700::1111\ntls=TLSv1.3\n"))
		}
	}))
	defer server.Close()
	r := &IPResolver{
		clients: map[string]*http.Client{"A": trustedIPTestClient(t, server, "tcp4"), "AAAA": trustedIPTestClient(t, server, "tcp6")},
		urls:    map[string][]string{"A": {server.URL + "/v4"}, "AAAA": {server.URL + "/v6"}},
	}
	address, err := r.Resolve(context.Background(), IPSource{Mode: "external", URL: server.URL + "/fail"}, "A", "")
	if err != nil || address != "8.8.8.8" || count.Load() != 2 {
		t.Fatalf("fallback = %q, %v (%d requests)", address, err, count.Load())
	}
	address, err = r.Resolve(context.Background(), IPSource{Mode: "external"}, "AAAA", "")
	if err != nil || address != "2606:4700:4700::1111" {
		t.Fatalf("trace = %q, %v", address, err)
	}
	if _, err = r.query(context.Background(), server.URL+"/v4", "AAAA"); err == nil {
		t.Fatal("wrong IP protocol accepted")
	}
}

func TestIPResponseLimitsAndRedirectSecurity(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/large":
			_, _ = w.Write([]byte(strings.Repeat("x", maxIPResponse+1)))
		case "/redirect":
			http.Redirect(w, req, "http://example.com/plain", http.StatusFound)
		case "/private":
			_, _ = w.Write([]byte("192.168.1.1"))
		default:
			_, _ = w.Write([]byte("8.8.8.8"))
		}
	}))
	defer server.Close()
	r := &IPResolver{clients: map[string]*http.Client{"A": trustedIPTestClient(t, server, "tcp4")}}
	for _, path := range []string{"/large", "/redirect", "/private"} {
		if _, err := r.query(context.Background(), server.URL+path, "A"); err == nil {
			t.Errorf("accepted invalid response at %s", path)
		}
	}
	// The production client must reject the untrusted test CA.
	r.clients["A"] = newIPHTTPClient("tcp4", nil)
	defer r.clients["A"].CloseIdleConnections()
	if _, err := r.query(context.Background(), server.URL, "A"); err == nil {
		t.Fatal("HTTPS certificate verification disabled")
	}
}

func TestIPClientHasNoProxyAndForcesNetwork(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	for _, network := range []string{"tcp4", "tcp6"} {
		var got string
		client := newIPHTTPClient(network, func(_ context.Context, n, _ string) (net.Conn, error) {
			got = n
			return nil, errors.New("dial test")
		})
		if client.Transport.(*http.Transport).Proxy != nil {
			t.Fatal("IP client uses proxy")
		}
		_, _ = client.Get("https://example.com/ip?private=do-not-log")
		if got != network {
			t.Errorf("forced network = %s, want %s", got, network)
		}
		client.CloseIdleConnections()
	}
	r := NewIPResolver()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Resolve(ctx, IPSource{Mode: "external"}, "A", ""); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled lookup = %v", err)
	}
}

func TestCustomSourceQueryNeverLeaksInError(t *testing.T) {
	client := newIPHTTPClient("tcp4", func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("offline") })
	r := &IPResolver{clients: map[string]*http.Client{"A": client}, urls: map[string][]string{"A": {}}}
	_, err := r.Resolve(context.Background(), IPSource{Mode: "external", URL: "https://example.com/ip?key=secret-value"}, "A", "")
	if err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("error must not expose custom query: %v", err)
	}
}
