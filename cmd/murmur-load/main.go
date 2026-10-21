// Command murmur-load puts a hub running on this machine under load: many laptops, each on one WebSocket doing
// what murmur's daemon does (hello, agent updates, checks, messages, deliveries, pings, reconnects), and team pages
// following the team's stream. It signs its laptops in with dev login, so the hub runs with DEV_LOGIN=1, and it
// refuses any hub that isn't on this computer. docs/development.md says how to use it.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

var (
	hubURL  = flag.String("hub", "http://127.0.0.1:8080", "the hub on this computer, running with DEV_LOGIN=1; several instances, comma-separated, share the laptops")
	laptops = flag.Int("laptops", 1000, "laptops, one person each")
	agents  = flag.Int("agents", 2, "agents per laptop")
	repos   = flag.Int("repos", 20, "repos the agents work in; fewer make more overlaps")
	pages   = flag.Int("pages", 0, "team pages following the team's stream")
	update  = flag.Duration("update", 10*time.Second, "each laptop reports its agents this often, on average (0: never)")
	send    = flag.Duration("send", time.Minute, "each laptop sends a message this often, on average (0: never)")
	check   = flag.Duration("check", 30*time.Second, "each laptop checks files before an edit this often, on average (0: never)")
	ramp    = flag.Duration("ramp", 10*time.Second, "spread the first connections over this long (0: all at once)")
	run     = flag.Duration("for", time.Minute, "how long to run once every laptop is connected (0: until interrupted)")
	storm   = flag.Duration("storm", 0, "every this long, every laptop drops its connection and reconnects at once (0: never)")
	every   = flag.Duration("every", 5*time.Second, "how often to print a line")
	ips     = flag.Int("ips", 1, "spread connections over 127.0.0.1 to 127.0.0.N, past the ephemeral ports of one address (Linux)")
	tokens  = flag.String("tokens", "", "file to keep the laptops' tokens in, to reuse them against a hub that kept its store")
)

// hubs are the hub's instances, from -hub: laptop i connects to hubs[i % len(hubs)], and signs in on the first.
var hubs []string

