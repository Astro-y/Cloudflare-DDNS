package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"
)

type fakeCloud struct {
	records                       []DNSRecord
	getErr                        error
	findErr                       error
	createErr                     error
	patchErr                      error
	createdDespiteError           bool
	createResponseTTL             int
	gets, finds, creates, patches int
	fields                        map[string]any
}

func (f *fakeCloud) VerifyToken(context.Context) error         { return nil }
func (f *fakeCloud) ListZones(context.Context) ([]Zone, error) { return nil, nil }
func (f *fakeCloud) FindRecords(context.Context, string, string) ([]DNSRecord, error) {
	f.finds++
	return f.records, f.findErr
}
func (f *fakeCloud) GetRecord(_ context.Context, _ string, id string) (DNSRecord, error) {
	f.gets++
	if f.getErr != nil {
		return DNSRecord{}, f.getErr
	}
	for _, record := range f.records {
		if record.ID == id {
			return record, nil
		}
	}
	return DNSRecord{}, &APIError{StatusCode: 404}
}
func (f *fakeCloud) CreateRecord(_ context.Context, _ string, record DNSRecord) (DNSRecord, error) {
	f.creates++
	record.ID = "created"
	if f.createResponseTTL != 0 {
		record.TTL = f.createResponseTTL
	}
	if f.createErr == nil || f.createdDespiteError {
		f.records = append(f.records, record)
	}
	return record, f.createErr
}
func (f *fakeCloud) PatchRecord(_ context.Context, _ string, _ string, fields map[string]any) (DNSRecord, error) {
	f.patches++
	f.fields = fields
	return DNSRecord{}, f.patchErr
}

type fakeResolver struct {
	addresses map[string]string
	failures  map[string]error
}

func (f fakeResolver) Resolve(_ context.Context, _ IPSource, kind, previous string) (string, error) {
	if err := f.failures[kind]; err != nil {
		return "", err
	}
	return f.addresses[kind], nil
}

func syncFixture() (*Config, *State, *fakeCloud, *SyncEngine, time.Time) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := &Config{Version: 1, IntervalSeconds: 60, TTL: 1, Records: []RecordConfig{{Type: "A", Name: "home.example.com", ZoneID: "zone", Enabled: true, Source: IPSource{Mode: "external"}}}}
	state := &State{Version: 1, Records: map[string]*RecordState{}}
	cloud := &fakeCloud{}
	engine := &SyncEngine{API: cloud, Resolver: fakeResolver{addresses: map[string]string{"A": "1.1.1.1", "AAAA": "2606:4700:4700::1111"}}, Now: func() time.Time { return now }}
	return cfg, state, cloud, engine, now
}

func TestSyncCreatesMissingAndStoresIdentity(t *testing.T) {
	cfg, state, cloud, engine, now := syncFixture()
	results := engine.Sync(context.Background(), cfg, state, false)
	if len(results) != 1 || results[0].Err != nil || !results[0].Changed || cloud.creates != 1 || cloud.patches != 0 || cfg.Records[0].RecordID != "created" {
		t.Fatalf("results=%+v cfg=%+v cloud=%+v", results, cfg, cloud)
	}
	current := state.Records["A"]
	if current.Address != "1.1.1.1" || !current.LastChange.Equal(now) || !current.LastSuccess.Equal(now) || !current.LastCloudCheck.Equal(now) {
		t.Fatalf("state=%+v", current)
	}
}

