package utils

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGeoDependenciesFromRuleText(t *testing.T) {
	needsGeoIP, needsGeoSite := GeoDependenciesFromRuleText("direct(geoip:cn)\nproxy(geosite:cn)\nreject(all)")
	assert.True(t, needsGeoIP)
	assert.True(t, needsGeoSite)

	needsGeoIP, needsGeoSite = GeoDependenciesFromRuleText("direct(example.com)\nproxy(all)")
	assert.False(t, needsGeoIP)
	assert.False(t, needsGeoSite)
}

func TestGeoLoaderPreloadLoadsRequestedDatabases(t *testing.T) {
	loader := &GeoLoader{
		GeoIPFilename:   filepath.Join("..", "..", "..", "extras", "outbounds", "acl", "v2geo", "geoip.dat"),
		GeoSiteFilename: filepath.Join("..", "..", "..", "extras", "outbounds", "acl", "v2geo", "geosite.dat"),
	}

	err := loader.Preload(true, true)
	require.NoError(t, err)
	assert.NotNil(t, loader.geoipMap)
	assert.NotNil(t, loader.geositeMap)
}