func main() {
	flag.Parse()
	if err := badFlags(); err != nil {
		fail(err)
	}
	hubs = strings.Split(*hubURL, ",")
	for _, h := range hubs {
		if u, err := url.Parse(h); err != nil || u.Scheme != "http" || !loopback(u.Hostname()) {
			fail(errors.New("-hub must be http:// on localhost or a loopback address: murmur-load never loads a hub others use"))
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fmt.Printf("signing in %d laptops and %d pages\n", *laptops, *pages)
	t0 := time.Now()
	toks, err := signIn(ctx, *laptops)
	if err != nil {
		fail(err)
	}
	fmt.Printf("signed in after %.1fs\n", time.Since(t0).Seconds())

	var wg sync.WaitGroup
	for i := range *pages {
		cookie, err := devLogin(ctx, fmt.Sprintf("page%d@load.test", i))
		if err != nil {
			fail(err)
		}
		wg.Go(func() { followTeam(ctx, hubs[i%len(hubs)], cookie) })
	}
	t0 = time.Now()
	for i := range *laptops {
		l := newLaptop(i, toks[i])
		wg.Go(func() {
			if *ramp > 0 {
				select {
				case <-time.After(time.Duration(rand.Int64N(int64(*ramp)))):
				case <-ctx.Done():
					return
				}
			}
			l.run(ctx)
		})
	}
	report(ctx, t0)
	stop()
	wg.Wait()
}

// badFlags refuses the numbers the load can't run with, instead of panicking once laptops run.
func badFlags() error {
	switch {
	case *laptops < 1 || *agents < 1 || *repos < 1:
		return errors.New("-laptops, -agents and -repos must be 1 or more")
	case *laptops < 2 && *send > 0:
		return errors.New("one laptop has no one to message: -send 0, or -laptops 2 or more")
	case *every <= 0:
		return errors.New("-every must be more than 0")
	}
	return nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "murmur-load:", err)
	os.Exit(1)
}

func loopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// base is the hub's address for laptop i: its instance's port on one of -ips loopback addresses.
func base(i int) string {
	u, _ := url.Parse(hubs[i%len(hubs)])
	if *ips > 1 && u.Hostname() == "127.0.0.1" {
		u.Host = net.JoinHostPort(fmt.Sprintf("127.0.0.%d", 1+i%*ips), u.Port())
	}
	return u.String()
}

// ---------------------------------------------------------------- signing in

var web = &http.Client{
	Timeout:       time.Minute,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport:     &http.Transport{MaxIdleConnsPerHost: 64},
}

// signIn gets a token for each laptop, as murmur join does, from the -tokens file when it has them.
func signIn(ctx context.Context, n int) ([]string, error) {
	var kept []string
	if *tokens != "" {
		if b, err := os.ReadFile(*tokens); err == nil {
			kept = strings.Fields(string(b))
		}
	}
	out := make([]string, n)
	copy(out, kept)
	var next atomic.Int64
	next.Store(int64(len(kept)))
	errs := make(chan error, 64)
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			for i := int(next.Add(1) - 1); i < n; i = int(next.Add(1) - 1) {
				t, err := join(ctx, fmt.Sprintf("load%d@load.test", i))
				if err != nil {
					errs <- fmt.Errorf("signing in laptop %d: %w", i, err)
					return
				}
				out[i] = t
			}
		})
	}
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return nil, err
	}
	if *tokens != "" && n > len(kept) {
		if err := os.WriteFile(*tokens, []byte(strings.Join(out, "\n")+"\n"), 0o600); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// devLogin signs a person in on the hub's dev login and returns their session cookie.
func devLogin(ctx context.Context, email string) (*http.Cookie, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, hubs[0]+"/dev-login", strings.NewReader(url.Values{"email": {email}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := web.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == "murmur" && c.Value != "" {
			return c, nil
		}
	}
	return nil, fmt.Errorf("dev login answered %s: run the hub with DEV_LOGIN=1", resp.Status)
}

// join is murmur join with --no-browser: sign in, ask for a code with a PKCE challenge, trade it for a token.
func join(ctx context.Context, email string) (string, error) {
	cookie, err := devLogin(ctx, email)
	if err != nil {
		return "", err
	}
	verifier := protocol.RandomHex(32)
	sum := sha256.Sum256([]byte(verifier))
	var code struct{ Code string }
	if err := post(ctx, "/api/cli/login", cookie, map[string]string{"port": "0", "state": protocol.RandomHex(12),
		"challenge": base64.RawURLEncoding.EncodeToString(sum[:]), "host": "load"}, &code); err != nil {
		return "", err
	}
	var tok struct{ Token string }
	if err := post(ctx, "/cli/token", nil, map[string]string{"code": code.Code, "verifier": verifier}, &tok); err != nil {
		return "", err
	}
	return tok.Token, nil
}

func post(ctx context.Context, path string, cookie *http.Cookie, body, v any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, hubs[0]+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := web.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("%s: %s %s", path, resp.Status, msg)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// ---------------------------------------------------------------- laptops

// counts are what every laptop and page adds to, and report prints.
var counts struct {
	online, ready, welcomes, drops      atomic.Int64 // ready: online with a roster
	dropAt                              atomic.Int64 // unix ns of the first drop since all were back
	sent, delivered, acked, refused     atomic.Int64
	rosters, rosterBytes, checks        atomic.Int64
	pageEvents, pageBytes, pageFailures atomic.Int64
	mu                                  sync.Mutex
	lat, all                            []time.Duration // sender to recipient: in this interval, and since every laptop was online
	why                                 map[string]int  // why connections ended
}

// storms is closed, and replaced, to drop every laptop at once.
var storms = struct {
	sync.Mutex
	ch chan struct{}
}{ch: make(chan struct{})}

type laptop struct {
	i      int
	token  string
	agents []protocol.Agent
}

func newLaptop(i int, token string) *laptop {
	l := &laptop{i: i, token: token}
	for a := range *agents {
		repo := i*7 + a*3
		files := make([]string, 6)
		for f := range files {
			files[f] = fmt.Sprintf("internal/pkg%d/file%d.go", (repo+f)%40, (i+a+f*13)%50)
		}
		l.agents = append(l.agents, protocol.Agent{Name: fmt.Sprintf("a%d", a+1), Repo: fmt.Sprintf("acme/repo%02d", repo%*repos),
			Branch: fmt.Sprintf("feat/load-%d-%d", i, a), Title: "Make the hub absorb many more laptops than it does today",
			Status: protocol.StatusBusy, Since: time.Now().UnixMilli(), Doing: "Editing internal/hub/route.go",
			Tree: fmt.Sprintf("%016x", i*64+a), Files: files, Kinds: "rrexeerrxwreeexrrrexsreermexre", Context: 40,
			Added: 120, Deleted: 30, Changed: len(files)})
	}
	return l
}

// run keeps the laptop connected, with the daemon's backoff: 0.5 s doubling to 30 s, from 0.5 s again after a
// connection that lasted 30 s.
func (l *laptop) run(ctx context.Context) {
	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		start := time.Now()
		err := l.session(ctx)
		if errors.Is(err, errRefused) || ctx.Err() != nil {
			return
		}
		counts.drops.Add(1)
		counts.dropAt.CompareAndSwap(0, time.Now().UnixNano())
		counts.mu.Lock()
		if counts.why == nil {
			counts.why = map[string]int{}
		}
		counts.why[protocol.Cut(err.Error(), 100)]++
		counts.mu.Unlock()
		if time.Since(start) > 30*time.Second {
			backoff = 500 * time.Millisecond
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

var errRefused = errors.New("refused")

func (l *laptop) session(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	storms.Lock()
	dropped := storms.ch
	storms.Unlock()
	dctx, done := context.WithTimeout(ctx, 15*time.Second)
	c, resp, err := websocket.Dial(dctx, "ws"+strings.TrimPrefix(base(l.i), "http")+"/ws",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + l.token}}})
	done()
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			fmt.Fprintf(os.Stderr, "laptop %d: its token is unknown to the hub: delete -tokens' file\n", l.i)
			return errRefused
		}
		return err
	}
	defer c.CloseNow()
	c.SetReadLimit(protocol.LaptopReadLimit)
	write := func(f protocol.Frame) error {
		wctx, done := context.WithTimeout(ctx, 10*time.Second)
		defer done()
		return wsjson.Write(wctx, c, f)
	}
	if err := write(protocol.Frame{T: protocol.FrameHello, Protocol: protocol.Version, Version: "load", Agents: l.agents}); err != nil {
		return err
	}
	read := make(chan error, 1)
	go func() { read <- l.read(ctx, c, write) }()

	timer := func(d time.Duration) *time.Timer {
		if d <= 0 {
			return &time.Timer{} // never fires
		}
		return time.NewTimer(jitter(d))
	}
	up, out, chk, ping := timer(*update), timer(*send), timer(*check), time.NewTicker(25*time.Second)
	defer ping.Stop()
	for {
		var err error
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-dropped:
			return errors.New("storm")
		case err = <-read:
		case <-up.C:
			up.Reset(jitter(*update))
			a := &l.agents[rand.IntN(len(l.agents))]
			a.Status = []string{protocol.StatusBusy, protocol.StatusBusy, protocol.StatusIdle, protocol.StatusWaiting}[rand.IntN(4)]
			a.Since, a.Doing = time.Now().UnixMilli(), fmt.Sprintf("Editing internal/pkg%d/file%d.go", rand.IntN(40), rand.IntN(50))
			err = write(protocol.Frame{T: protocol.FrameAgents, Agents: l.agents})
		case <-out.C:
			out.Reset(jitter(*send))
			to := rand.IntN(*laptops - 1)
			if to >= l.i {
				to++
			}
			id := protocol.RandomHex(8)
			err = write(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: id, From: l.agents[rand.IntN(len(l.agents))].Name,
				To: fmt.Sprintf("load%d/a%d", to, 1+rand.IntN(*agents)), Text: "load " + strconv.FormatInt(time.Now().UnixNano(), 10)}})
			counts.sent.Add(1)
		case <-chk.C:
			chk.Reset(jitter(*check))
			a := l.agents[rand.IntN(len(l.agents))]
			err = write(protocol.Frame{T: protocol.FrameCheck, ID: protocol.RandomHex(4), Repo: a.Repo, Tree: a.Tree, Files: a.Files[:2]})
		case <-ping.C:
			pctx, done := context.WithTimeout(ctx, 20*time.Second)
			err = c.Ping(pctx)
			done()
		}
		if err != nil {
			return err
		}
	}
}

