// ddnsync: a tiny noip-compatible DDNS shim that translates dyndns2-style
// updates into DNSimple API calls.
//
// Required env:
//   DNSIMPLE_TOKEN       Personal access token (account id auto-discovered)
//   AUTH_USER, AUTH_PASS Basic auth credentials the router presents
//
// Optional env:
//   LISTEN_ADDR   default :8245
//   DNSIMPLE_API  default https://api.dnsimple.com
//   RECORD_TTL    default 60
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type config struct {
	token, user, pass, addr, apiBase string
	ttl                              int
}

func loadConfig() (config, error) {
	c := config{
		token:   os.Getenv("DNSIMPLE_TOKEN"),
		user:    os.Getenv("AUTH_USER"),
		pass:    os.Getenv("AUTH_PASS"),
		addr:    os.Getenv("LISTEN_ADDR"),
		apiBase: os.Getenv("DNSIMPLE_API"),
		ttl:     60,
	}
	if c.addr == "" {
		c.addr = ":8245"
	}
	if c.apiBase == "" {
		c.apiBase = "https://api.dnsimple.com"
	}
	if v := os.Getenv("RECORD_TTL"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("invalid RECORD_TTL: %q", v)
		}
		c.ttl = n
	}
	var missing []string
	for k, v := range map[string]string{
		"DNSIMPLE_TOKEN": c.token,
		"AUTH_USER":      c.user,
		"AUTH_PASS":      c.pass,
	} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("missing env: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

type dnsimple struct {
	base, token, account string
	http                 *http.Client
}

type record struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
}

type pagination struct {
	CurrentPage int `json:"current_page"`
	TotalPages  int `json:"total_pages"`
}

func (d *dnsimple) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("dnsimple %s %s: %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (d *dnsimple) whoami(ctx context.Context) (string, error) {
	var out struct {
		Data struct {
			Account *struct {
				ID int64 `json:"id"`
			} `json:"account"`
		} `json:"data"`
	}
	if err := d.do(ctx, "GET", "/v2/whoami", nil, &out); err != nil {
		return "", err
	}
	if out.Data.Account == nil {
		return "", fmt.Errorf("token is not scoped to an account (use an account access token, not a user token)")
	}
	return strconv.FormatInt(out.Data.Account.ID, 10), nil
}

func (d *dnsimple) listZones(ctx context.Context) ([]string, error) {
	var names []string
	for page := 1; ; page++ {
		var out struct {
			Data       []struct{ Name string } `json:"data"`
			Pagination pagination              `json:"pagination"`
		}
		if err := d.do(ctx, "GET",
			fmt.Sprintf("/v2/%s/zones?page=%d&per_page=100", d.account, page),
			nil, &out); err != nil {
			return nil, err
		}
		for _, z := range out.Data {
			names = append(names, strings.ToLower(z.Name))
		}
		if out.Pagination.CurrentPage >= out.Pagination.TotalPages {
			return names, nil
		}
	}
}

func (d *dnsimple) findRecord(ctx context.Context, zone, name, recType string) (*record, error) {
	q := url.Values{"name": {name}, "type": {recType}}
	var out struct {
		Data []record `json:"data"`
	}
	if err := d.do(ctx, "GET",
		fmt.Sprintf("/v2/%s/zones/%s/records?%s", d.account, zone, q.Encode()),
		nil, &out); err != nil {
		return nil, err
	}
	for i := range out.Data {
		// DNSimple's name filter is exact, but be defensive.
		if strings.EqualFold(out.Data[i].Name, name) && out.Data[i].Type == recType {
			return &out.Data[i], nil
		}
	}
	return nil, nil
}

func (d *dnsimple) createRecord(ctx context.Context, zone, name, recType, content string, ttl int) error {
	return d.do(ctx, "POST",
		fmt.Sprintf("/v2/%s/zones/%s/records", d.account, zone),
		map[string]any{"name": name, "type": recType, "content": content, "ttl": ttl}, nil)
}

