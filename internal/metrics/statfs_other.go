//go:build !linux

package metrics

import "errors"

// The agent is built for Linux only; the stub exists so that the package
// compiles during local development on another system.
func statfs(string) (usage, error) {
	return usage{}, errors.New("statfs is available on linux only")
}
