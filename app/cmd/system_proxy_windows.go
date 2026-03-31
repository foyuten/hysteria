//go:build windows

package cmd

import (
	"errors"
	"fmt"
	"strings"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

const (
	winInternetSettingsPath       = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	internetOptionSettingsChanged = 39
	internetOptionRefresh         = 37
)

type windowsProxyState struct {
	proxyEnableValue uint64
	proxyEnableSet   bool
	proxyServerValue string
	proxyServerSet   bool
	autoConfigValue  string
	autoConfigSet    bool
}

func configureSystemProxy(proxies *localProxySet, pacURL string) (func() error, error) {
	state, err := captureWindowsProxyState()
	if err != nil {
		return nil, err
	}
	if pacURL != "" {
		err = applyWindowsPAC(pacURL)
	} else {
		err = applyWindowsManual(proxies)
	}
	if err != nil {
		return nil, err
	}
	return func() error {
		if err := restoreWindowsProxyState(state); err != nil {
			return err
		}
		return notifyWindowsProxyChanged()
	}, nil
}

func captureWindowsProxyState() (*windowsProxyState, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, winInternetSettingsPath, registry.QUERY_VALUE)
	if err != nil {
		return nil, err
	}
	defer key.Close()
	state := &windowsProxyState{}
	if v, _, err := key.GetIntegerValue("ProxyEnable"); err == nil {
		state.proxyEnableValue = v
		state.proxyEnableSet = true
	}
	if v, _, err := key.GetStringValue("ProxyServer"); err == nil {
		state.proxyServerValue = v
		state.proxyServerSet = true
	}
	if v, _, err := key.GetStringValue("AutoConfigURL"); err == nil {
		state.autoConfigValue = v
		state.autoConfigSet = true
	}
	return state, nil
}

func restoreWindowsProxyState(state *windowsProxyState) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, winInternetSettingsPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if state.proxyEnableSet {
		if err := key.SetDWordValue("ProxyEnable", uint32(state.proxyEnableValue)); err != nil {
			return err
		}
	} else {
		_ = key.DeleteValue("ProxyEnable")
	}
	if state.proxyServerSet {
		if err := key.SetStringValue("ProxyServer", state.proxyServerValue); err != nil {
			return err
		}
	} else {
		_ = key.DeleteValue("ProxyServer")
	}
	if state.autoConfigSet {
		if err := key.SetStringValue("AutoConfigURL", state.autoConfigValue); err != nil {
			return err
		}
	} else {
		_ = key.DeleteValue("AutoConfigURL")
	}
	return nil
}

func applyWindowsPAC(pacURL string) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, winInternetSettingsPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if err := key.SetDWordValue("ProxyEnable", 0); err != nil {
		return err
	}
	if err := key.SetStringValue("AutoConfigURL", pacURL); err != nil {
		return err
	}
	if err := key.SetStringValue("ProxyServer", ""); err != nil {
		return err
	}
	return notifyWindowsProxyChanged()
}

func applyWindowsManual(proxies *localProxySet) error {
	if proxies == nil || !proxies.HasAny() {
		return errors.New("no local proxy endpoints available")
	}
	parts := make([]string, 0, 3)
	if proxies.HTTP != nil {
		parts = append(parts, fmt.Sprintf("http=%s", proxies.HTTP.Addr()))
		parts = append(parts, fmt.Sprintf("https=%s", proxies.HTTP.Addr()))
	}
	if proxies.SOCKS5 != nil {
		parts = append(parts, fmt.Sprintf("socks=%s", proxies.SOCKS5.Addr()))
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, winInternetSettingsPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if err := key.SetDWordValue("ProxyEnable", 1); err != nil {
		return err
	}
	if err := key.SetStringValue("AutoConfigURL", ""); err != nil {
		return err
	}
	if err := key.SetStringValue("ProxyServer", strings.Join(parts, ";")); err != nil {
		return err
	}
	return notifyWindowsProxyChanged()
}

func notifyWindowsProxyChanged() error {
	wininet := syscall.NewLazyDLL("wininet.dll")
	proc := wininet.NewProc("InternetSetOptionW")
	for _, option := range []uintptr{internetOptionSettingsChanged, internetOptionRefresh} {
		r1, _, err := proc.Call(0, option, 0, 0)
		if r1 == 0 {
			return err
		}
	}
	return nil
}
