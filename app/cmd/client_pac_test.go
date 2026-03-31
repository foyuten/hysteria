package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLocalProxyEndpointNormalizesWildcardHost(t *testing.T) {
	ep, err := parseLocalProxyEndpoint("0.0.0.0:8080")
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", ep.Host)
	assert.Equal(t, 8080, ep.Port)
}

func TestBuildLocalProxySetUsesHTTPAndSOCKS5(t *testing.T) {
	proxies, err := buildLocalProxySet(clientConfig{
		HTTP:   &httpConfig{Listen: "127.0.0.1:8080"},
		SOCKS5: &socks5Config{Listen: "0.0.0.0:1080"},
	})
	require.NoError(t, err)
	require.NotNil(t, proxies.HTTP)
	require.NotNil(t, proxies.SOCKS5)
	assert.Equal(t, "127.0.0.1:8080", proxies.HTTP.Addr())
	assert.Equal(t, "127.0.0.1:1080", proxies.SOCKS5.Addr())
}

func TestBuildPACProxyChain(t *testing.T) {
	chain := buildPACProxyChain(&localProxySet{
		HTTP:   &localProxyEndpoint{Host: "127.0.0.1", Port: 8080},
		SOCKS5: &localProxyEndpoint{Host: "127.0.0.1", Port: 1080},
	})
	assert.Equal(t, "PROXY 127.0.0.1:8080; SOCKS5 127.0.0.1:1080; DIRECT", chain)
}

func TestPACURLHostNormalizesWildcardHost(t *testing.T) {
	host, err := pacURLHost("0.0.0.0:9090")
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:9090", host)
}
