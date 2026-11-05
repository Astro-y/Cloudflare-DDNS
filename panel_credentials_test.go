package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestPanelAccountTokenDefaultRequiresValidAccountID(t *testing.T) {
	id := strings.Repeat("a", 32)
	token := "cfat_test-only-never-print"
	var output bytes.Buffer
	p := NewPanel(NewStore(testPaths(t)), strings.NewReader("\n\nwrong-id\n"+id+"\n"), &output)
	got, err := p.selectTokenAccount(token, "")
	if err != nil || got != id {
		t.Fatalf("account selection failed: %q %v", got, err)
	}
	if strings.Contains(output.String(), token) {
		t.Fatal("token leaked in account prompts")
	}
}

func TestPanelAccountTokenCannotUseUserEndpoint(t *testing.T) {
	id := strings.Repeat("b", 32)
	p := NewPanel(NewStore(testPaths(t)), strings.NewReader("1\n2\n"+id+"\n"), io.Discard)
	got, err := p.selectTokenAccount("cfat_test", "")
	if err != nil || got != id {
		t.Fatalf("wrong token endpoint accepted: %q %v", got, err)
	}
}

func TestPanelNewUserTokenClearsPreviousAccountChoice(t *testing.T) {
	p := NewPanel(NewStore(testPaths(t)), strings.NewReader("\n"), io.Discard)
	got, err := p.selectTokenAccount("cfut_test", strings.Repeat("a", 32))
	if err != nil || got != "" {
		t.Fatalf("account association was not cleared: %q %v", got, err)
	}
}

func TestPanelAccountSelectionCanCancel(t *testing.T) {
	p := NewPanel(NewStore(testPaths(t)), strings.NewReader("2\nq\n"), io.Discard)
	if _, err := p.selectTokenAccount("cfat_test", ""); !errors.Is(err, errCanceled) {
		t.Fatalf("cancel ignored: %v", err)
	}
}

type credentialPanelAPI struct {
	CloudAPI
	verifyError error
	listError   error
	readError   error
	zones       []Zone
}

func (c *credentialPanelAPI) VerifyToken(context.Context) error         { return c.verifyError }
func (c *credentialPanelAPI) ListZones(context.Context) ([]Zone, error) { return c.zones, c.listError }
func (c *credentialPanelAPI) FindRecords(context.Context, string, string) ([]DNSRecord, error) {
	return nil, c.readError
}

func TestPanelCredentialReplacementValidatesAndPreservesOldPairOnFailure(t *testing.T) {
	for _, kind := range []string{"verification", "zone-list", "zone-scope", "dns-read", "success"} {
		t.Run(kind, func(t *testing.T) {
			store := NewStore(testPaths(t))
			if err := store.SaveConfig(testConfig()); err != nil {
				t.Fatal(err)
			}
			old := Credentials{Version: 1, Token: "old-test-secret", AccountID: strings.Repeat("a", 32)}
			if err := store.SaveCredentials(old); err != nil {
				t.Fatal(err)
			}
			newCredentials := Credentials{Version: 1, Token: "cfat_new-test-secret", AccountID: strings.Repeat("b", 32)}
			mock := &credentialPanelAPI{zones: []Zone{{ID: "zone1", Name: "example.com"}}}
			switch kind {
			case "verification":
				mock.verifyError = &APIError{StatusCode: 401}
			case "zone-list":
				mock.listError = &APIError{StatusCode: 403}
			case "zone-scope":
				mock.zones = []Zone{{ID: "different-zone", Name: "other.example"}}
			case "dns-read":
				mock.readError = &APIError{StatusCode: 403}
			}
			before, _ := os.ReadFile(store.Paths.ConfigFile())
			p := NewPanel(store, strings.NewReader(""), io.Discard)
			p.apiFactory = func(token, accountID string) CloudAPI {
				if token != newCredentials.Token || accountID != newCredentials.AccountID {
					t.Fatal("credential pair not forwarded together")
				}
				return mock
			}
			_, err := p.replaceCredentials(context.Background(), newCredentials)
			if (err == nil) != (kind == "success") {
				t.Fatalf("unexpected replacement result: %v", err)
			}
			got, loadErr := store.LoadCredentials()
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			want := old
			if kind == "success" {
				want = newCredentials
			}
			if got != want {
				t.Fatal("credential pair changed unexpectedly")
			}
			after, _ := os.ReadFile(store.Paths.ConfigFile())
			if !bytes.Equal(before, after) {
				t.Fatal("token replacement changed DNS configuration")
			}
		})
	}
}

func TestPanelLoadsSavedAccountForAPIAndDiagnostics(t *testing.T) {
	store := NewStore(testPaths(t))
	want := Credentials{Version: 1, Token: "cfat_test", AccountID: strings.Repeat("c", 32)}
	if err := store.SaveCredentials(want); err != nil {
		t.Fatal(err)
	}
	p := NewPanel(store, strings.NewReader(""), io.Discard)
	called := false
	p.apiFactory = func(token, accountID string) CloudAPI {
		called = true
		if token != want.Token || accountID != want.AccountID {
			t.Fatal("saved account was lost")
		}
		return &credentialPanelAPI{}
	}
	if _, err := p.api(); err != nil || !called {
		t.Fatalf("load failed: %v", err)
	}
}
