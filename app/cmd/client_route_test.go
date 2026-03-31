package cmd

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/apernet/hysteria/core/v2/client"
	"github.com/apernet/hysteria/extras/v2/outbounds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type recordingClient struct {
	mutex    sync.Mutex
	tcpCalls []string
	udpCalls int
}

func (c *recordingClient) TCP(addr string) (net.Conn, error) {
	c.mutex.Lock()
	c.tcpCalls = append(c.tcpCalls, addr)
	c.mutex.Unlock()
	left, right := net.Pipe()
	go right.Close()
	return left, nil
}

func (c *recordingClient) UDP() (client.HyUDPConn, error) {
	c.mutex.Lock()
	c.udpCalls++
	c.mutex.Unlock()
	return &recordingHyUDPConn{recvCh: make(chan routedUDPPacket, 8)}, nil
}

func (c *recordingClient) Close() error {
	return nil
}

func (c *recordingClient) TCPCalls() []string {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return append([]string(nil), c.tcpCalls...)
}

type recordingHyUDPConn struct {
	recvCh chan routedUDPPacket
}

func (c *recordingHyUDPConn) Receive() ([]byte, string, error) {
	pkt, ok := <-c.recvCh
	if !ok {
		return nil, "", io.EOF
	}
	return pkt.Data, pkt.Addr, nil
}

func (c *recordingHyUDPConn) Send(bs []byte, addr string) error {
	c.recvCh <- routedUDPPacket{Data: append([]byte(nil), bs...), Addr: addr}
	return nil
}

func (c *recordingHyUDPConn) Close() error {
	close(c.recvCh)
	return nil
}

type fakePluggableOutbound struct {
	mutex      sync.Mutex
	udpCreated map[string]int
	errHosts   map[string]error
}

func newFakePluggableOutbound() *fakePluggableOutbound {
	return &fakePluggableOutbound{
		udpCreated: make(map[string]int),
		errHosts:   make(map[string]error),
	}
}

func (o *fakePluggableOutbound) TCP(reqAddr *outbounds.AddrEx) (net.Conn, error) {
	left, right := net.Pipe()
	go right.Close()
	return left, nil
}

func (o *fakePluggableOutbound) UDP(reqAddr *outbounds.AddrEx) (outbounds.UDPConn, error) {
	o.mutex.Lock()
	defer o.mutex.Unlock()
	if err := o.errHosts[reqAddr.Host]; err != nil {
		return nil, err
	}
	o.udpCreated[reqAddr.String()]++
	return &fakeOutboundUDPConn{
		readCh: make(chan routedUDPPacket, 8),
	}, nil
}

func (o *fakePluggableOutbound) UDPCreateCount(addr string) int {
	o.mutex.Lock()
	defer o.mutex.Unlock()
	return o.udpCreated[addr]
}

type fakeOutboundUDPConn struct {
	readCh chan routedUDPPacket
	mutex  sync.Mutex
	closed bool
}

func (c *fakeOutboundUDPConn) ReadFrom(bs []byte) (int, *outbounds.AddrEx, error) {
	pkt, ok := <-c.readCh
	if !ok {
		return 0, nil, io.EOF
	}
	addr, err := parseAddrEx(pkt.Addr)
	if err != nil {
		return 0, nil, err
	}
	return copy(bs, pkt.Data), addr, nil
}

func (c *fakeOutboundUDPConn) WriteTo(bs []byte, addr *outbounds.AddrEx) (int, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.closed {
		return 0, io.EOF
	}
	c.readCh <- routedUDPPacket{Data: append([]byte(nil), bs...), Addr: addr.String()}
	return len(bs), nil
}

func (c *fakeOutboundUDPConn) Close() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if !c.closed {
		c.closed = true
		close(c.readCh)
	}
	return nil
}

func TestClientBuildModeClientRoutesTCP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()

	base := &recordingClient{}
	modeClient, err := (&clientConfig{
		ACL: clientConfigACL{Inline: []string{
			"direct(127.0.0.1)",
			"reject(blocked.example)",
		}},
	}).buildModeClient(base)
	require.NoError(t, err)

	proxyConn, err := modeClient.TCP("example.com:443")
	require.NoError(t, err)
	_ = proxyConn.Close()
	assert.Equal(t, []string{"example.com:443"}, base.TCPCalls())

	directConn, err := modeClient.TCP(listener.Addr().String())
	require.NoError(t, err)
	_ = directConn.Close()
	<-acceptDone
	assert.Equal(t, []string{"example.com:443"}, base.TCPCalls())

	rejectedConn, err := modeClient.TCP("blocked.example:80")
	assert.Error(t, err)
	assert.Nil(t, rejectedConn)
}

