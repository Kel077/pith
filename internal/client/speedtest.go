package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Kel077/pith/internal/api"
	proxyegress "github.com/Kel077/pith/internal/proxy/egress"
)

// A speed test asks Cloudflare's speed test, which answers from its data
// center nearest to the interface's network, as speed.cloudflare.com does
// in a browser: speedRounds small requests for the latency, then a download
// and an upload, each for speedPart at most. It doesn't answer downloads of
// 100 MB, nor uploads of 10 MB.
const (
	speedServer   = "https://speed.cloudflare.com"
	speedRounds   = 5
	speedRound    = 2 * time.Second // for one small request, but the first
	speedDownload = 25_000_000
	speedUpload   = 8_000_000
	speedPart     = 8 * time.Second
	speedLeast    = 1 << 20 // bytes moved for a speed cut short to count
)

func (c *Client) SpeedTest(ctx context.Context, iface string) (api.SpeedTest, error) {
	result := api.SpeedTest{Interface: iface, At: time.Now()}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if iface == "" {
		return result, fmt.Errorf("speed test needs an interface name")
	}
	interfaces, err := c.interfaces(ctx)
	if err != nil {
		return result, err
	}
	// IPv4 when the interface has it: the public IP people look for, and
	// the one changing a hotspot's IP changes.
	network, dns := "tcp4", []string(nil)
	for _, link := range interfaces {
		if link.Name == iface {
			dns = link.DNS
			if len(link.IPv4) == 0 && len(link.IPv6) > 0 {
				network = "tcp6"
			}
		}
	}
	dialer, err := c.dialer(iface, probeTimeout, dns)
	if err != nil {
		result.Problem = err.Error()
		return result, nil
	}
	names := newProbeDNS(dialer, iface, dns, proxyegress.FallbackDNS(interfaces, proxyegress.SystemDNS()))
	transport := &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 1, DisableCompression: true, TLSHandshakeTimeout: probeTimeout,
		DialContext: func(ctx context.Context, _ string, address string) (net.Conn, error) {
			return names.dial(ctx, dialer, network, address)
		}}
	defer transport.CloseIdleConnections()
	t := speedTester{base: c.speedURL, agent: "Pith/" + c.opts.Version, client: &http.Client{Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if err := t.latency(ctx, &result); err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		result.Problem = err.Error()
		return result, nil
	}
	result.OK = true
	var problems []error
	if result.Download, err = t.download(ctx); err != nil {
		problems = append(problems, fmt.Errorf("download: %w", err))
	}
	if result.Upload, err = t.upload(ctx, result.Latency); err != nil {
		problems = append(problems, fmt.Errorf("upload: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if len(problems) > 0 {
		result.Problem = errors.Join(problems...).Error()
	}
	return result, nil
}

type speedTester struct {
	base, agent string
	client      *http.Client
}

func (t speedTester) request(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, t.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", t.agent)
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	res, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("speed test server: status %d", res.StatusCode)
	}
	return res, nil
}

// latency is the shortest time a small request took to be answered, but the
// time the server says it took to answer, over the connection the first
// opens. The first answer also tells the server's place and the public IP
// it saw.
func (t speedTester) latency(ctx context.Context, r *api.SpeedTest) error {
	best, last := time.Duration(-1), error(nil)
	for i := range speedRounds {
		limit := speedRound
		if i == 0 {
			limit = probeTimeout // it connects
		}
		took, header, err := t.round(ctx, limit)
		if err != nil {
			if i == 0 || ctx.Err() != nil {
				return err
			}
			last = err
			continue
		}
		if i == 0 {
			r.Server, r.PublicIP = serverPlace(header), publicIP(header)
		}
		if best < 0 || took < best {
			best = took
		}
	}
	if best < 0 {
		return last
	}
	r.Latency = best
	return nil
}

func (t speedTester) round(ctx context.Context, limit time.Duration) (time.Duration, http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	var mu sync.Mutex
	var wrote, first time.Time
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest:         func(httptrace.WroteRequestInfo) { mu.Lock(); wrote = time.Now(); mu.Unlock() },
		GotFirstResponseByte: func() { mu.Lock(); first = time.Now(); mu.Unlock() },
	})
	res, err := t.request(ctx, http.MethodGet, "/__down?bytes=0", nil)
	if err != nil {
		return 0, nil, err
	}
	_, err = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<10))
	res.Body.Close()
	mu.Lock()
	timed, took := !wrote.IsZero() && !first.IsZero(), first.Sub(wrote)
	mu.Unlock()
	if err != nil || !timed || took <= 0 {
		return 0, nil, cmpOr(err, errors.New("speed test server: no timing"))
	}
	return max(took-serverTime(res.Header), 0), res.Header, nil
}

