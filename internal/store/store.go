// Package store keeps the CLI's secrets: the refresh and access token of
// each profile. On macOS it is the login keychain, through /usr/bin/security;
// elsewhere, or without that binary, one private JSON file in the state
// directory.
package store

import "errors"

// ErrNotFound means nothing is stored under the key. Any other error means
// the store could not be read, which says nothing about whether a sign in
// exists.
var ErrNotFound = errors.New("Nothing is stored under that name.")

// Store holds string secrets by key.
type Store interface {
	Get(key string) (string, error)
	Put(key, value string) error
	Delete(key string) error
}
