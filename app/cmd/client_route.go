package cmd

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apernet/hysteria/app/v2/internal/utils"
	"github.com/apernet/hysteria/core/v2/client"
	"github.com/apernet/hysteria/extras/v2/outbounds"
	"go.uber.org/zap"
)

const (
	clientRouteUDPIdleTimeout   = 60 * time.Second
	clientRouteUDPCleanupPeriod = 5 * time.Second
)

var errClientRoutingDisabled = errors.New("client routing is not configured")

var errClientACLRejected = errors.New("rejected")

type clientACLRejectOutbound struct{}

func (o *clientACLRejectOutbound) TCP(_ *outbounds.AddrEx) (net.Conn, error) {
	return nil, errClientACLRejected
}

func (o *clientACLRejectOutbound) UDP(_ *outbounds.AddrEx) (outbounds.UDPConn, error) {
	return nil, errClientACLRejected
}

type clientACLDebugOutbound struct {
	Strategy string
	Outbound outbounds.PluggableOutbound
}

func (o *clientACLDebugOutbound) TCP(reqAddr *outbounds.AddrEx) (net.Conn, error) {
	conn, err := o.Outbound.TCP(reqAddr)
	logClientACLDecision("tcp", o.Strategy, reqAddr, err)
	return conn, err
}

func (o *clientACLDebugOutbound) UDP(reqAddr *outbounds.AddrEx) (outbounds.UDPConn, error) {
	conn, err := o.Outbound.UDP(reqAddr)
	logClientACLDecision("udp", o.Strategy, reqAddr, err)
	return conn, err
}

func logClientACLDecision(protocol, strategy string, reqAddr *outbounds.AddrEx, err error) {
	if logger == nil {
		return
	}
	fields := []zap.Field{
		zap.String("protocol", protocol),
		zap.String("strategy", strategy),
	}
	if reqAddr != nil {
		fields = append(fields, zap.String("reqAddr", reqAddr.String()))
	}
	if err != nil {
		logger.Debug("client ACL matched", append(fields, zap.Error(err))...)
		return
	}
	logger.Debug("client ACL matched", fields...)
}

type clientProxyOutbound struct {
	Client client.Client

	mutex   sync.Mutex
	udpConn *clientProxySharedUDPConn
}

func (o *clientProxyOutbound) TCP(reqAddr *outbounds.AddrEx) (net.Conn, error) {
	return o.Client.TCP(reqAddr.String())
}

func (o *clientProxyOutbound) UDP(_ *outbounds.AddrEx) (outbounds.UDPConn, error) {
	o.mutex.Lock()
	defer o.mutex.Unlock()
	if o.udpConn != nil {
		return o.udpConn, nil
	}
	hyConn, err := o.Client.UDP()
	if err != nil {
		return nil, err
	}
	o.udpConn = &clientProxySharedUDPConn{
		parent:    o,
		HyUDPConn: hyConn,
	}
	return o.udpConn, nil
}

type sharedUDPConn interface {
	outbounds.UDPConn
	SharedKey() string
}

const clientProxySharedUDPKey = "client-proxy"

type clientProxySharedUDPConn struct {
	client.HyUDPConn
	parent *clientProxyOutbound

	writeMutex sync.Mutex
	closeOnce  sync.Once
}

func (c *clientProxySharedUDPConn) SharedKey() string {
	return clientProxySharedUDPKey
}

func (c *clientProxySharedUDPConn) ReadFrom(b []byte) (int, *outbounds.AddrEx, error) {
	bs, addr, err := c.HyUDPConn.Receive()
	if err != nil {
		c.parent.clearUDPConn(c)
		return 0, nil, err
	}
	reqAddr, err := parseAddrEx(addr)
	if err != nil {
		return 0, nil, err
	}
	return copy(b, bs), reqAddr, nil
}

func (c *clientProxySharedUDPConn) WriteTo(b []byte, addr *outbounds.AddrEx) (int, error) {
	c.writeMutex.Lock()
	defer c.writeMutex.Unlock()
	if err := c.HyUDPConn.Send(b, addr.String()); err != nil {
		c.parent.clearUDPConn(c)
		return 0, err
	}
	return len(b), nil
}

func (c *clientProxySharedUDPConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.parent.clearUDPConn(c)
		err = c.HyUDPConn.Close()
	})
	return err
}

func (o *clientProxyOutbound) clearUDPConn(conn *clientProxySharedUDPConn) {
	o.mutex.Lock()
	defer o.mutex.Unlock()
	if o.udpConn == conn {
		o.udpConn = nil
	}
}