func TestSyncNoChangeNoWriteAndPeriodicRepair(t *testing.T) {
	cfg, state, cloud, engine, now := syncFixture()
	cfg.Records[0].RecordID = "existing"
	state.Records["A"] = &RecordState{Name: cfg.Records[0].Name, Address: "1.1.1.1", LastCloudCheck: now.Add(-time.Minute)}
	cloud.records = []DNSRecord{{ID: "existing", Type: "A", Name: cfg.Records[0].Name, Content: "9.9.9.9", TTL: 300, Proxied: true}}
	result := engine.Sync(context.Background(), cfg, state, false)[0]
	if result.Err != nil || result.Changed || cloud.gets != 0 || cloud.patches != 0 {
		t.Fatalf("result=%+v cloud=%+v", result, cloud)
	}
	state.Records["A"].LastCloudCheck = now.Add(-10 * time.Minute)
	result = engine.Sync(context.Background(), cfg, state, false)[0]
	want := map[string]any{"content": "1.1.1.1", "ttl": 1, "proxied": false}
	if result.Err != nil || !result.Changed || !reflect.DeepEqual(cloud.fields, want) {
		t.Fatalf("result=%+v patch=%v", result, cloud.fields)
	}
}

func TestSyncCloudAlreadyCorrectNoPatch(t *testing.T) {
	cfg, state, cloud, engine, _ := syncFixture()
	cloud.records = []DNSRecord{{ID: "existing", Type: "A", Name: cfg.Records[0].Name, Content: "1.1.1.1", TTL: 1}}
	result := engine.Sync(context.Background(), cfg, state, true)[0]
	if result.Err != nil || result.Changed || cloud.patches != 0 || cloud.creates != 0 || cfg.Records[0].RecordID != "existing" {
		t.Fatalf("result=%+v cloud=%+v", result, cloud)
	}
}

func TestSyncStaleIDRelookup(t *testing.T) {
	cfg, state, cloud, engine, _ := syncFixture()
	cfg.Records[0].RecordID = "deleted"
	cloud.records = []DNSRecord{{ID: "replacement", Type: "A", Name: cfg.Records[0].Name, Content: "1.1.1.1", TTL: 1}}
	result := engine.Sync(context.Background(), cfg, state, true)[0]
	if result.Err != nil || cfg.Records[0].RecordID != "replacement" || cloud.finds != 1 || cloud.gets != 1 {
		t.Fatalf("result=%+v cloud=%+v", result, cloud)
	}
}

func TestSyncConflictsNeverWrite(t *testing.T) {
	for _, kind := range []string{"CNAME", "multiple", "stale-multiple", "saved-multiple", "saved-CNAME"} {
		t.Run(kind, func(t *testing.T) {
			cfg, state, cloud, engine, _ := syncFixture()
			if kind == "CNAME" {
				cloud.records = []DNSRecord{{ID: "alias", Type: "CNAME", Name: cfg.Records[0].Name}}
			} else {
				cloud.records = []DNSRecord{{ID: "one", Type: "A", Name: cfg.Records[0].Name}, {ID: "two", Type: "A", Name: cfg.Records[0].Name}}
				if kind == "stale-multiple" {
					cfg.Records[0].RecordID = "deleted"
				}
				if kind == "saved-multiple" || kind == "saved-CNAME" {
					cfg.Records[0].RecordID = "one"
				}
				if kind == "saved-CNAME" {
					cloud.records[1].Type = "CNAME"
					cfg.Records[0].AllowMultiple = true
				}
			}
			result := engine.Sync(context.Background(), cfg, state, true)[0]
			if result.Err == nil || cloud.creates != 0 || cloud.patches != 0 {
				t.Fatalf("result=%+v cloud=%+v", result, cloud)
			}
		})
	}
}

func TestSyncExplicitSelectedRecordCanBeManaged(t *testing.T) {
	cfg, state, cloud, engine, _ := syncFixture()
	cfg.Records[0].RecordID = "selected"
	cfg.Records[0].AllowMultiple = true
	cloud.records = []DNSRecord{{ID: "selected", Type: "A", Name: cfg.Records[0].Name, Content: "9.9.9.9", TTL: 1}, {ID: "other", Type: "A", Name: cfg.Records[0].Name, Content: "8.8.8.8", TTL: 1}}
	result := engine.Sync(context.Background(), cfg, state, true)[0]
	if result.Err != nil || cloud.patches != 1 || cloud.finds != 1 {
		t.Fatalf("result=%+v cloud=%+v", result, cloud)
	}
}

