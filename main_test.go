package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockDNSimple implements the slice of the DNSimple v2 API that ddnsync calls,
// backed by an in-memory record map.
type mockDNSimple struct {
	mu       sync.Mutex
	account  string
	nextID   int64
	records  map[string]*record // key = zone|name|type
	failNext int                // if >0, decrement and return 500
}

func newMockDNSimple() *mockDNSimple {
	return &mockDNSimple{
		account: "12345",
		nextID:  1000,
		records: map[string]*record{},
	}
}

func (m *mockDNSimple) key(zone, name, t string) string {
	return zone + "|" + strings.ToLower(name) + "|" + t
}

func (m *mockDNSimple) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/whoami", func(w http.ResponseWriter, _ *http.Request) {
		id, _ := strconv.ParseInt(m.account, 10, 64)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"account": map[string]any{"id": id, "email": "test@test"},
			},
		})
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.failNext > 0 {
			m.failNext--
			http.Error(w, `{"message":"forced failure"}`, http.StatusInternalServerError)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/"+m.account+"/"), "/")
		if len(parts) < 3 || parts[0] != "zones" || parts[2] != "records" {
			http.NotFound(w, r)
			return
		}
		zone := parts[1]
		switch r.Method {
		case http.MethodGet:
			name := r.URL.Query().Get("name")
			typ := r.URL.Query().Get("type")
			data := []any{}
			if rec, ok := m.records[m.key(zone, name, typ)]; ok {
				data = []any{rec}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data":       data,
				"pagination": map[string]any{"current_page": 1, "total_pages": 1},
			})
		case http.MethodPost:
			var body record
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.nextID++
			body.ID = m.nextID
			m.records[m.key(zone, body.Name, body.Type)] = &body
			_ = json.NewEncoder(w).Encode(map[string]any{"data": body})
		case http.MethodPatch:
			if len(parts) != 4 {
				http.NotFound(w, r)
				return
			}
			id, _ := strconv.ParseInt(parts[3], 10, 64)
			var body struct {
				Content string `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, rec := range m.records {
				if rec.ID == id {
					rec.Content = body.Content
					_ = json.NewEncoder(w).Encode(map[string]any{"data": rec})
					return
				}
			}
			http.NotFound(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func newTestServer(t *testing.T) (*server, *mockDNSimple) {
	t.Helper()
	m := newMockDNSimple()
	ts := httptest.NewServer(m.handler())
	t.Cleanup(ts.Close)
	d := &dnsimple{
		base:    ts.URL,
		token:   "test-token",
		account: m.account,
		http:    ts.Client(),
	}
	s := &server{
		cfg: config{
			user:         "u",
			pass:         "p",
			ttl:          60,
			addr:         ":8245",
			pollIPSource: "https://api.ipify.org",
		},
		dns:      d,
		zones:    []string{"example.com"},
		ipClient: &http.Client{Timeout: 5 * time.Second},
	}
	return s, m
}

func doUpdate(t *testing.T, s *server, query, user, pass string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/nic/update?"+query, nil)
	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}
	w := httptest.NewRecorder()
	s.handleUpdate(w, req)
	b, _ := io.ReadAll(w.Body)
	return w.Code, strings.TrimSpace(string(b))
}

// ---- handler tests ----

func TestHandleUpdate_BadAuth(t *testing.T) {
	s, _ := newTestServer(t)
	code, body := doUpdate(t, s, "hostname=home.example.com&myip=1.2.3.4", "wrong", "wrong")
	if code != http.StatusUnauthorized || body != "badauth" {
		t.Fatalf("got %d %q, want 401 badauth", code, body)
	}
}

func TestHandleUpdate_MissingAuth(t *testing.T) {
	s, _ := newTestServer(t)
	code, body := doUpdate(t, s, "hostname=home.example.com&myip=1.2.3.4", "", "")
	if code != http.StatusUnauthorized || body != "badauth" {
		t.Fatalf("got %d %q, want 401 badauth", code, body)
	}
}

func TestHandleUpdate_NoHostname(t *testing.T) {
	s, _ := newTestServer(t)
	_, body := doUpdate(t, s, "myip=1.2.3.4", "u", "p")
	if body != "nohost" {
		t.Fatalf("got %q, want nohost", body)
	}
}

func TestHandleUpdate_UnknownZone(t *testing.T) {
	s, _ := newTestServer(t)
	_, body := doUpdate(t, s, "hostname=home.other.org&myip=1.2.3.4", "u", "p")
	if body != "nohost" {
		t.Fatalf("got %q, want nohost", body)
	}
}

func TestHandleUpdate_CreateNewRecord(t *testing.T) {
	s, m := newTestServer(t)
	_, body := doUpdate(t, s, "hostname=home.example.com&myip=1.2.3.4", "u", "p")
	if body != "good 1.2.3.4" {
		t.Fatalf("got %q, want good 1.2.3.4", body)
	}
	rec := m.records[m.key("example.com", "home", "A")]
	if rec == nil || rec.Content != "1.2.3.4" || rec.TTL != 60 {
		t.Fatalf("record not created correctly: %+v", rec)
	}
}

func TestHandleUpdate_NoChange(t *testing.T) {
	s, m := newTestServer(t)
	m.records[m.key("example.com", "home", "A")] = &record{
		ID: 1, Name: "home", Type: "A", Content: "1.2.3.4",
	}
	_, body := doUpdate(t, s, "hostname=home.example.com&myip=1.2.3.4", "u", "p")
	if body != "nochg 1.2.3.4" {
		t.Fatalf("got %q, want nochg 1.2.3.4", body)
	}
}

func TestHandleUpdate_PatchExisting(t *testing.T) {
	s, m := newTestServer(t)
	m.records[m.key("example.com", "home", "A")] = &record{
		ID: 1, Name: "home", Type: "A", Content: "1.1.1.1",
	}
	_, body := doUpdate(t, s, "hostname=home.example.com&myip=2.2.2.2", "u", "p")
	if body != "good 2.2.2.2" {
		t.Fatalf("got %q, want good 2.2.2.2", body)
	}
	if got := m.records[m.key("example.com", "home", "A")].Content; got != "2.2.2.2" {
		t.Fatalf("record content = %q, want 2.2.2.2", got)
	}
}

func TestHandleUpdate_IPv6(t *testing.T) {
	s, m := newTestServer(t)
	_, body := doUpdate(t, s, "hostname=home.example.com&myip=2001:db8::1", "u", "p")
	if body != "good 2001:db8::1" {
		t.Fatalf("got %q, want good 2001:db8::1", body)
	}
	if m.records[m.key("example.com", "home", "AAAA")] == nil {
		t.Fatalf("AAAA record not created")
	}
}

func TestHandleUpdate_BadIP(t *testing.T) {
	s, _ := newTestServer(t)
	_, body := doUpdate(t, s, "hostname=home.example.com&myip=not-an-ip", "u", "p")
	if body != "911" {
		t.Fatalf("got %q, want 911", body)
	}
}

func TestHandleUpdate_UsesRemoteAddrWhenMyipMissing(t *testing.T) {
	s, m := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/nic/update?hostname=home.example.com", nil)
	req.SetBasicAuth("u", "p")
	req.RemoteAddr = "9.8.7.6:54321"
	w := httptest.NewRecorder()
	s.handleUpdate(w, req)
	b, _ := io.ReadAll(w.Body)
	if got := strings.TrimSpace(string(b)); got != "good 9.8.7.6" {
		t.Fatalf("got %q, want good 9.8.7.6", got)
	}
	if rec := m.records[m.key("example.com", "home", "A")]; rec == nil || rec.Content != "9.8.7.6" {
		t.Fatalf("record not created from RemoteAddr: %+v", rec)
	}
}

func TestHandleUpdate_DNSimpleFailure(t *testing.T) {
	s, m := newTestServer(t)
	m.failNext = 1
	_, body := doUpdate(t, s, "hostname=home.example.com&myip=1.2.3.4", "u", "p")
	if body != "911" {
		t.Fatalf("got %q, want 911", body)
	}
}

// ---- pure-function tests ----

func TestSplitHost(t *testing.T) {
	s := &server{zones: []string{"example.co.uk", "example.com"}}
	sortLongestFirst(s.zones)
	cases := []struct {
		host, zone, name string
		ok               bool
	}{
		{"home.example.com", "example.com", "home", true},
		{"example.com", "example.com", "", true},
		{"vpn.example.co.uk", "example.co.uk", "vpn", true},
		{"a.b.example.com", "example.com", "a.b", true},
		{"HOME.EXAMPLE.COM.", "example.com", "home", true},
		{"unknown.org", "", "", false},
	}
	for _, c := range cases {
		zone, name, ok := s.splitHost(c.host)
		if zone != c.zone || name != c.name || ok != c.ok {
			t.Errorf("splitHost(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.host, zone, name, ok, c.zone, c.name, c.ok)
		}
	}
}

func TestDetectType(t *testing.T) {
	cases := []struct {
		ip, typ string
		ok      bool
	}{
		{"1.2.3.4", "A", true},
		{"2001:db8::1", "AAAA", true},
		{"::1", "AAAA", true},
		{"not-an-ip", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		typ, ok := detectType(c.ip)
		if typ != c.typ || ok != c.ok {
			t.Errorf("detectType(%q) = (%q, %v), want (%q, %v)", c.ip, typ, ok, c.typ, c.ok)
		}
	}
}

func TestClientIP(t *testing.T) {
	cases := []struct {
		xff, remoteAddr, want string
	}{
		{"", "1.2.3.4:5678", "1.2.3.4"},
		{"5.6.7.8", "1.2.3.4:5678", "5.6.7.8"},
		{"5.6.7.8, 9.10.11.12", "1.2.3.4:5678", "5.6.7.8"},
		{"", "[2001:db8::1]:5678", "2001:db8::1"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = c.remoteAddr
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := clientIP(r); got != c.want {
			t.Errorf("clientIP(xff=%q, remote=%q) = %q, want %q",
				c.xff, c.remoteAddr, got, c.want)
		}
	}
}

// ---- poll-mode tests ----

// newIPSource spins up an httptest server that returns a fixed body for the
// configured IP-discovery URL.
func newIPSource(t *testing.T, body string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestFetchIP_TrimAndValidate(t *testing.T) {
	s, _ := newTestServer(t)
	ip := newIPSource(t, "  9.8.7.6\n")
	s.cfg.pollIPSource = ip.URL
	s.ipClient = ip.Client()

	got, err := s.fetchIP(context.Background())
	if err != nil {
		t.Fatalf("fetchIP: %v", err)
	}
	if got != "9.8.7.6" {
		t.Fatalf("got %q, want 9.8.7.6", got)
	}
}

func TestFetchIP_RejectsGarbage(t *testing.T) {
	s, _ := newTestServer(t)
	ip := newIPSource(t, "not-an-ip")
	s.cfg.pollIPSource = ip.URL
	s.ipClient = ip.Client()

	if _, err := s.fetchIP(context.Background()); err == nil {
		t.Fatal("expected error on garbage IP source response")
	}
}

func TestPollOnce_UpdatesAllHostnames(t *testing.T) {
	s, m := newTestServer(t)
	s.cfg.pollHostnames = []string{"home.example.com", "vpn.example.com"}
	ip := newIPSource(t, "9.8.7.6")
	s.cfg.pollIPSource = ip.URL
	s.ipClient = ip.Client()

	s.pollOnce(context.Background())

	for _, name := range []string{"home", "vpn"} {
		rec := m.records[m.key("example.com", name, "A")]
		if rec == nil || rec.Content != "9.8.7.6" {
			t.Errorf("host %s: record = %+v", name, rec)
		}
	}
}

func TestPollOnce_BadIPSourceDoesNothing(t *testing.T) {
	s, m := newTestServer(t)
	s.cfg.pollHostnames = []string{"home.example.com"}
	ip := newIPSource(t, "garbage")
	s.cfg.pollIPSource = ip.URL
	s.ipClient = ip.Client()

	s.pollOnce(context.Background())

	if len(m.records) != 0 {
		t.Fatalf("expected no records, got %d", len(m.records))
	}
}

func TestPollOnce_ContinuesPastBadHost(t *testing.T) {
	s, m := newTestServer(t)
	// First host has no matching zone, second one does.
	s.cfg.pollHostnames = []string{"home.unknown.org", "home.example.com"}
	ip := newIPSource(t, "1.2.3.4")
	s.cfg.pollIPSource = ip.URL
	s.ipClient = ip.Client()

	s.pollOnce(context.Background())

	rec := m.records[m.key("example.com", "home", "A")]
	if rec == nil || rec.Content != "1.2.3.4" {
		t.Fatalf("good host should still update: %+v", rec)
	}
}

func TestPollLoop_FiresAndStops(t *testing.T) {
	s, m := newTestServer(t)
	s.cfg.pollHostnames = []string{"home.example.com"}
	s.cfg.pollInterval = 50 * time.Millisecond

	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "1.2.3.4")
	}))
	t.Cleanup(ts.Close)
	s.cfg.pollIPSource = ts.URL
	s.ipClient = ts.Client()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.pollLoop(ctx)
		close(done)
	}()

	// Let the loop tick a couple of times, then cancel and assert it stops.
	time.Sleep(180 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pollLoop did not exit after context cancel")
	}

	if hits.Load() < 2 {
		t.Fatalf("expected at least 2 IP probes, got %d", hits.Load())
	}
	if rec := m.records[m.key("example.com", "home", "A")]; rec == nil || rec.Content != "1.2.3.4" {
		t.Fatalf("record not updated by loop: %+v", rec)
	}
}

// ---- config tests ----

func TestLoadConfig_Validation(t *testing.T) {
	// Clean slate for env vars we touch.
	keys := []string{
		"DNSIMPLE_TOKEN", "AUTH_USER", "AUTH_PASS",
		"LISTEN_ADDR", "DNSIMPLE_API", "RECORD_TTL",
		"POLL_INTERVAL", "POLL_HOSTNAMES", "POLL_IP_SOURCE",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}

	cases := []struct {
		name    string
		env     map[string]string
		wantErr string // substring; "" means must succeed
	}{
		{
			name:    "no token",
			env:     map[string]string{},
			wantErr: "DNSIMPLE_TOKEN",
		},
		{
			name: "default server mode requires auth",
			env: map[string]string{
				"DNSIMPLE_TOKEN": "tok",
			},
			wantErr: "AUTH_USER",
		},
		{
			name: "server mode happy path",
			env: map[string]string{
				"DNSIMPLE_TOKEN": "tok",
				"AUTH_USER":      "u",
				"AUTH_PASS":      "p",
			},
		},
		{
			name: "poll-only with listen off",
			env: map[string]string{
				"DNSIMPLE_TOKEN": "tok",
				"LISTEN_ADDR":    "off",
				"POLL_INTERVAL":  "5m",
				"POLL_HOSTNAMES": "home.example.com",
			},
		},
		{
			name: "poll without hostnames",
			env: map[string]string{
				"DNSIMPLE_TOKEN": "tok",
				"LISTEN_ADDR":    "off",
				"POLL_INTERVAL":  "5m",
			},
			wantErr: "POLL_HOSTNAMES",
		},
		{
			name: "everything off",
			env: map[string]string{
				"DNSIMPLE_TOKEN": "tok",
				"LISTEN_ADDR":    "off",
			},
			wantErr: "nothing to do",
		},
		{
			name: "poll interval too short",
			env: map[string]string{
				"DNSIMPLE_TOKEN": "tok",
				"LISTEN_ADDR":    "off",
				"POLL_INTERVAL":  "5s",
				"POLL_HOSTNAMES": "home.example.com",
			},
			wantErr: "too short",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range keys {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := loadConfig()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got err=%v, want substring %q", err, tc.wantErr)
			}
		})
	}
}