// jitter spreads a laptop's actions: half to one and a half times d.
func jitter(d time.Duration) time.Duration { return d/2 + time.Duration(rand.Int64N(int64(d))) }

// read takes the hub's frames until the connection ends. Rosters are counted, not decoded: a few thousand laptops
// decoding every roster would load this machine more than the hub.
func (l *laptop) read(ctx context.Context, c *websocket.Conn, write func(protocol.Frame) error) error {
	online, ready := false, false
	defer func() {
		if online {
			counts.online.Add(-1)
		}
		if ready {
			counts.ready.Add(-1)
		}
	}()
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			return err
		}
		if bytes.HasPrefix(b, []byte(`{"t":"roster"`)) {
			counts.rosters.Add(1)
			counts.rosterBytes.Add(int64(len(b)))
			if !ready { // its console shows the team from now on
				ready = true
				counts.ready.Add(1)
			}
			continue
		}
		var f protocol.Frame
		if err := json.Unmarshal(b, &f); err != nil {
			return err
		}
		switch f.T {
		case protocol.FrameWelcome:
			if !online {
				online = true
				counts.online.Add(1)
				counts.welcomes.Add(1)
			}
		case protocol.FrameMsg:
			if f.Echo || f.Msg == nil {
				continue
			}
			if at, ok := strings.CutPrefix(f.Msg.Text, "load "); ok {
				if n, err := strconv.ParseInt(at, 10, 64); err == nil {
					counts.mu.Lock()
					counts.lat = append(counts.lat, time.Since(time.Unix(0, n)))
					counts.mu.Unlock()
				}
			}
			counts.delivered.Add(1)
			if err := write(protocol.Frame{T: protocol.FrameDelivered, ID: f.Msg.ID, OK: true}); err != nil {
				return err
			}
		case protocol.FrameAck:
			if f.OK {
				counts.acked.Add(1)
			} else if f.Err != "" {
				counts.refused.Add(1)
			}
		case protocol.FrameChecked:
			counts.checks.Add(1)
		}
	}
}

