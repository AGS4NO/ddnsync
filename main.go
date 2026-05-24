// ddnsync: tiny DDNS service that keeps a DNSimple record pointing at the
// host's current public IP.
//
// Two operating modes, either or both:
//
//   server mode  — accepts noip/dyndns2 GET requests on LISTEN_ADDR
//                  (set LISTEN_ADDR=off to disable)
//   poll mode    — periodically self-detects the WAN IP and updates records
//                  (set POLL_INTERVAL to enable; min 30s)
//
// Required env:
//   DNSIMPLE_TOKEN       Personal account access token (account auto-discovered)
//
// Server-mode env (required when LISTEN_ADDR != "off"):
//   AUTH_USER, AUTH_PASS Basic auth credentials the router presents
//
// Poll-mode env (required when POLL_INTERVAL is set):
//   POLL_HOSTNAMES       Comma-separated list, e.g. home.example.com,vpn.example.com
//
// Optional env:
//   LISTEN_ADDR     default :8245 ("off" disables the HTTP server)
//   DNSIMPLE_API    default https://api.dnsimple.com
//   RECORD_TTL      default 60
//   POLL_INTERVAL   e.g. 5m; absent or 0 disables poll mode
//   POLL_IP_SOURCE  default https://api.ipify.org
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type config struct {
	token, user, pass, addr, apiBase string
	ttl                              int

	pollInterval  time.Duration
	pollHostnames []string
	pollIPSource  string
}

func (c config) serverEnabled() bool { return c.addr != "" && !strings.EqualFold(c.addr, "off") }
func (c config) pollEnabled() bool   { return c.pollInterval > 0 }

func loadConfig() (config, error) {
	c := config{
		token:        os.Getenv("DNSIMPLE_TOKEN"),
		user:         os.Getenv("AUTH_USER"),
		pass:         os.Getenv("AUTH_PASS"),
		addr:         os.Getenv("LISTEN_ADDR"),
		apiBase:      os.Getenv("DNSIMPLE_API"),
		pollIPSource: os.Getenv("POLL_IP_SOURCE"),
		ttl:          60,
	}
	if c.addr == "" {
		c.addr = ":8245"
	}
	if c.apiBase == "" {
		c.apiBase = "https://api.dnsimple.com"
	}
	if c.pollIPSource == "" {
		c.pollIPSource = "https://api.ipify.org"
	}
	if v := os.Getenv("RECORD_TTL"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("invalid RECORD_TTL: %q", v)
		}
		c.ttl = n
	}
	if v := os.Getenv("POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("invalid POLL_INTERVAL: %q", v)
		}
		if d < 30*time.Second {
			return c, fmt.Errorf("POLL_INTERVAL too short (min 30s): %s", d)
		}
		c.pollInterval = d
	}
	if v := os.Getenv("POLL_HOSTNAMES"); v != "" {
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				c.pollHostnames = append(c.pollHostnames, h)
			}
		}
	}

	if c.token == "" {
		return c, fmt.Errorf("missing env: DNSIMPLE_TOKEN")
	}
	if !c.serverEnabled() && !c.pollEnabled() {
		return c, fmt.Errorf("nothing to do: set LISTEN_ADDR (server mode) and/or POLL_INTERVAL (poll mode)")
	}
	if c.serverEnabled() {
		var missing []string
		if c.user == "" {
			missing = append(missing, "AUTH_USER")
		}
		if c.pass == "" {
			missing = append(missing, "AUTH_PASS")
		}
		if len(missing) > 0 {
			return c, fmt.Errorf("server mode requires: %s", strings.Join(missing, ", "))
		}
	}
	if c.pollEnabled() && len(c.pollHostnames) == 0 {
		return c, fmt.Errorf("POLL_INTERVAL set but POLL_HOSTNAMES is empty")
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

type upsertResult int

const (
	upsertNoChange upsertResult = iota
	upsertCreated
	upsertUpdated
)

// errUnknownZone signals that no cached zone matches the hostname's suffix.
var errUnknownZone = errors.New("no matching zone")

type server struct {
	cfg      config
	dns      *dnsimple
	zones    []string // sorted longest-first, lowercase
	ipClient *http.Client
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

// upsert is the shared core: find the record for (host, ip-family), then
// create/patch/no-op against DNSimple. Used by both the HTTP handler and the
// poll loop.
func (s *server) upsert(ctx context.Context, host, ip string) (upsertResult, error) {
	zone, name, ok := s.splitHost(host)
	if !ok {
		return 0, errUnknownZone
	}
	recType, ok := detectType(ip)
	if !ok {
		return 0, fmt.Errorf("unparseable ip %q", ip)
	}
	rec, err := s.dns.findRecord(ctx, zone, name, recType)
	if err != nil {
		return 0, fmt.Errorf("find: %w", err)
	}
	if rec == nil {
		if err := s.dns.createRecord(ctx, zone, name, recType, ip, s.cfg.ttl); err != nil {
			return 0, fmt.Errorf("create: %w", err)
		}
		return upsertCreated, nil
	}
	if rec.Content == ip {
		return upsertNoChange, nil
	}
	if err := s.dns.updateRecord(ctx, zone, rec.ID, ip); err != nil {
		return 0, fmt.Errorf("patch: %w", err)
	}
	return upsertUpdated, nil
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

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	result, err := s.upsert(ctx, host, ip)
	if err != nil {
		switch {
		case errors.Is(err, errUnknownZone):
			log.Printf("update %s: no matching zone", host)
			fmt.Fprintln(w, "nohost")
		default:
			log.Printf("update %s: %v", host, err)
			fmt.Fprintln(w, "911")
		}
		return
	}
	switch result {
	case upsertCreated:
		log.Printf("update %s: created -> %s", host, ip)
		fmt.Fprintf(w, "good %s\n", ip)
	case upsertUpdated:
		log.Printf("update %s: -> %s", host, ip)
		fmt.Fprintf(w, "good %s\n", ip)
	case upsertNoChange:
		fmt.Fprintf(w, "nochg %s\n", ip)
	}
}

// fetchIP asks the configured IP-discovery service for the current WAN IP and
// validates that the response parses as one.
func (s *server) fetchIP(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.pollIPSource, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.ipClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ip source %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(b))
	if _, ok := detectType(ip); !ok {
		return "", fmt.Errorf("ip source returned %q", ip)
	}
	return ip, nil
}

func (s *server) pollOnce(ctx context.Context) {
	ipCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	ip, err := s.fetchIP(ipCtx)
	cancel()
	if err != nil {
		log.Printf("poll: fetch ip: %v", err)
		return
	}
	for _, host := range s.cfg.pollHostnames {
		opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		result, err := s.upsert(opCtx, host, ip)
		cancel()
		if err != nil {
			log.Printf("poll %s: %v", host, err)
			continue
		}
		switch result {
		case upsertCreated:
			log.Printf("poll %s: created -> %s", host, ip)
		case upsertUpdated:
			log.Printf("poll %s: -> %s", host, ip)
		case upsertNoChange:
			// silent — usual case
		}
	}
}

func (s *server) pollLoop(ctx context.Context) {
	s.pollOnce(ctx)
	t := time.NewTicker(s.cfg.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.pollOnce(ctx)
		}
	}
}

