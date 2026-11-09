//go:build !darwin

package store

// Default is the file in the state directory: there is no keychain here.
func Default() Store { return &File{} }
