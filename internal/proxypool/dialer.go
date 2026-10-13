package proxypool

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// TransportFor builds an *http.Transport that routes through p.
func TransportFor(p Proxy) (*http.Transport, error) {
	p = Normalize(p)
	if err := Validate(p); err != nil {
		return nil, err
	}
	base := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(authority string, c *tls.Conn) http.RoundTripper{},
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       180 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 55 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	switch p.Type {
	case "http", "https":
		u, err := buildHTTPProxyURL(p)
		if err != nil {
			return nil, err
		}
		base.Proxy = http.ProxyURL(u)
		return base, nil
	case "socks5", "socks5h":
		dial, err := socksDialContext(p)
		if err != nil {
			return nil, err
		}
		base.DialContext = dial
		base.Proxy = nil
		return base, nil
	default:
		return nil, fmt.Errorf("unsupported proxy type %q", p.Type)
	}
}

func buildHTTPProxyURL(p Proxy) (*url.URL, error) {
	u := &url.URL{
		Scheme: p.Type,
		Host:   p.Address(),
	}
	if p.Username != "" || p.Password != "" {
		u.User = url.UserPassword(p.Username, p.Password)
	}
	return u, nil
}

func socksDialContext(p Proxy) (func(ctx context.Context, network, address string) (net.Conn, error), error) {
	var auth *xproxy.Auth
	if p.Username != "" || p.Password != "" {
		auth = &xproxy.Auth{User: p.Username, Password: p.Password}
	}
	forward := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	d, err := xproxy.SOCKS5("tcp", p.Address(), auth, forward)
	if err != nil {
		return nil, err
	}
	resolveLocal := p.Type == "socks5" // socks5 = local DNS; socks5h = remote DNS
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		target := address
		if resolveLocal {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			if net.ParseIP(host) == nil {
				ips, err := net.DefaultResolver.LookupHost(ctx, host)
				if err != nil {
					return nil, err
				}
				if len(ips) == 0 {
					return nil, fmt.Errorf("no ip for %s", host)
				}
				target = net.JoinHostPort(ips[0], port)
			}
		}
		if cd, ok := d.(xproxy.ContextDialer); ok {
			return cd.DialContext(ctx, network, target)
		}
		return d.Dial(network, target)
	}, nil
}

// HTTPClient returns a ready-to-use client.
// timeout<=0 means no overall deadline (required for long SSE streams);
// ResponseHeaderTimeout on the transport still bounds time-to-first-byte.
func HTTPClient(p Proxy, timeout time.Duration) (*http.Client, error) {
	tr, err := TransportFor(p)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: tr, Timeout: timeout}, nil
}
