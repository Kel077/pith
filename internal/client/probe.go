package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/Kel077/pith/internal/api"
	proxyegress "github.com/Kel077/pith/internal/proxy/egress"
)

const probeTimeout = 8 * time.Second

func (c *Client) probeLimit() time.Duration {
	if c.probeTimeout > 0 {
		return c.probeTimeout
	}
	return probeTimeout
}

func (c *Client) Probe(ctx context.Context, iface, target string) (api.Probe, error) {
	result := api.Probe{Interface: iface, Target: target, At: c.exitTime()}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if iface == "" {
		return result, fmt.Errorf("probe needs an interface name")
	}
	interfaces, err := c.Interfaces(ctx)
	if err != nil {
		return result, err
	}
	if target == "" {
		if link := (api.InterfaceRef{Name: iface}).Resolve(interfaces); link != nil {
			return c.publicProbe(ctx, *link, interfaces, nil)
		}
	}
	return c.probe(ctx, iface, target, interfaces)
}

type probeOptions struct {
	timeout        time.Duration
	skipV4, skipV6 bool
	preferFallback bool
	dnsCache       *probeDNSCache
	cachedOnly     bool
}

func (c *Client) probe(ctx context.Context, iface, target string, interfaces []api.Interface) (api.Probe, error) {
	return c.probeWith(ctx, iface, target, interfaces, probeOptions{})
}

func (c *Client) probeWith(ctx context.Context, iface, target string, interfaces []api.Interface, opts probeOptions) (api.Probe, error) {
	result := api.Probe{Interface: iface, Target: target, At: c.exitTime()}
	timeout := opts.timeout
	if timeout <= 0 {
		timeout = c.probeLimit()
	}
	var dns []string
	for _, link := range interfaces {
		if link.Name == iface {
			dns = link.DNS
			break
		}
	}
	dialer, err := c.dialer(iface, timeout, dns)
	if err != nil {
		result.Problem = err.Error()
		return result, nil
	}
	names := newProbeDNS(dialer, iface, dns, proxyegress.FallbackDNS(interfaces, proxyegress.SystemDNS()))
	names.preferFallback = opts.preferFallback
	names.cache = opts.dnsCache
	names.cachedOnly = opts.cachedOnly
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	if target != "" {
		host, port, err := net.SplitHostPort(target)
		if err != nil || host == "" || port == "" {
			return result, fmt.Errorf("probe target must be host:port")
		}
		conn, err := names.dial(probeCtx, dialer, "tcp", target)
		result.Latency = time.Since(started)
		if ctx.Err() != nil {
			if conn != nil {
				conn.Close()
			}
			return result, ctx.Err()
		}
		if err != nil {
			result.Problem = err.Error()
			return result, nil
		}
		result.Address, result.OK = conn.RemoteAddr().String(), true
		_ = conn.Close()
		return result, nil
	}
	type response struct {
		address netip.Addr
		remote  string
		err     error
	}
	responses := make(chan response, 2)
	var workers sync.WaitGroup
	defer workers.Wait()
	expected := 0
	// Separate families prevent a working IPv4 path from concealing broken IPv6.
	for i, url := range c.probeURLs {
		if (i == 0 && opts.skipV4) || (i == 1 && opts.skipV6) {
			continue
		}
		expected++
		workers.Go(func() {
			network := "tcp4"
			if i == 1 {
				network = "tcp6"
			}
			var remote string
			transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout,
				DialContext: func(ctx context.Context, _ string, address string) (net.Conn, error) {
					conn, err := names.dial(ctx, dialer, network, address)
					if err == nil {
						remote = conn.RemoteAddr().String()
					}
					return conn, err
				}}
			defer transport.CloseIdleConnections()
			httpClient := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
			if err != nil {
				responses <- response{err: err}
				return
			}
			req.Header.Set("User-Agent", "Pith/"+c.opts.Version)
			res, err := httpClient.Do(req)
			if err != nil {
				responses <- response{err: err}
				return
			}
			defer res.Body.Close()
			body, err := io.ReadAll(io.LimitReader(res.Body, 128))
			if err == nil && res.StatusCode != http.StatusOK {
				err = fmt.Errorf("HTTPS probe: status %d", res.StatusCode)
			}
			address := netip.Addr{}
			if err == nil {
				address, err = netip.ParseAddr(strings.TrimSpace(string(body)))
			}
			if err == nil && (!address.IsGlobalUnicast() || address.Is4() != (i == 0)) {
				err = errors.New("HTTPS probe returned an unexpected address family")
			}
			responses <- response{address: address, remote: remote, err: err}
		})
	}
	var failures []error
	for range expected {
		res := <-responses
		if res.err != nil {
			failures = append(failures, res.err)
			continue
		}
		result.OK = true
		if result.Address == "" {
			result.Address = res.remote
		}
		if res.address.Is4() {
			result.PublicIPv4 = res.address.String()
		} else {
			result.PublicIPv6 = res.address.String()
		}
	}
	result.Latency = time.Since(started)
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !result.OK && len(failures) > 0 {
		result.Problem = errors.Join(failures...).Error()
	}
	return result, nil
}