// ---------------------------------------------------------------- team pages

// followTeam reads the team page's stream, as its script does, and opens it again when it ends.
func followTeam(ctx context.Context, hub string, cookie *http.Cookie) {
	for ctx.Err() == nil {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, hub+"/team/events", nil)
		req.Header.Set("Accept", "text/event-stream")
		req.AddCookie(cookie)
		resp, err := (&http.Client{}).Do(req)
		if err == nil {
			sc := bufio.NewScanner(resp.Body)
			sc.Buffer(make([]byte, 64<<10), protocol.LaptopReadLimit)
			for sc.Scan() {
				if line := sc.Bytes(); bytes.HasPrefix(line, []byte("data: ")) {
					counts.pageEvents.Add(1)
					counts.pageBytes.Add(int64(len(line)))
				}
			}
			resp.Body.Close()
		}
		if ctx.Err() == nil {
			counts.pageFailures.Add(1)
			time.Sleep(time.Second)
		}
	}
}

// ---------------------------------------------------------------- report

// report prints a line every -every until -for has passed with every laptop connected, then the totals. With
// -storm, it drops every laptop that often, and says how long they took to come back, and to show the team again.
func report(ctx context.Context, start time.Time) {
	tick := time.NewTicker(*every)
	defer tick.Stop()
	var full, stormAt, backAt time.Time
	var steady int64 // delivered when every laptop was online
	var last [7]int64
	stormTick := &time.Ticker{}
	if *storm > 0 {
		stormTick = time.NewTicker(*storm)
		defer stormTick.Stop()
	}
	fmt.Println("   time online  ready  sent/s  deliv/s   p50ms   p99ms  refused  rosters/s  roster MB/s  page ev/s  page MB/s  drops")
	for {
		select {
		case <-ctx.Done():
			summary(start, full, steady)
			return
		case <-stormTick.C:
			storms.Lock()
			close(storms.ch)
			storms.ch = make(chan struct{})
			storms.Unlock()
			fmt.Printf("storm: every laptop drops and reconnects\n")
			continue
		case <-tick.C:
		}
		// after a storm, or the hub restarting: how long from the first laptop dropped until all are back
		online, ready := counts.online.Load(), counts.ready.Load()
		if at := counts.dropAt.Load(); stormAt.IsZero() && at != 0 && !full.IsZero() {
			stormAt, backAt = time.Unix(0, at), time.Time{}
		}
		if !stormAt.IsZero() && online == int64(*laptops) && backAt.IsZero() {
			backAt = time.Now()
			fmt.Printf("storm: all %d laptops back within %.1fs\n", *laptops, time.Since(stormAt).Seconds())
		}
		if !stormAt.IsZero() && ready == int64(*laptops) {
			fmt.Printf("storm: all %d laptops show the team again within %.1fs\n", *laptops, time.Since(stormAt).Seconds())
			stormAt = time.Time{}
			counts.dropAt.Store(0)
		}
		now := [7]int64{counts.sent.Load(), counts.delivered.Load(), counts.refused.Load(), counts.rosters.Load(),
			counts.rosterBytes.Load(), counts.pageEvents.Load(), counts.pageBytes.Load()}
		counts.mu.Lock()
		lat := counts.lat
		counts.lat = nil
		if !full.IsZero() { // the steady state, not the first connections
			counts.all = append(counts.all, lat...)
		}
		counts.mu.Unlock()
		secs := every.Seconds()
		rate := func(i int) float64 { return float64(now[i]-last[i]) / secs }
		fmt.Printf("%6.0fs %6d %6d %7.0f %8.0f %7.1f %7.1f %8d %10.0f %12.1f %10.0f %10.1f %6d\n", time.Since(start).Seconds(), online,
			ready, rate(0), rate(1), pct(lat, 50), pct(lat, 99), now[2]-last[2], rate(3), rate(4)/1e6, rate(5), rate(6)/1e6,
			counts.drops.Load())
		last = now
		if online == int64(*laptops) && full.IsZero() {
			full, steady = time.Now(), counts.delivered.Load()
			counts.dropAt.Store(0) // drops while connecting are no storm
			fmt.Printf("all %d laptops online after %.1fs\n", *laptops, time.Since(start).Seconds())
		}
		if *run > 0 && !full.IsZero() && time.Since(full) >= *run { // whether the hub still holds them all or not
			summary(start, full, steady)
			return
		}
		if full.IsZero() && time.Since(start) > *ramp+time.Minute {
			fmt.Printf("not every laptop came online: %d of %d after %.0fs\n", online, *laptops, time.Since(start).Seconds())
			summary(start, full, steady)
			return
		}
	}
}

