// Package keychain wraps the OS keychain (macOS Keychain, Linux secret-tool /
// Secret Service, Windows Credential Manager) with a timeout, because a locked
// Linux keyring can block forever waiting for an unlock prompt.
package keychain

import (
	"errors"
	"os"
	"time"

	"github.com/zalando/go-keyring"
)

const (
	Service = "mldojo"
	User    = "master-key"
)

var ErrDisabled = errors.New("keychain disabled (MLDOJO_NO_KEYCHAIN=1)")

func disabled() bool { return os.Getenv("MLDOJO_NO_KEYCHAIN") == "1" }

func withTimeout[T any](f func() (T, error)) (T, error) {
	type res struct {
		v   T
		err error
	}
	ch := make(chan res, 1)
	go func() {
		v, err := f()
		ch <- res{v, err}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-time.After(3 * time.Second):
		var zero T
		return zero, errors.New("keychain timed out (locked keyring?)")
	}
}

// Get returns the master key stored in the OS keychain.
func Get() (string, error) {
	if disabled() {
		return "", ErrDisabled
	}
	return withTimeout(func() (string, error) { return keyring.Get(Service, User) })
}

// Set stores the master key in the OS keychain.
func Set(key string) error {
	if disabled() {
		return ErrDisabled
	}
	_, err := withTimeout(func() (struct{}, error) { return struct{}{}, keyring.Set(Service, User, key) })
	return err
}