// download times speedDownload bytes, from the answer until the last.
func (t speedTester) download(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, speedPart)
	defer cancel()
	res, err := t.request(ctx, http.MethodGet, "/__down?bytes="+strconv.Itoa(speedDownload), nil)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	start := time.Now()
	n, err := io.Copy(io.Discard, io.LimitReader(res.Body, speedDownload))
	return speed(n, time.Since(start), err)
}

// upload times speedUpload bytes, from the first sent until the answer, but
// a round trip.
func (t speedTester) upload(ctx context.Context, latency time.Duration) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, speedPart)
	defer cancel()
	body := &sentBytes{left: speedUpload}
	res, err := t.request(ctx, http.MethodPost, "/__up", body)
	end := time.Now()
	n, start := body.sent()
	if err == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<10))
		res.Body.Close()
		if n < speedUpload {
			err = cmpOr(err, errors.New("the server answered before the upload ended"))
		}
	}
	if start.IsZero() {
		return 0, cmpOr(err, errors.New("nothing was sent"))
	}
	elapsed := end.Sub(start)
	if err == nil && elapsed > 2*latency {
		elapsed -= latency
	}
	return speed(n, elapsed, err)
}

// speed is bits per second, of a transfer complete, or cut short by its
// time after moving speedLeast bytes or more.
func speed(n int64, elapsed time.Duration, err error) (int64, error) {
	if err != nil && (!errors.Is(err, context.DeadlineExceeded) || n < speedLeast) {
		return 0, err
	}
	if elapsed <= 0 || n <= 0 {
		return 0, errors.New("nothing was timed")
	}
	return int64(float64(n*8) / elapsed.Seconds()), nil
}

// sentBytes is an upload's body of zeros: how much the transport took, and
// when it took the first.
type sentBytes struct {
	mu    sync.Mutex
	left  int64
	n     int64
	first time.Time
}

func (s *sentBytes) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.left == 0 {
		return 0, io.EOF
	}
	if s.first.IsZero() {
		s.first = time.Now()
	}
	n := int(min(int64(len(p)), s.left))
	clear(p[:n])
	s.left -= int64(n)
	s.n += int64(n)
	return n, nil
}

func (s *sentBytes) sent() (int64, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n, s.first
}

// serverTime is how long the server says it took to answer, in its
// Server-Timing header: the sum of its durations.
func serverTime(h http.Header) time.Duration {
	var total float64
	for _, value := range h.Values("Server-Timing") {
		for _, metric := range strings.Split(value, ",") {
			for _, param := range strings.Split(metric, ";") {
				k, v, ok := strings.Cut(strings.TrimSpace(param), "=")
				if !ok || !strings.EqualFold(k, "dur") {
					continue
				}
				if ms, err := strconv.ParseFloat(strings.Trim(v, `"`), 64); err == nil && ms > 0 && ms < 60_000 {
					total += ms
				}
			}
		}
	}
	return time.Duration(total * float64(time.Millisecond))
}

// serverPlace is the server's place: Cloudflare names its data centers by
// the code of the nearest airport, "BOS".
func serverPlace(h http.Header) string {
	place := h.Get("Colo")
	if place == "" {
		_, place, _ = strings.Cut(h.Get("Cf-Ray"), "-")
	}
	if len(place) < 2 || len(place) > 8 || strings.Trim(place, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		return ""
	}
	return place
}

// publicIP is the address the server saw.
func publicIP(h http.Header) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(h.Get("Cf-Meta-Ip")))
	if err != nil || !addr.IsGlobalUnicast() {
		return ""
	}
	return addr.Unmap().String()
}

// cmpOr is err, or else otherwise.
func cmpOr(err, otherwise error) error {
	if err != nil {
		return err
	}
	return otherwise
}
