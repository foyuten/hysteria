//go:build darwin

package cmd

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

type macOSProxyState struct {
	services []macOSNetworkServiceState
}

type macOSNetworkServiceState struct {
	name   string
	web    macOSProxyEndpointState
	secure macOSProxyEndpointState
	socks  macOSProxyEndpointState
	auto   macOSAutoProxyState
}

type macOSProxyEndpointState struct {
	enabled bool
	server  string
	port    int
}

type macOSAutoProxyState struct {
	enabled bool
	url     string
}

func configureSystemProxy(proxies *localProxySet, pacURL string) (func() error, error) {
	if _, err := exec.LookPath("networksetup"); err != nil {
		return nil, errors.New("networksetup not found")
	}
	services, err := listMacOSNetworkServices()
	if err != nil {
		return nil, err
	}
	state, err := captureMacOSProxyState(services)
	if err != nil {
		return nil, err
	}
	if pacURL != "" {
		err = applyMacOSPAC(services, proxies, pacURL)
	} else {
		err = applyMacOSManual(services, proxies)
	}
	if err != nil {
		return nil, err
	}
	return func() error {
		return restoreMacOSProxyState(state)
	}, nil
}

func listMacOSNetworkServices() ([]string, error) {
	output, err := networksetupOutput("-listallnetworkservices")
	if err != nil {
		return nil, err
	}
	services := make([]string, 0)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "An asterisk") || strings.HasPrefix(line, "*") {
			continue
		}
		services = append(services, line)
	}
	if len(services) == 0 {
		return nil, errors.New("no macOS network services found")
	}
	return services, nil
}

func captureMacOSProxyState(services []string) (*macOSProxyState, error) {
	state := &macOSProxyState{services: make([]macOSNetworkServiceState, 0, len(services))}
	for _, service := range services {
		serviceState, err := captureMacOSNetworkServiceState(service)
		if err != nil {
			return nil, err
		}
		state.services = append(state.services, serviceState)
	}
	return state, nil
}

func captureMacOSNetworkServiceState(service string) (macOSNetworkServiceState, error) {
	web, err := readMacOSProxyEndpointState(service, "-getwebproxy")
	if err != nil {
		return macOSNetworkServiceState{}, err
	}
	secure, err := readMacOSProxyEndpointState(service, "-getsecurewebproxy")
	if err != nil {
		return macOSNetworkServiceState{}, err
	}
	socks, err := readMacOSProxyEndpointState(service, "-getsocksfirewallproxy")
	if err != nil {
		return macOSNetworkServiceState{}, err
	}
	auto, err := readMacOSAutoProxyState(service)
	if err != nil {
		return macOSNetworkServiceState{}, err
	}
	return macOSNetworkServiceState{
		name:   service,
		web:    web,
		secure: secure,
		socks:  socks,
		auto:   auto,
	}, nil
}

func restoreMacOSProxyState(state *macOSProxyState) error {
	var firstErr error
	for _, service := range state.services {
		recordMacOSFirstError(&firstErr, restoreMacOSNetworkServiceState(service))
	}
	return firstErr
}

func restoreMacOSNetworkServiceState(service macOSNetworkServiceState) error {
	var firstErr error
	recordMacOSFirstError(&firstErr, restoreMacOSProxyEndpoint(service.name, "-setwebproxy", "-setwebproxystate", service.web))
	recordMacOSFirstError(&firstErr, restoreMacOSProxyEndpoint(service.name, "-setsecurewebproxy", "-setsecurewebproxystate", service.secure))
	recordMacOSFirstError(&firstErr, restoreMacOSProxyEndpoint(service.name, "-setsocksfirewallproxy", "-setsocksfirewallproxystate", service.socks))
	recordMacOSFirstError(&firstErr, restoreMacOSAutoProxy(service.name, service.auto))
	return firstErr
}

func applyMacOSPAC(services []string, proxies *localProxySet, pacURL string) error {
	if proxies == nil || !proxies.HasAny() {
		return errors.New("no local proxy endpoints available")
	}
	if err := applyMacOSEndpoints(services, proxies); err != nil {
		return err
	}
	var firstErr error
	for _, service := range services {
		recordMacOSFirstError(&firstErr, networksetupRun("-setautoproxyurl", service, pacURL))
		recordMacOSFirstError(&firstErr, networksetupRun("-setautoproxystate", service, "on"))
	}
	return firstErr
}

