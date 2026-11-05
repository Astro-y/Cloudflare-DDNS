package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestIntervalBoundaries(t *testing.T) {
	for input, expected := range map[string]int{"30": 30, "30s": 30, "1m": 60, "24h": 86400, "86400": 86400, "1h30m": 5400} {
		actual, err := parseInterval(input)
		if err != nil || actual != expected {
			t.Fatalf("%q => %d %v", input, actual, err)
		}
	}
	for _, input := range []string{"", "-1", "0", "29s", "24h1s", "30.1s", "invalid", "99999999999999999999999s"} {
		if _, err := parseInterval(input); err == nil {
			t.Fatalf("accepted invalid interval %q", input)
		}
	}
}

func TestPanelUninstalledInvalidBlankCancelAndEOF(t *testing.T) {
	var output bytes.Buffer
	store := NewStore(testPaths(t))
	p := NewPanel(store, strings.NewReader("\nwrong\n\n2\n\nq\n0\n"), &output)
	if err := p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"未安装", "无效选项", "尚未初始化", "已退出面板"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("missing %q in %s", expected, output.String())
		}
	}
	if _, err := os.Stat(store.Paths.ConfigDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only menu created directory: %v", err)
	}
	p = NewPanel(store, strings.NewReader(""), io.Discard)
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("EOF should exit cleanly: %v", err)
	}
}

func TestPanelInterruptUnblocksWaitingInput(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 1)
	p := NewPanel(NewStore(testPaths(t)), notifyingReader{reader, started}, io.Discard)
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("panel never requested input")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected canceled: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("panel remained blocked on input")
	}
}

type notifyingReader struct {
	io.Reader
	started chan struct{}
}

func (r notifyingReader) Read(data []byte) (int, error) {
	select {
	case r.started <- struct{}{}:
	default:
	}
	return r.Reader.Read(data)
}

func TestPanelRefusesUnhiddenSecretInput(t *testing.T) {
	var output bytes.Buffer
	p := NewPanel(NewStore(testPaths(t)), strings.NewReader("test-token\n"), &output)
	if _, err := p.secret(); err == nil {
		t.Fatal("nonterminal secret input accepted")
	}
	if strings.Contains(output.String(), "test-token") {
		t.Fatal("secret leaked")
	}
}

type panelTestCloud struct {
	CloudAPI
	records []DNSRecord
	reads   int
}

func (c *panelTestCloud) FindRecords(context.Context, string, string) ([]DNSRecord, error) {
	c.reads++
	return c.records, nil
}

func TestPanelMultipleRecordSelectionPersistsIntent(t *testing.T) {
	api := &panelTestCloud{records: []DNSRecord{{ID: "first", Type: "A", Name: "home.example.com", Content: "8.8.8.8"}, {ID: "second", Type: "A", Name: "home.example.com", Content: "1.1.1.1"}}}
	p := NewPanel(NewStore(testPaths(t)), strings.NewReader("1\nhome\nbad\n\n2\n"), io.Discard)
	r, err := p.chooseRecord(context.Background(), api, []Zone{{ID: "zone", Name: "example.com"}}, "A", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.RecordID != "second" || !r.AllowMultiple {
		t.Fatalf("explicit selection lost: %+v", r)
	}
}

func TestPanelCNAMEConflictCancelsSelection(t *testing.T) {
	api := &panelTestCloud{records: []DNSRecord{{ID: "cname", Type: "CNAME", Name: "home.example.com"}}}
	p := NewPanel(NewStore(testPaths(t)), strings.NewReader("1\nhome\n"), io.Discard)
	if _, err := p.chooseRecord(context.Background(), api, []Zone{{ID: "zone", Name: "example.com"}}, "A", nil); err == nil || !strings.Contains(err.Error(), "CNAME") {
		t.Fatalf("CNAME conflict ignored: %v", err)
	}
}

func TestPanelReadOnlyCloudRefreshDoesNotSaveConfig(t *testing.T) {
	store := NewStore(testPaths(t))
	if err := store.SaveConfig(testConfig()); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveToken("fake-token"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(store.Paths.ConfigFile())
	api := &panelTestCloud{records: []DNSRecord{{ID: "r", Type: "A", Name: "home.example.com", Content: "8.8.8.8"}}}
	p := NewPanel(store, strings.NewReader("y\n"), io.Discard)
	p.apiFactory = func(string) CloudAPI { return api }
	if err := p.details(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(store.Paths.ConfigFile())
	if api.reads != 1 || !bytes.Equal(before, after) {
		t.Fatal("cloud refresh must only read")
	}
	if _, err := os.Stat(store.Paths.StateFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refresh wrote local sync state")
	}
}

func TestCLINonInteractiveHelper(t *testing.T) {
	if os.Getenv("CFDDNS_TEST_NONINTERACTIVE") != "1" {
		return
	}
	os.Args = []string{"cfddns"}
	main()
}

func TestCLINonInteractiveHasClearError(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLINonInteractiveHelper$")
	cmd.Env = append(os.Environ(), "CFDDNS_TEST_NONINTERACTIVE=1")
	data, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(string(data), "需要交互终端") {
		t.Fatalf("unexpected CLI result: %v %s", err, data)
	}
}