func TestSyncCreateLostResponseReconcilesWithoutDuplicate(t *testing.T) {
	cfg, state, cloud, engine, _ := syncFixture()
	cloud.createErr = errors.New("response timeout")
	cloud.createdDespiteError = true
	result := engine.Sync(context.Background(), cfg, state, true)[0]
	if result.Err != nil || !result.Changed || cloud.creates != 1 || cloud.finds != 2 || len(cloud.records) != 1 || cfg.Records[0].RecordID != "created" {
		t.Fatalf("result=%+v cloud=%+v", result, cloud)
	}
}

func TestSyncCreateUnconfirmedDoesNotRepeat(t *testing.T) {
	cfg, state, cloud, engine, _ := syncFixture()
	cloud.createErr = errors.New("timeout")
	result := engine.Sync(context.Background(), cfg, state, true)[0]
	if result.Err == nil || cloud.creates != 1 || cloud.finds != 2 {
		t.Fatalf("result=%+v cloud=%+v", result, cloud)
	}
}

func TestSyncPartialCloudChangeRecordedWithoutAdvancingSuccess(t *testing.T) {
	cfg, state, cloud, engine, now := syncFixture()
	previous := now.Add(-time.Hour)
	state.Records["A"] = &RecordState{Name: cfg.Records[0].Name, Address: "8.8.8.8", LastSuccess: previous, LastChange: previous}
	// The create succeeds, but Cloudflare returns a TTL that needs correction.
	// A failed follow-up PATCH must not hide that a new DNS record was created.
	cloud.createResponseTTL = 300
	cloud.patchErr = &APIError{StatusCode: 403}
	result := engine.Sync(context.Background(), cfg, state, true)[0]
	current := state.Records["A"]
	if result.Err == nil || !result.Changed || cloud.creates != 1 || cloud.patches != 1 || cfg.Records[0].RecordID != "created" {
		t.Fatalf("result=%+v cfg=%+v cloud=%+v", result, cfg, cloud)
	}
	if !current.LastChange.Equal(now) || !current.LastSuccess.Equal(previous) || current.LastError == "" || !current.LastCloudCheck.IsZero() || current.Address != "8.8.8.8" {
		t.Fatalf("partial change state=%+v", current)
	}
}

