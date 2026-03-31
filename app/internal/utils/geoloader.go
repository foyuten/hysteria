package utils

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/apernet/hysteria/extras/v2/outbounds/acl"
	"github.com/apernet/hysteria/extras/v2/outbounds/acl/v2geo"
)

const (
	geoipFilename   = "geoip.dat"
	geoipURL        = "https://cdn.jsdelivr.net/gh/Loyalsoldier/v2ray-rules-dat@release/geoip.dat"
	geositeFilename = "geosite.dat"
	geositeURL      = "https://cdn.jsdelivr.net/gh/Loyalsoldier/v2ray-rules-dat@release/geosite.dat"
	geoDlTmpPattern = ".hysteria-geoloader.dlpart.*"

	geoDefaultUpdateInterval = 7 * 24 * time.Hour // 7 days
)

var _ acl.GeoLoader = (*GeoLoader)(nil)

// GeoLoader provides the on-demand GeoIP/GeoSite database
// loading functionality required by the ACL engine.
// Empty filenames = automatic download from built-in URLs.
type GeoLoader struct {
	GeoIPFilename   string
	GeoSiteFilename string
	UpdateInterval  time.Duration
	HTTPClient      *http.Client

	DownloadFunc    func(filename, url string)
	DownloadErrFunc func(err error)

	geoipMap   map[string]*v2geo.GeoIP
	geositeMap map[string]*v2geo.GeoSite
}

func (l *GeoLoader) preloadFromRuleText(ruleText string) error {
	needsGeoIP, needsGeoSite := GeoDependenciesFromRuleText(ruleText)
	return l.Preload(needsGeoIP, needsGeoSite)
}

func (l *GeoLoader) Preload(needsGeoIP, needsGeoSite bool) error {
	if needsGeoIP {
		if _, err := l.LoadGeoIP(); err != nil {
			return err
		}
	}
	if needsGeoSite {
		if _, err := l.LoadGeoSite(); err != nil {
			return err
		}
	}
	return nil
}

func GeoDependenciesFromRuleText(ruleText string) (needsGeoIP, needsGeoSite bool) {
	text := strings.ToLower(ruleText)
	return strings.Contains(text, "geoip:"), strings.Contains(text, "geosite:")
}

func (l *GeoLoader) notifyDownload(filename, url string) {
	if l.DownloadFunc != nil {
		l.DownloadFunc(filename, url)
	}
}

func (l *GeoLoader) notifyDownloadError(err error) {
	if l.DownloadErrFunc != nil {
		l.DownloadErrFunc(err)
	}
}

func (l *GeoLoader) shouldDownload(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return true
	}
	if info.Size() == 0 {
		// empty files are loadable by v2geo, but we consider it broken
		return true
	}
	dt := time.Since(info.ModTime())
	if l.UpdateInterval == 0 {
		return dt > geoDefaultUpdateInterval
	} else {
		return dt > l.UpdateInterval
	}
}

func (l *GeoLoader) downloadAndCheck(filename, url string, checkFunc func(filename string) error) error {
	l.notifyDownload(filename, url)

	hc := l.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Get(url)
	if err != nil {
		l.notifyDownloadError(err)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err = fmt.Errorf("unexpected HTTP status: %s", resp.Status)
		l.notifyDownloadError(err)
		return err
	}

	f, err := os.CreateTemp(".", geoDlTmpPattern)
	if err != nil {
		l.notifyDownloadError(err)
		return err
	}
	defer os.Remove(f.Name())

	_, err = io.Copy(f, resp.Body)
	if err != nil {
		f.Close()
		l.notifyDownloadError(err)
		return err
	}
	f.Close()

	err = checkFunc(f.Name())
	if err != nil {
		l.notifyDownloadError(fmt.Errorf("integrity check failed: %w", err))
		return err
	}

	err = os.Rename(f.Name(), filename)
	if err != nil {
		l.notifyDownloadError(fmt.Errorf("rename failed: %w", err))
		return err
	}

	return nil
}

func (l *GeoLoader) LoadGeoIP() (map[string]*v2geo.GeoIP, error) {
	if l.geoipMap != nil {
		return l.geoipMap, nil
	}
	autoDL := false
	filename := l.GeoIPFilename
	if filename == "" {
		autoDL = true
		filename = geoipFilename
	}
	if autoDL {
		if !l.shouldDownload(filename) {
			m, err := v2geo.LoadGeoIP(filename)
			if err == nil {
				l.geoipMap = m
				return m, nil
			}
			// file is broken, download it again
		}
		err := l.downloadAndCheck(filename, geoipURL, func(filename string) error {
			_, err := v2geo.LoadGeoIP(filename)
			return err
		})
		if err != nil {
			// as long as the previous download exists, fallback to it
			if _, serr := os.Stat(filename); os.IsNotExist(serr) {
				return nil, err
			}
		}
	}
	m, err := v2geo.LoadGeoIP(filename)
	if err != nil {
		return nil, err
	}
	l.geoipMap = m
	return m, nil
}

func (l *GeoLoader) LoadGeoSite() (map[string]*v2geo.GeoSite, error) {
	if l.geositeMap != nil {
		return l.geositeMap, nil
	}
	autoDL := false
	filename := l.GeoSiteFilename
	if filename == "" {
		autoDL = true
		filename = geositeFilename
	}
	if autoDL {
		if !l.shouldDownload(filename) {
			m, err := v2geo.LoadGeoSite(filename)
			if err == nil {
				l.geositeMap = m
				return m, nil
			}
			// file is broken, download it again
		}
		err := l.downloadAndCheck(filename, geositeURL, func(filename string) error {
			_, err := v2geo.LoadGeoSite(filename)
			return err
		})
		if err != nil {
			// as long as the previous download exists, fallback to it
			if _, serr := os.Stat(filename); os.IsNotExist(serr) {
				return nil, err
			}
		}
	}
	m, err := v2geo.LoadGeoSite(filename)
	if err != nil {
		return nil, err
	}
	l.geositeMap = m
	return m, nil
}
