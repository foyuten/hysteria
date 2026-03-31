//go:build !linux && !windows && !darwin

package cmd

import "errors"

func configureSystemProxy(_ *localProxySet, _ string) (func() error, error) {
	return nil, errors.New("system proxy is not supported on this platform")
}