func (d *dnsimple) updateRecord(ctx context.Context, zone string, id int64, content string) error {
	return d.do(ctx, "PATCH",
		fmt.Sprintf("/v2/%s/zones/%s/records/%d", d.account, zone, id),
		map[string]any{"content": content}, nil)
}

type server struct {
	cfg   config
	dns   *dnsimple
	zones []string // sorted longest-first, lowercase
}

func (s *server) splitHost(host string) (zone, name string, ok bool) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, z := range s.zones {
		if host == z {
			return z, "", true
		}
		if strings.HasSuffix(host, "."+z) {
			return z, strings.TrimSuffix(host, "."+z), true
		}
	}
	return "", "", false
}

func detectType(ip string) (string, bool) {
	addr := net.ParseIP(ip)
	if addr == nil {
		return "", false
	}
	if addr.To4() != nil {
		return "A", true
	}
	return "AAAA", true
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

func constEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (s *server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	user, pass, ok := r.BasicAuth()
	if !ok || !constEq(user, s.cfg.user) || !constEq(pass, s.cfg.pass) {
		w.Header().Set("WWW-Authenticate", `Basic realm="ddnsync"`)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintln(w, "badauth")
		return
	}

	host := r.URL.Query().Get("hostname")
	if host == "" {
		fmt.Fprintln(w, "nohost")
		return
	}

	ip := r.URL.Query().Get("myip")
	if ip == "" {
		ip = clientIP(r)
	}
	recType, ok := detectType(ip)
	if !ok {
		log.Printf("update %s: unparseable ip %q", host, ip)
		fmt.Fprintln(w, "911")
		return
	}

	zone, name, ok := s.splitHost(host)
	if !ok {
		log.Printf("update %s: no matching zone", host)
		fmt.Fprintln(w, "nohost")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	rec, err := s.dns.findRecord(ctx, zone, name, recType)
	if err != nil {
		log.Printf("update %s: find: %v", host, err)
		fmt.Fprintln(w, "911")
		return
	}

	if rec == nil {
		if err := s.dns.createRecord(ctx, zone, name, recType, ip, s.cfg.ttl); err != nil {
			log.Printf("update %s: create: %v", host, err)
			fmt.Fprintln(w, "911")
			return
		}
		log.Printf("update %s: created %s -> %s", host, recType, ip)
		fmt.Fprintf(w, "good %s\n", ip)
		return
	}

	if rec.Content == ip {
		fmt.Fprintf(w, "nochg %s\n", ip)
		return
	}

	if err := s.dns.updateRecord(ctx, zone, rec.ID, ip); err != nil {
		log.Printf("update %s: patch: %v", host, err)
		fmt.Fprintln(w, "911")
		return
	}
	log.Printf("update %s: %s -> %s", host, rec.Content, ip)
	fmt.Fprintf(w, "good %s\n", ip)
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	d := &dnsimple{
		base:  cfg.apiBase,
		token: cfg.token,
		http:  &http.Client{Timeout: 15 * time.Second},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	account, err := d.whoami(ctx)
	if err != nil {
		cancel()
		log.Fatalf("whoami: %v", err)
	}
	d.account = account
	zones, err := d.listZones(ctx)
	cancel()
	if err != nil {
		log.Fatalf("list zones: %v", err)
	}
	// Longest first so com.example.foo matches before example.foo.
	sortLongestFirst(zones)

	s := &server{cfg: cfg, dns: d, zones: zones}

	mux := http.NewServeMux()
	mux.HandleFunc("/nic/update", s.handleUpdate)
	mux.HandleFunc("/v2/update", s.handleUpdate) // alias some clients use
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("ddnsync listening on %s; %d zones cached", cfg.addr, len(zones))
	log.Fatal(srv.ListenAndServe())
}

func sortLongestFirst(s []string) {
	// tiny insertion sort; zone counts are small
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && len(s[j]) > len(s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}