type routedClient struct {
	Base     client.Client
	Outbound outbounds.PluggableOutbound
}

func (c *routedClient) TCP(addr string) (net.Conn, error) {
	reqAddr, err := parseAddrEx(addr)
	if err != nil {
		return nil, err
	}
	return c.Outbound.TCP(reqAddr)
}

func (c *routedClient) UDP() (client.HyUDPConn, error) {
	return newRoutedHyUDPConn(c.Outbound), nil
}

func (c *routedClient) Close() error {
	return c.Base.Close()
}

type routedUDPPacket struct {
	Data []byte
	Addr string
}

type routedUDPChild struct {
	SessionKey string
	Conn       outbounds.UDPConn
	Last       atomic.Int64
}

func newRoutedUDPChild(sessionKey string, conn outbounds.UDPConn) *routedUDPChild {
	child := &routedUDPChild{
		SessionKey: sessionKey,
		Conn:       conn,
	}
	child.Touch()
	return child
}

func (c *routedUDPChild) Touch() {
	c.Last.Store(time.Now().UnixNano())
}

func (c *routedUDPChild) LastUsed() time.Time {
	return time.Unix(0, c.Last.Load())
}

type routedHyUDPConn struct {
	Outbound    outbounds.PluggableOutbound
	IdleTimeout time.Duration

	mutex     sync.Mutex
	children  map[string]*routedUDPChild
	routes    map[string]string
	recvCh    chan routedUDPPacket
	closeCh   chan struct{}
	closeOnce sync.Once
}

func newRoutedHyUDPConn(ob outbounds.PluggableOutbound) *routedHyUDPConn {
	c := &routedHyUDPConn{
		Outbound:    ob,
		IdleTimeout: clientRouteUDPIdleTimeout,
		children:    make(map[string]*routedUDPChild),
		routes:      make(map[string]string),
		recvCh:      make(chan routedUDPPacket, 32),
		closeCh:     make(chan struct{}),
	}
	go c.cleanupLoop()
	return c
}

func (c *routedHyUDPConn) Receive() ([]byte, string, error) {
	for {
		select {
		case pkt := <-c.recvCh:
			return pkt.Data, pkt.Addr, nil
		case <-c.closeCh:
			select {
			case pkt := <-c.recvCh:
				return pkt.Data, pkt.Addr, nil
			default:
				return nil, "", io.EOF
			}
		}
	}
}

func (c *routedHyUDPConn) Send(bs []byte, addr string) error {
	select {
	case <-c.closeCh:
		return io.EOF
	default:
	}
	reqAddr, err := parseAddrEx(addr)
	if err != nil {
		return err
	}
	key := reqAddr.String()
	child, err := c.getOrCreateChild(key, reqAddr)
	if err != nil {
		return err
	}
	child.Touch()
	_, err = child.Conn.WriteTo(bs, cloneAddrEx(reqAddr))
	if err != nil {
		c.removeChildBySession(child.SessionKey, child, true)
		return err
	}
	return nil
}

func (c *routedHyUDPConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closeCh)
		c.mutex.Lock()
		children := c.children
		c.children = make(map[string]*routedUDPChild)
		c.routes = make(map[string]string)
		c.mutex.Unlock()
		for _, child := range children {
			_ = child.Conn.Close()
		}
	})
	return nil
}

func (c *routedHyUDPConn) getOrCreateChild(key string, reqAddr *outbounds.AddrEx) (*routedUDPChild, error) {
	c.mutex.Lock()
	if sessionKey, ok := c.routes[key]; ok {
		if child := c.children[sessionKey]; child != nil {
			c.mutex.Unlock()
			return child, nil
		}
	}
	c.mutex.Unlock()

	actualAddr := cloneAddrEx(reqAddr)
	conn, err := c.Outbound.UDP(actualAddr)
	if err != nil {
		return nil, err
	}
	sessionKey := key
	if sharedConn, ok := conn.(sharedUDPConn); ok {
		sessionKey = sharedConn.SharedKey()
	}

	c.mutex.Lock()
	defer c.mutex.Unlock()
	if child := c.children[sessionKey]; child != nil {
		c.routes[key] = sessionKey
		if conn != child.Conn {
			_ = conn.Close()
		}
		return child, nil
	}
	child := newRoutedUDPChild(sessionKey, conn)
	c.children[sessionKey] = child
	c.routes[key] = sessionKey
	go c.readLoop(sessionKey, child)
	return child, nil
}

