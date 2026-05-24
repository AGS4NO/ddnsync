package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
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
		// Expecting /v2/{account}/zones/{zone}/records[/{id}]
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
		cfg:   config{user: "u", pass: "p", ttl: 60},
		dns:   d,
		zones: []string{"example.com"},
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