func applyMacOSManual(services []string, proxies *localProxySet) error {
	if proxies == nil || !proxies.HasAny() {
		return errors.New("no local proxy endpoints available")
	}
	if err := applyMacOSEndpoints(services, proxies); err != nil {
		return err
	}
	var firstErr error
	for _, service := range services {
		recordMacOSFirstError(&firstErr, networksetupRun("-setautoproxystate", service, "off"))
	}
	return firstErr
}

func applyMacOSEndpoints(services []string, proxies *localProxySet) error {
	var firstErr error
	for _, service := range services {
		recordMacOSFirstError(&firstErr, setMacOSProxyEndpoint(service, "-setwebproxy", "-setwebproxystate", proxies.HTTP))
		recordMacOSFirstError(&firstErr, setMacOSProxyEndpoint(service, "-setsecurewebproxy", "-setsecurewebproxystate", proxies.HTTP))
		recordMacOSFirstError(&firstErr, setMacOSProxyEndpoint(service, "-setsocksfirewallproxy", "-setsocksfirewallproxystate", proxies.SOCKS5))
	}
	return firstErr
}

func recordMacOSFirstError(firstErr *error, err error) {
	if err != nil && *firstErr == nil {
		*firstErr = err
	}
}

func setMacOSProxyEndpoint(service, setCmd, stateCmd string, endpoint *localProxyEndpoint) error {
	if endpoint == nil {
		return networksetupRun(stateCmd, service, "off")
	}
	if err := networksetupRun(setCmd, service, endpoint.Host, strconv.Itoa(endpoint.Port)); err != nil {
		return err
	}
	return networksetupRun(stateCmd, service, "on")
}

func restoreMacOSProxyEndpoint(service, setCmd, stateCmd string, state macOSProxyEndpointState) error {
	if state.server != "" && state.port > 0 {
		if err := networksetupRun(setCmd, service, state.server, strconv.Itoa(state.port)); err != nil {
			return err
		}
	}
	if state.enabled {
		return networksetupRun(stateCmd, service, "on")
	}
	return networksetupRun(stateCmd, service, "off")
}

func restoreMacOSAutoProxy(service string, state macOSAutoProxyState) error {
	if state.url != "" {
		if err := networksetupRun("-setautoproxyurl", service, state.url); err != nil {
			return err
		}
	}
	if state.enabled {
		return networksetupRun("-setautoproxystate", service, "on")
	}
	return networksetupRun("-setautoproxystate", service, "off")
}

func readMacOSProxyEndpointState(service, getCmd string) (macOSProxyEndpointState, error) {
	output, err := networksetupOutput(getCmd, service)
	if err != nil {
		return macOSProxyEndpointState{}, err
	}
	values, err := parseMacOSNetworksetupOutput(output)
	if err != nil {
		return macOSProxyEndpointState{}, err
	}
	port, err := parseMacOSPort(values["Port"])
	if err != nil {
		return macOSProxyEndpointState{}, err
	}
	return macOSProxyEndpointState{
		enabled: parseMacOSBool(values["Enabled"]),
		server:  normalizeMacOSValue(values["Server"]),
		port:    port,
	}, nil
}

func readMacOSAutoProxyState(service string) (macOSAutoProxyState, error) {
	output, err := networksetupOutput("-getautoproxyurl", service)
	if err != nil {
		return macOSAutoProxyState{}, err
	}
	values, err := parseMacOSNetworksetupOutput(output)
	if err != nil {
		return macOSAutoProxyState{}, err
	}
	return macOSAutoProxyState{
		enabled: parseMacOSBool(values["Enabled"]),
		url:     normalizeMacOSValue(values["URL"]),
	}, nil
}

func parseMacOSNetworksetupOutput(output string) (map[string]string, error) {
	values := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("unexpected networksetup output: %q", line)
		}
		values[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	return values, nil
}

func parseMacOSPort(value string) (int, error) {
	value = normalizeMacOSValue(value)
	if value == "" {
		return 0, nil
	}
	port, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("invalid port %q: %w", value, err)
	}
	return port, nil
}

func parseMacOSBool(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "yes" || value == "on" || value == "1" || value == "true"
}

func normalizeMacOSValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "(null)" {
		return ""
	}
	return value
}

func networksetupOutput(args ...string) (string, error) {
	output, err := exec.Command("networksetup", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("networksetup %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func networksetupRun(args ...string) error {
	_, err := networksetupOutput(args...)
	return err
}