func (c *routedHyUDPConn) readLoop(key string, child *routedUDPChild) {
	buf := make([]byte, 64*1024)
	for {
		n, addr, err := child.Conn.ReadFrom(buf)
		if err != nil {
			c.removeChildBySession(key, child, true)
			return
		}
		child.Touch()
		if addr == nil {
			continue
		}
		pkt := routedUDPPacket{
			Data: append([]byte(nil), buf[:n]...),
			Addr: addr.String(),
		}
		select {
		case c.recvCh <- pkt:
		case <-c.closeCh:
			c.removeChildBySession(key, child, true)
			return
		}
	}
}

func (c *routedHyUDPConn) removeChildBySession(sessionKey string, child *routedUDPChild, closeConn bool) {
	c.mutex.Lock()
	if current := c.children[sessionKey]; current == child {
		delete(c.children, sessionKey)
		for routeKey, key := range c.routes {
			if key == sessionKey {
				delete(c.routes, routeKey)
			}
		}
	}
	c.mutex.Unlock()
	if closeConn {
		_ = child.Conn.Close()
	}
}

func (c *routedHyUDPConn) cleanupLoop() {
	ticker := time.NewTicker(clientRouteUDPCleanupPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.cleanupIdleChildren()
		case <-c.closeCh:
			return
		}
	}
}

func (c *routedHyUDPConn) cleanupIdleChildren() {
	now := time.Now()
	var stale []*routedUDPChild
	c.mutex.Lock()
	for sessionKey, child := range c.children {
		if now.Sub(child.LastUsed()) > c.IdleTimeout {
			stale = append(stale, child)
			delete(c.children, sessionKey)
			for routeKey, key := range c.routes {
				if key == sessionKey {
					delete(c.routes, routeKey)
				}
			}
		}
	}
	c.mutex.Unlock()
	for _, child := range stale {
		_ = child.Conn.Close()
	}
}

func (c *clientConfig) buildModeClient(base client.Client) (client.Client, error) {
	if !c.hasClientRoutingConfig() {
		return base, nil
	}
	ob, err := c.buildClientOutbound(base)
	if err != nil {
		return nil, err
	}
	return &routedClient{Base: base, Outbound: ob}, nil
}

func (c *clientConfig) hasClientRoutingConfig() bool {
	return c.ACL.File != "" || len(c.ACL.Inline) > 0
}

func (c *clientConfig) buildClientOutbound(base client.Client) (outbounds.PluggableOutbound, error) {
	if !c.hasClientRoutingConfig() {
		return nil, errClientRoutingDisabled
	}
	proxyOb := &clientProxyOutbound{Client: base}
	proxyDebugOb := &clientACLDebugOutbound{Strategy: "proxy", Outbound: proxyOb}
	directDebugOb := &clientACLDebugOutbound{Strategy: "direct", Outbound: outbounds.NewDirectOutboundSimple(outbounds.DirectOutboundModeAuto)}
	rejectDebugOb := &clientACLDebugOutbound{Strategy: "reject", Outbound: &clientACLRejectOutbound{}}
	defaultDebugOb := &clientACLDebugOutbound{Strategy: "default", Outbound: proxyOb}

	var unified outbounds.PluggableOutbound
	if c.ACL.File != "" || len(c.ACL.Inline) > 0 {
		obs := []outbounds.OutboundEntry{
			{Name: "proxy", Outbound: proxyDebugOb},
			{Name: "direct", Outbound: directDebugOb},
			{Name: "reject", Outbound: rejectDebugOb},
			{Name: "default", Outbound: defaultDebugOb},
		}
		var hasACL bool
		var err error
		unified, hasACL, err = buildClientACL(c.ACL, obs, base)
		if err != nil {
			return nil, err
		}
		return wrapPluggableResolver(c.Resolver, unified, hasACL)
	} else {
		unified = proxyOb
	}

	return wrapPluggableResolver(c.Resolver, unified, false)
}

