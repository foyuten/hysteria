//go:build linux

package cmd

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

type gsettingsKey struct {
	schema string
	key    string
}

type gsettingsValue struct {
	gsettingsKey
	value string
}

var gsettingsProxyKeys = []gsettingsKey{
	{schema: "org.gnome.system.proxy", key: "mode"},
	{schema: "org.gnome.system.proxy", key: "autoconfig-url"},
	{schema: "org.gnome.system.proxy.http", key: "host"},
	{schema: "org.gnome.system.proxy.http", key: "port"},
	{schema: "org.gnome.system.proxy.https", key: "host"},
	{schema: "org.gnome.system.proxy.https", key: "port"},
	{schema: "org.gnome.system.proxy.socks", key: "host"},
	{schema: "org.gnome.system.proxy.socks", key: "port"},
}

func configureSystemProxy(proxies *localProxySet, pacURL string) (func() error, error) {
	if _, err := exec.LookPath("gsettings"); err != nil {
		return nil, errors.New("gsettings not found")
	}
	values, err := captureGSettingsValues(gsettingsProxyKeys)
	if err != nil {
		return nil, err
	}
	if pacURL != "" {
		err = applyGSettingsPAC(proxies, pacURL)
	} else {
		err = applyGSettingsManual(proxies)
	}
	if err != nil {
		return nil, err
	}
	return func() error {
		return restoreGSettingsValues(values)
	}, nil
}

func captureGSettingsValues(keys []gsettingsKey) ([]gsettingsValue, error) {
	values := make([]gsettingsValue, 0, len(keys))
	for _, key := range keys {
		value, err := gsettingsGet(key.schema, key.key)
		if err != nil {
			return nil, err
		}
		values = append(values, gsettingsValue{gsettingsKey: key, value: value})
	}
	return values, nil
}

func restoreGSettingsValues(values []gsettingsValue) error {
	var firstErr error
	for _, value := range values {
		if err := gsettingsSet(value.schema, value.key, value.value); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func applyGSettingsPAC(proxies *localProxySet, pacURL string) error {
	if proxies != nil && proxies.HasAny() {
		if err := applyGSettingsEndpoints(proxies); err != nil {
			return err
		}
	}
	if err := gsettingsSet("org.gnome.system.proxy", "autoconfig-url", quoteGSettingsString(pacURL)); err != nil {
		return err
	}
	return gsettingsSet("org.gnome.system.proxy", "mode", quoteGSettingsString("auto"))
}

func applyGSettingsManual(proxies *localProxySet) error {
	if proxies == nil || !proxies.HasAny() {
		return errors.New("no local proxy endpoints available")
	}
	if err := applyGSettingsEndpoints(proxies); err != nil {
		return err
	}
	if err := gsettingsSet("org.gnome.system.proxy", "autoconfig-url", quoteGSettingsString("")); err != nil {
		return err
	}
	return gsettingsSet("org.gnome.system.proxy", "mode", quoteGSettingsString("manual"))
}

func applyGSettingsEndpoints(proxies *localProxySet) error {
	if proxies.HTTP != nil {
		if err := gsettingsSet("org.gnome.system.proxy.http", "host", quoteGSettingsString(proxies.HTTP.Host)); err != nil {
			return err
		}
		if err := gsettingsSet("org.gnome.system.proxy.http", "port", fmt.Sprintf("%d", proxies.HTTP.Port)); err != nil {
			return err
		}
		if err := gsettingsSet("org.gnome.system.proxy.https", "host", quoteGSettingsString(proxies.HTTP.Host)); err != nil {
			return err
		}
		if err := gsettingsSet("org.gnome.system.proxy.https", "port", fmt.Sprintf("%d", proxies.HTTP.Port)); err != nil {
			return err
		}
	} else {
		if err := gsettingsSet("org.gnome.system.proxy.http", "host", quoteGSettingsString("")); err != nil {
			return err
		}
		if err := gsettingsSet("org.gnome.system.proxy.http", "port", "0"); err != nil {
			return err
		}
		if err := gsettingsSet("org.gnome.system.proxy.https", "host", quoteGSettingsString("")); err != nil {
			return err
		}
		if err := gsettingsSet("org.gnome.system.proxy.https", "port", "0"); err != nil {
			return err
		}
	}
	if proxies.SOCKS5 != nil {
		if err := gsettingsSet("org.gnome.system.proxy.socks", "host", quoteGSettingsString(proxies.SOCKS5.Host)); err != nil {
			return err
		}
		if err := gsettingsSet("org.gnome.system.proxy.socks", "port", fmt.Sprintf("%d", proxies.SOCKS5.Port)); err != nil {
			return err
		}
	} else {
		if err := gsettingsSet("org.gnome.system.proxy.socks", "host", quoteGSettingsString("")); err != nil {
			return err
		}
		if err := gsettingsSet("org.gnome.system.proxy.socks", "port", "0"); err != nil {
			return err
		}
	}
	return nil
}

func gsettingsGet(schema, key string) (string, error) {
	output, err := exec.Command("gsettings", "get", schema, key).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gsettings get %s %s failed: %w: %s", schema, key, err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func gsettingsSet(schema, key, value string) error {
	output, err := exec.Command("gsettings", "set", schema, key, value).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gsettings set %s %s failed: %w: %s", schema, key, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func quoteGSettingsString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "\\'") + "'"
}