func TestBuildClientACLUsesProxyAsDefault(t *testing.T) {
	proxyDefault := newHelperTestOutbound()

	clientACL, hasACL, err := buildClientACL(clientConfigACL{Inline: []string{"reject(block.example)"}}, []outbounds.OutboundEntry{
		{Name: "proxy", Outbound: proxyDefault},
	}, nil)
	require.NoError(t, err)
	assert.True(t, hasACL)

	conn, err := clientACL.TCP(&outbounds.AddrEx{Host: "miss.example", Port: 8443})
	require.NoError(t, err)
	_ = conn.Close()
	assert.Equal(t, "miss.example:8443", <-proxyDefault.tcpCh)
}

func TestRoutedHyUDPConnReusesSingleProxySession(t *testing.T) {
	base := &recordingClient{}
	modeClient, err := (&clientConfig{
		ACL: clientConfigACL{Inline: []string{
			"reject(blocked.example)",
		}},
	}).buildModeClient(base)
	require.NoError(t, err)

	udpConn, err := modeClient.UDP()
	require.NoError(t, err)
	defer udpConn.Close()

	require.NoError(t, udpConn.Send([]byte("one"), "alpha.example:53"))
	require.NoError(t, udpConn.Send([]byte("two"), "beta.example:53"))

	assert.Equal(t, 1, base.udpCalls)

	packets := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		data, addr, err := udpConn.Receive()
		require.NoError(t, err)
		packets = append(packets, addr+"="+string(data))
	}
	sort.Strings(packets)
	assert.Equal(t, []string{
		"alpha.example:53=one",
		"beta.example:53=two",
	}, packets)
}

func TestClientACLDebugLogsMatchedStrategies(t *testing.T) {
	core, observed := observer.New(zap.DebugLevel)
	oldLogger := logger
	logger = zap.New(core)
	defer func() {
		logger = oldLogger
	}()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()

	base := &recordingClient{}
	modeClient, err := (&clientConfig{
		ACL: clientConfigACL{Inline: []string{
			"direct(127.0.0.1)",
			"reject(blocked.example)",
		}},
	}).buildModeClient(base)
	require.NoError(t, err)

	proxyConn, err := modeClient.TCP("miss.example:443")
	require.NoError(t, err)
	_ = proxyConn.Close()

	directConn, err := modeClient.TCP(listener.Addr().String())
	require.NoError(t, err)
	_ = directConn.Close()
	<-acceptDone

	_, err = modeClient.TCP("blocked.example:80")
	require.Error(t, err)

	entries := observed.FilterMessage("client ACL matched").All()
	require.Len(t, entries, 3)
	assert.Equal(t, "default", entries[0].ContextMap()["strategy"])
	assert.Equal(t, "miss.example:443", entries[0].ContextMap()["reqAddr"])
	assert.Equal(t, "direct", entries[1].ContextMap()["strategy"])
	assert.Equal(t, listener.Addr().String(), entries[1].ContextMap()["reqAddr"])
	assert.Equal(t, "reject", entries[2].ContextMap()["strategy"])
	assert.Equal(t, "blocked.example:80", entries[2].ContextMap()["reqAddr"])
}

func TestBuildClientACLFromFileUsesProxyAsDefault(t *testing.T) {
	tmpDir := t.TempDir()
	aclFile := tmpDir + "/client.acl"
	err := os.WriteFile(aclFile, []byte("reject(block.example)\n"), 0o600)
	require.NoError(t, err)

	proxyDefault := newHelperTestOutbound()
	clientACL, hasACL, err := buildClientACL(clientConfigACL{File: aclFile}, []outbounds.OutboundEntry{
		{Name: "proxy", Outbound: proxyDefault},
	}, nil)
	require.NoError(t, err)
	assert.True(t, hasACL)

	conn, err := clientACL.TCP(&outbounds.AddrEx{Host: "miss.example", Port: 9443})
	require.NoError(t, err)
	_ = conn.Close()
	assert.Equal(t, "miss.example:9443", <-proxyDefault.tcpCh)
}

func TestBuildClientACLRejectsFileAndInlineTogether(t *testing.T) {
	_, _, err := buildClientACL(clientConfigACL{File: "/tmp/unused.acl", Inline: []string{"reject(block.example)"}}, nil, nil)
	assert.EqualError(t, err, "invalid config: acl: cannot set both acl.file and acl.inline")
}

