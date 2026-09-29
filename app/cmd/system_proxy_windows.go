//go:build windows

package cmd

import (
	"errors"
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
		err = applyWindowsPAC(proxies, pacURL)
	} else {
		err = applyWindowsManual(proxies)
	}
	if err != nil {
		rollbackErr := restoreWindowsProxyState(state)
		notifyErr := notifyWindowsProxyChanged()
		return nil, errors.Join(err, rollbackErr, notifyErr)
	}
	return func() error {
		return errors.Join(restoreWindowsProxyState(state), notifyWindowsProxyChanged())
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
	var restoreErr error
	if state.proxyEnableSet {
		restoreErr = errors.Join(restoreErr, key.SetDWordValue("ProxyEnable", uint32(state.proxyEnableValue)))
	} else {
		_ = key.DeleteValue("ProxyEnable")
	}
	if state.proxyServerSet {
		restoreErr = errors.Join(restoreErr, key.SetStringValue("ProxyServer", state.proxyServerValue))
	} else {
		_ = key.DeleteValue("ProxyServer")
	}
	if state.autoConfigSet {
		restoreErr = errors.Join(restoreErr, key.SetStringValue("AutoConfigURL", state.autoConfigValue))
	} else {
		_ = key.DeleteValue("AutoConfigURL")
	}
	return restoreErr
}

func applyWindowsPAC(proxies *localProxySet, pacURL string) error {
	proxyServer, err := buildWindowsProxyServer(proxies)
	if err != nil {
		return err
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, winInternetSettingsPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if err := key.SetDWordValue("ProxyEnable", 1); err != nil {
		return err
	}
	if err := key.SetStringValue("AutoConfigURL", pacURL); err != nil {
		return err
	}
	if err := key.SetStringValue("ProxyServer", proxyServer); err != nil {
		return err
	}
	return notifyWindowsProxyChanged()
}

func applyWindowsManual(proxies *localProxySet) error {
	proxyServer, err := buildWindowsProxyServer(proxies)
	if err != nil {
		return err
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
	if err := key.SetStringValue("ProxyServer", proxyServer); err != nil {
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
