package cmd

import (
	"errors"
	"fmt"
	"net"
	stdhttp "net/http"
	"strconv"
	"strings"
	"sync"

	"go.uber.org/zap"
)

type clientCleanup struct {
	once  sync.Once
	funcs []func() error
}

func (c *clientCleanup) Add(f func() error) {
	if f == nil {
		return
	}
	c.funcs = append(c.funcs, f)
}

func (c *clientCleanup) Run() {
	c.once.Do(func() {
		for i := len(c.funcs) - 1; i >= 0; i-- {
			if err := c.funcs[i](); err != nil && logger != nil {
				logger.Warn("cleanup failed", zap.Error(err))
			}
		}
	})
}

type localProxyEndpoint struct {
	Host string
	Port int
}

func (e *localProxyEndpoint) Addr() string {
	return net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
}

type localProxySet struct {
	HTTP   *localProxyEndpoint
	SOCKS5 *localProxyEndpoint
}

func (s *localProxySet) HasAny() bool {
	return s != nil && (s.HTTP != nil || s.SOCKS5 != nil)
}

func buildLocalProxySet(config clientConfig) (*localProxySet, error) {
	proxies := &localProxySet{}
	if config.HTTP != nil && config.HTTP.Listen != "" {
		ep, err := parseLocalProxyEndpoint(config.HTTP.Listen)
		if err != nil {
			return nil, configError{Field: "http.listen", Err: err}
		}
		proxies.HTTP = ep
	}
	if config.SOCKS5 != nil && config.SOCKS5.Listen != "" {
		ep, err := parseLocalProxyEndpoint(config.SOCKS5.Listen)
		if err != nil {
			return nil, configError{Field: "socks5.listen", Err: err}
		}
		proxies.SOCKS5 = ep
	}
	if !proxies.HasAny() {
		return nil, errors.New("system proxy and PAC require socks5 or http proxy to be enabled")
	}
	return proxies, nil
}

func parseLocalProxyEndpoint(listen string) (*localProxyEndpoint, error) {
	host, portStr, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return &localProxyEndpoint{Host: host, Port: port}, nil
}

func buildPACScript(proxies *localProxySet) string {
	chain := buildPACProxyChain(proxies)
	return fmt.Sprintf(`function FindProxyForURL(url, host) {
  if (host == "localhost" || shExpMatch(host, "127.*") || shExpMatch(host, "::1")) {
    return "DIRECT";
  }
  return "%s";
}
`, chain)
}

func buildPACProxyChain(proxies *localProxySet) string {
	parts := make([]string, 0, 3)
	if proxies != nil && proxies.HTTP != nil {
		parts = append(parts, "PROXY "+proxies.HTTP.Addr())
	}
	if proxies != nil && proxies.SOCKS5 != nil {
		parts = append(parts, "SOCKS5 "+proxies.SOCKS5.Addr())
	}
	parts = append(parts, "DIRECT")
	return strings.Join(parts, "; ")
}

type pacServer struct {
	listener net.Listener
	server   *stdhttp.Server
	url      string
}

func newPACServer(listen, script string) (*pacServer, error) {
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	mux := stdhttp.NewServeMux()
	mux.HandleFunc("/proxy.pac", func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
		w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
		_, _ = w.Write([]byte(script))
	})
	urlHost, err := pacURLHost(listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	return &pacServer{
		listener: listener,
		server:   &stdhttp.Server{Handler: mux},
		url:      "http://" + urlHost + "/proxy.pac",
	}, nil
}

func pacURLHost(listenAddr string) (string, error) {
	ep, err := parseLocalProxyEndpoint(listenAddr)
	if err != nil {
		return "", err
	}
	return ep.Addr(), nil
}

func (s *pacServer) URL() string {
	return s.url
}

func (s *pacServer) Run() error {
	logger.Info("PAC server listening", zap.String("url", s.url))
	err := s.server.Serve(s.listener)
	if errors.Is(err, stdhttp.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *pacServer) Close() error {
	return s.server.Close()
}