func TestNewClientGeoLoaderUsesProxyHTTPClient(t *testing.T) {
	base := &dialingClient{}
	loader, err := newClientGeoLoader(clientConfigACL{GeoDownloadProxy: true}, base)
	require.NoError(t, err)
	require.NotNil(t, loader.HTTPClient)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	resp, err := loader.HTTPClient.Get(server.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, []string{server.Listener.Addr().String()}, base.TCPCalls())
}

func TestRoutedHyUDPConnRoutesAndReusesChildren(t *testing.T) {
	ob := newFakePluggableOutbound()
	conn := newRoutedHyUDPConn(ob)
	defer conn.Close()

	require.NoError(t, conn.Send([]byte("one"), "alpha.example:53"))
	require.NoError(t, conn.Send([]byte("two"), "alpha.example:53"))
	require.NoError(t, conn.Send([]byte("three"), "beta.example:53"))

	assert.Equal(t, 1, ob.UDPCreateCount("alpha.example:53"))
	assert.Equal(t, 1, ob.UDPCreateCount("beta.example:53"))

	packets := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		data, addr, err := conn.Receive()
		require.NoError(t, err)
		packets = append(packets, addr+"="+string(data))
	}
	sort.Strings(packets)
	assert.Equal(t, []string{
		"alpha.example:53=one",
		"alpha.example:53=two",
		"beta.example:53=three",
	}, packets)
}

func TestRoutedHyUDPConnPropagatesOutboundErrors(t *testing.T) {
	ob := newFakePluggableOutbound()
	ob.errHosts["blocked.example"] = io.ErrClosedPipe

	conn := newRoutedHyUDPConn(ob)
	defer conn.Close()

	err := conn.Send([]byte("blocked"), "blocked.example:53")
	assert.ErrorIs(t, err, io.ErrClosedPipe)
}

func TestClientBuildModeClientWithoutRoutingReturnsBase(t *testing.T) {
	base := &recordingClient{}
	modeClient, err := (&clientConfig{}).buildModeClient(base)
	require.NoError(t, err)
	assert.Same(t, base, modeClient)
}

func TestRoutedHyUDPConnCloseReturnsEOF(t *testing.T) {
	conn := newRoutedHyUDPConn(newFakePluggableOutbound())
	require.NoError(t, conn.Close())

	_, _, err := conn.Receive()
	assert.ErrorIs(t, err, io.EOF)

	err = conn.Send([]byte("data"), "closed.example:53")
	assert.ErrorIs(t, err, io.EOF)
}

func TestRoutedHyUDPConnIdleCleanup(t *testing.T) {
	ob := newFakePluggableOutbound()
	conn := newRoutedHyUDPConn(ob)
	conn.IdleTimeout = 10 * time.Millisecond
	defer conn.Close()

	require.NoError(t, conn.Send([]byte("data"), "idle.example:53"))
	time.Sleep(30 * time.Millisecond)
	conn.cleanupIdleChildren()
	require.NoError(t, conn.Send([]byte("data"), "idle.example:53"))

	assert.Equal(t, 2, ob.UDPCreateCount("idle.example:53"))
}

type helperTestOutbound struct {
	tcpCh chan string
}

type dialingClient struct {
	mutex    sync.Mutex
	tcpCalls []string
}

func (c *dialingClient) TCP(addr string) (net.Conn, error) {
	c.mutex.Lock()
	c.tcpCalls = append(c.tcpCalls, addr)
	c.mutex.Unlock()
	return net.Dial("tcp", addr)
}

func (c *dialingClient) UDP() (client.HyUDPConn, error) {
	return nil, errors.New("unused")
}

func (c *dialingClient) Close() error {
	return nil
}

func (c *dialingClient) TCPCalls() []string {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return append([]string(nil), c.tcpCalls...)
}

func newHelperTestOutbound() *helperTestOutbound {
	return &helperTestOutbound{tcpCh: make(chan string, 1)}
}

func (o *helperTestOutbound) TCP(reqAddr *outbounds.AddrEx) (net.Conn, error) {
	o.tcpCh <- reqAddr.String()
	left, right := net.Pipe()
	go right.Close()
	return left, nil
}

func (o *helperTestOutbound) UDP(reqAddr *outbounds.AddrEx) (outbounds.UDPConn, error) {
	return nil, nil
}