func buildClientACL(cfg clientConfigACL, obs []outbounds.OutboundEntry, base client.Client) (outbounds.PluggableOutbound, bool, error) {
	if cfg.File != "" && len(cfg.Inline) > 0 {
		return nil, false, configError{Field: "acl", Err: errors.New("cannot set both acl.file and acl.inline")}
	}
	if cfg.File == "" && len(cfg.Inline) == 0 {
		if len(obs) == 0 {
			return nil, false, nil
		}
		return obs[0].Outbound, false, nil
	}
	gLoader, err := newClientGeoLoader(cfg, base)
	if err != nil {
		return nil, false, err
	}
	if cfg.File != "" {
		ruleBytes, err := os.ReadFile(cfg.File)
		if err != nil {
			return nil, true, configError{Field: "acl.file", Err: err}
		}
		if err := gLoader.Preload(utils.GeoDependenciesFromRuleText(string(ruleBytes))); err != nil {
			return nil, true, configError{Field: "acl.file", Err: err}
		}
		acl, err := outbounds.NewACLEngineFromString(string(ruleBytes), obs, gLoader)
		if err != nil {
			return nil, true, configError{Field: "acl.file", Err: err}
		}
		return acl, true, nil
	}
	ruleText := strings.Join(cfg.Inline, "\n")
	if err := gLoader.Preload(utils.GeoDependenciesFromRuleText(ruleText)); err != nil {
		return nil, true, configError{Field: "acl.inline", Err: err}
	}
	acl, err := outbounds.NewACLEngineFromString(ruleText, obs, gLoader)
	if err != nil {
		return nil, true, configError{Field: "acl.inline", Err: err}
	}
	return acl, true, nil
}

func newClientGeoLoader(cfg clientConfigACL, base client.Client) (*utils.GeoLoader, error) {
	gLoader := &utils.GeoLoader{
		GeoIPFilename:   cfg.GeoIP,
		GeoSiteFilename: cfg.GeoSite,
		UpdateInterval:  cfg.GeoUpdateInterval,
		DownloadFunc:    geoDownloadFunc,
		DownloadErrFunc: geoDownloadErrFunc,
	}
	if cfg.GeoDownloadProxy {
		if base == nil {
			return nil, configError{Field: "acl.geoDownloadProxy", Err: errors.New("requires a client connection")}
		}
		gLoader.HTTPClient = newGeoDownloadHTTPClient(base)
	}
	return gLoader, nil
}

func newGeoDownloadHTTPClient(base client.Client) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return base.TCP(addr)
	}
	return &http.Client{Transport: transport}
}

func wrapPluggableResolver(cfg serverConfigResolver, ob outbounds.PluggableOutbound, hasACL bool) (outbounds.PluggableOutbound, error) {
	switch strings.ToLower(cfg.Type) {
	case "", "system":
		if hasACL {
			ob = outbounds.NewSystemResolver(ob)
		}
	case "tcp":
		if cfg.TCP.Addr == "" {
			return nil, configError{Field: "resolver.tcp.addr", Err: errors.New("empty resolver address")}
		}
		ob = outbounds.NewStandardResolverTCP(cfg.TCP.Addr, cfg.TCP.Timeout, ob)
	case "udp":
		if cfg.UDP.Addr == "" {
			return nil, configError{Field: "resolver.udp.addr", Err: errors.New("empty resolver address")}
		}
		ob = outbounds.NewStandardResolverUDP(cfg.UDP.Addr, cfg.UDP.Timeout, ob)
	case "tls", "tcp-tls":
		if cfg.TLS.Addr == "" {
			return nil, configError{Field: "resolver.tls.addr", Err: errors.New("empty resolver address")}
		}
		ob = outbounds.NewStandardResolverTLS(cfg.TLS.Addr, cfg.TLS.Timeout, cfg.TLS.SNI, cfg.TLS.Insecure, ob)
	case "https", "http":
		if cfg.HTTPS.Addr == "" {
			return nil, configError{Field: "resolver.https.addr", Err: errors.New("empty resolver address")}
		}
		ob = outbounds.NewDoHResolver(cfg.HTTPS.Addr, cfg.HTTPS.Timeout, cfg.HTTPS.SNI, cfg.HTTPS.Insecure, ob)
	default:
		return nil, configError{Field: "resolver.type", Err: errors.New("unsupported resolver type")}
	}
	return ob, nil
}

func parseAddrEx(addr string) (*outbounds.AddrEx, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	portInt, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}
	return &outbounds.AddrEx{Host: host, Port: uint16(portInt)}, nil
}

func cloneAddrEx(addr *outbounds.AddrEx) *outbounds.AddrEx {
	if addr == nil {
		return nil
	}
	clone := &outbounds.AddrEx{
		Host: addr.Host,
		Port: addr.Port,
	}
	if addr.ResolveInfo != nil {
		clone.ResolveInfo = &outbounds.ResolveInfo{
			Err: addr.ResolveInfo.Err,
		}
		if addr.ResolveInfo.IPv4 != nil {
			clone.ResolveInfo.IPv4 = append(net.IP(nil), addr.ResolveInfo.IPv4...)
		}
		if addr.ResolveInfo.IPv6 != nil {
			clone.ResolveInfo.IPv6 = append(net.IP(nil), addr.ResolveInfo.IPv6...)
		}
	}
	return clone
}
