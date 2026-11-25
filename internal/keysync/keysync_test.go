package keysync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sillykea/llm-gateway/internal/metrics"
)

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// controlPlane serves whatever body the test sets, with an ETag.
type controlPlane struct {
	body   atomic.Value // string
	status atomic.Int32
	hits   atomic.Int32
	srv    *httptest.Server
}

func newControlPlane(t *testing.T) *controlPlane {
	cp := &controlPlane{}
	cp.body.Store(`{"keys":[]}`)
	cp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cp.hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer sync-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if s := cp.status.Load(); s != 0 {
			w.WriteHeader(int(s))
			return
		}
		body := cp.body.Load().(string)
		etag := `"` + hash(body)[:16] + `"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(cp.srv.Close)
	return cp
}

func newSyncer(cp *controlPlane, reserved ...string) (*Syncer, *metrics.Registry) {
	m := metrics.New()
	res := map[string]bool{}
	for _, r := range reserved {
		res[r] = true
	}
	return New(Options{
		URL: cp.srv.URL, Token: "sync-token", Metrics: m,
		Reserved: func() map[string]bool { return res },
	}), m
}

func TestNotReadyUntilFirstSync(t *testing.T) {
	cp := newControlPlane(t)
	cp.status.Store(http.StatusInternalServerError)
	s, _ := newSyncer(cp)

	if err := s.Sync(context.Background()); err == nil {
		t.Fatal("expected an error from a failing control plane")
	}
	if s.Ping(context.Background()) == nil {
		t.Fatal("must not be ready before a successful sync")
	}

	cp.status.Store(0)
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("should be ready after a sync, even with zero keys: %v", err)
	}
}

func TestLookupByHash(t *testing.T) {
	cp := newControlPlane(t)
	cp.body.Store(fmt.Sprintf(`{"keys":[{"name":"team-a","sha256":%q,"rpm":60,"monthly_budget_usd":25}]}`, hash("lgw_secret")))
	s, m := newSyncer(cp)
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	k, ok := s.Lookup("lgw_secret")
	if !ok || k.Name != "team-a" || k.RPM != 60 || k.MonthlyBudgetUSD != 25 {
		t.Fatalf("lookup = %+v, %v", k, ok)
	}
	if _, ok := s.Lookup("lgw_wrong"); ok {
		t.Fatal("wrong token matched")
	}
	if _, ok := s.Lookup(""); ok {
		t.Fatal("empty token matched")
	}
	if got := m.Value("llm_gateway_remote_keys", nil); got != 1 {
		t.Fatalf("remote_keys gauge = %v", got)
	}
}

func TestRevokedKeyStopsWorking(t *testing.T) {
	cp := newControlPlane(t)
	cp.body.Store(fmt.Sprintf(`{"keys":[{"name":"a","sha256":%q}]}`, hash("k1")))
	s, _ := newSyncer(cp)
	s.Sync(context.Background())
	if _, ok := s.Lookup("k1"); !ok {
		t.Fatal("key should work before revocation")
	}

	cp.body.Store(`{"keys":[]}`)
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup("k1"); ok {
		t.Fatal("revoked key still works")
	}
}

func TestFailStaticOnOutage(t *testing.T) {
	cp := newControlPlane(t)
	cp.body.Store(fmt.Sprintf(`{"keys":[{"name":"a","sha256":%q}]}`, hash("k1")))
	s, m := newSyncer(cp)
	s.Sync(context.Background())

	cp.status.Store(http.StatusBadGateway)
	if err := s.Sync(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if _, ok := s.Lookup("k1"); !ok {
		t.Fatal("an outage must not revoke keys")
	}
	if m.Value("llm_gateway_key_syncs_total", metrics.Labels{"result": "error"}) != 1 {
		t.Fatal("failed sync not counted")
	}
}

func TestInvalidListIsRejectedWhole(t *testing.T) {
	cases := map[string]string{
		"bad json":       `{"keys":[`,
		"bad hash":       `{"keys":[{"name":"b","sha256":"nothex"}]}`,
		"short hash":     `{"keys":[{"name":"b","sha256":"abcd"}]}`,
		"duplicate name": fmt.Sprintf(`{"keys":[{"name":"b","sha256":%q},{"name":"b","sha256":%q}]}`, hash("x"), hash("y")),
		"no name":        fmt.Sprintf(`{"keys":[{"name":"","sha256":%q}]}`, hash("x")),
		"negative rpm":   fmt.Sprintf(`{"keys":[{"name":"b","sha256":%q,"rpm":-1}]}`, hash("x")),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			cp := newControlPlane(t)
			cp.body.Store(fmt.Sprintf(`{"keys":[{"name":"a","sha256":%q}]}`, hash("k1")))
			s, _ := newSyncer(cp)
			s.Sync(context.Background())

			cp.body.Store(body)
			if err := s.Sync(context.Background()); err == nil {
				t.Fatal("expected the list to be rejected")
			}
			if _, ok := s.Lookup("k1"); !ok {
				t.Fatal("a bad list must not replace the good one")
			}
		})
	}
}

func TestReservedNamesAreSkipped(t *testing.T) {
	cp := newControlPlane(t)
	cp.body.Store(fmt.Sprintf(`{"keys":[{"name":"default","sha256":%q},{"name":"team-b","sha256":%q}]}`, hash("x"), hash("y")))
	s, _ := newSyncer(cp, "default")
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup("x"); ok {
		t.Fatal("key clashing with a config file key should be skipped")
	}
	if _, ok := s.Lookup("y"); !ok {
		t.Fatal("other keys should still load")
	}
}

func TestNotModifiedKeepsKeys(t *testing.T) {
	cp := newControlPlane(t)
	cp.body.Store(fmt.Sprintf(`{"keys":[{"name":"a","sha256":%q}]}`, hash("k1")))
	s, m := newSyncer(cp)
	s.Sync(context.Background())
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Value("llm_gateway_key_syncs_total", metrics.Labels{"result": "not_modified"}) != 1 {
		t.Fatal("second sync should be a 304")
	}
	if _, ok := s.Lookup("k1"); !ok {
		t.Fatal("304 must keep the current keys")
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	cp := newControlPlane(t)
	s, _ := newSyncer(cp)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	for cp.hits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}
