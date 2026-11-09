//go:build darwin

package store

import "os"

// Default is the keychain when /usr/bin/security exists, else the file in the
// state directory.
func Default() Store {
	if st, err := os.Stat(SecurityPath); err == nil && st.Mode().IsRegular() {
		return &Keychain{}
	}
	return &File{}
}