func sortLongestFirst(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && len(s[j]) > len(s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
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

	bootCtx, bootCancel := context.WithTimeout(context.Background(), 30*time.Second)
	account, err := d.whoami(bootCtx)
	if err != nil {
		bootCancel()
		log.Fatalf("whoami: %v", err)
	}
	d.account = account
	zones, err := d.listZones(bootCtx)
	bootCancel()
	if err != nil {
		log.Fatalf("list zones: %v", err)
	}
	sortLongestFirst(zones)

	s := &server{
		cfg:      cfg,
		dns:      d,
		zones:    zones,
		ipClient: &http.Client{Timeout: 10 * time.Second},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	var httpSrv *http.Server

	if cfg.pollEnabled() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Printf("poll: every %s, hosts=%v, source=%s",
				cfg.pollInterval, cfg.pollHostnames, cfg.pollIPSource)
			s.pollLoop(ctx)
		}()
	}

	if cfg.serverEnabled() {
		mux := http.NewServeMux()
		mux.HandleFunc("/nic/update", s.handleUpdate)
		mux.HandleFunc("/v2/update", s.handleUpdate)
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintln(w, "ok")
		})
		httpSrv = &http.Server{
			Addr:              cfg.addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Printf("ddnsync listening on %s; %d zones cached", cfg.addr, len(zones))
			if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("http server: %v", err)
				stop() // bring the whole process down on listener failure
			}
		}()
	}

	<-ctx.Done()
	log.Printf("shutting down")
	if httpSrv != nil {
		sdCtx, sdCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = httpSrv.Shutdown(sdCtx)
		sdCancel()
	}
	wg.Wait()
}