func TestSyncHTTPCreateTimeoutReconciles(t *testing.T) {
	cfg, state, _, engine, _ := syncFixture()
	var mu sync.Mutex
	var record *DNSRecord
	posts := 0
	client, _ := testCF(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var created DNSRecord
			if json.NewDecoder(r.Body).Decode(&created) != nil {
				t.Error("invalid request")
			}
			created.ID = "created"
			mu.Lock()
			record = &created
			posts++
			mu.Unlock()
			// Simulate successful cloud creation followed by a lost HTTP response.
			<-r.Context().Done()
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if record == nil {
			fmt.Fprint(w, `{"success":true,"result":[]}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []DNSRecord{*record}})
	})
	client.HTTPClient.Timeout = 100 * time.Millisecond
	engine.API = client
	result := engine.Sync(context.Background(), cfg, state, true)[0]
	mu.Lock()
	defer mu.Unlock()
	if result.Err != nil || !result.Changed || posts != 1 || cfg.Records[0].RecordID != "created" {
		t.Fatalf("result=%+v posts=%d config=%+v", result, posts, cfg)
	}
}

func TestSyncLostSelectedIDDoesNotChooseAnotherOfMany(t *testing.T) {
	cfg, state, cloud, engine, _ := syncFixture()
	cfg.Records[0].RecordID = "deleted"
	cfg.Records[0].AllowMultiple = true
	cloud.records = []DNSRecord{{ID: "one", Type: "A", Name: cfg.Records[0].Name}, {ID: "two", Type: "A", Name: cfg.Records[0].Name}}
	result := engine.Sync(context.Background(), cfg, state, true)[0]
	if result.Err == nil || cfg.Records[0].AllowMultiple || cloud.patches != 0 || cloud.creates != 0 {
		t.Fatalf("result=%+v cfg=%+v cloud=%+v", result, cfg, cloud)
	}
}

func TestSyncProtocolFailureIndependentAndOldAddressPreserved(t *testing.T) {
	cfg, state, cloud, engine, _ := syncFixture()
	cfg.Records = append(cfg.Records, RecordConfig{Type: "AAAA", Name: "home6.example.com", ZoneID: "zone", Enabled: true})
	state.Records["A"] = &RecordState{Name: cfg.Records[0].Name, Address: "8.8.8.8"}
	engine.Resolver = fakeResolver{addresses: map[string]string{"AAAA": "2606:4700:4700::1111"}, failures: map[string]error{"A": errors.New("offline")}}
	results := engine.Sync(context.Background(), cfg, state, true)
	if len(results) != 2 || results[0].Err == nil || results[1].Err != nil || cloud.creates != 1 || state.Records["A"].Address != "8.8.8.8" || state.Records["AAAA"].Address != "2606:4700:4700::1111" {
		t.Fatalf("results=%+v state=%+v cloud=%+v", results, state, cloud)
	}
}

func TestSyncRejectsInvalidAddress(t *testing.T) {
	for _, address := range []string{"", "invalid", "127.0.0.1", "10.0.0.1", "::1", "2606:4700:4700::1111", "192.0.2.1", "100.64.0.1", "2001:db8::1"} {
		t.Run(address, func(t *testing.T) {
			cfg, state, cloud, engine, _ := syncFixture()
			engine.Resolver = fakeResolver{addresses: map[string]string{"A": address}}
			result := engine.Sync(context.Background(), cfg, state, true)[0]
			if result.Err == nil || cloud.creates != 0 || cloud.finds != 0 {
				t.Fatalf("result=%+v cloud=%+v", result, cloud)
			}
		})
	}
}

func TestSyncDisabledAndPermissionFailure(t *testing.T) {
	cfg, state, cloud, engine, _ := syncFixture()
	cfg.Records[0].Enabled = false
	if results := engine.Sync(context.Background(), cfg, state, true); len(results) != 0 || cloud.finds != 0 {
		t.Fatalf("results=%+v cloud=%+v", results, cloud)
	}
	cfg.Records[0].Enabled = true
	cfg.Records[0].RecordID = "existing"
	cloud.getErr = &APIError{StatusCode: 403}
	result := engine.Sync(context.Background(), cfg, state, true)[0]
	if result.Err == nil || cloud.finds != 0 || cloud.creates != 0 || cloud.patches != 0 || state.Records["A"].LastError == "" {
		t.Fatalf("result=%+v cloud=%+v", result, cloud)
	}
}

func TestSyncNameChangeDoesNotReusePreviousSuccess(t *testing.T) {
	cfg, state, cloud, engine, _ := syncFixture()
	state.Records["A"] = &RecordState{Name: "old.example.com", Address: "8.8.8.8", LastSuccess: time.Now()}
	result := engine.Sync(context.Background(), cfg, state, true)[0]
	if result.Err != nil || state.Records["A"].Name != cfg.Records[0].Name || cloud.creates != 1 {
		t.Fatalf("result=%+v state=%+v", result, state)
	}
}

func TestSyncSameEngineSerializesCalls(t *testing.T) {
	cfg, state, cloud, engine, _ := syncFixture()
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() { defer wait.Done(); engine.Sync(context.Background(), cfg, state, false) }()
	}
	wait.Wait()
	if cloud.creates != 1 {
		t.Fatalf("creates=%d", cloud.creates)
	}
}