// summary prints the totals, and the deliveries and their latency once every laptop was online (full).
func summary(start, full time.Time, steady int64) {
	counts.mu.Lock()
	all := counts.all
	counts.mu.Unlock()
	rate := 0.0
	if !full.IsZero() {
		rate = float64(counts.delivered.Load()-steady) / time.Since(full).Seconds()
	}
	fmt.Printf("summary: %d laptops, %.0fs: %d sent, %d delivered, %d acked, %d refused; online: %.0f delivered/s, latency p50 %.1fms "+
		"p99 %.1fms max %.1fms; %d rosters (%.1f MB); %d checks; %d page events (%.1f MB), %d page failures; %d drops\n",
		*laptops, time.Since(start).Seconds(), counts.sent.Load(), counts.delivered.Load(), counts.acked.Load(), counts.refused.Load(),
		rate, pct(all, 50), pct(all, 99), pct(all, 100), counts.rosters.Load(), float64(counts.rosterBytes.Load())/1e6,
		counts.checks.Load(), counts.pageEvents.Load(), float64(counts.pageBytes.Load())/1e6, counts.pageFailures.Load(), counts.drops.Load())
	counts.mu.Lock()
	defer counts.mu.Unlock()
	for why, n := range counts.why {
		fmt.Printf("  %6d dropped: %s\n", n, why)
	}
}

// pct is the p-th percentile of ds, in milliseconds.
func pct(ds []time.Duration, p int) float64 {
	if len(ds) == 0 {
		return 0
	}
	ds = slices.Clone(ds)
	slices.Sort(ds)
	return float64(ds[min(len(ds)-1, len(ds)*p/100)].Microseconds()) / 1000
}
