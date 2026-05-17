package account

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Account is one named API key entry.
type Account struct {
	Provider string `toml:"provider"`
	APIKey   string `toml:"api_key"`
}

// File is the on-disk shape of ~/.fuse/accounts.toml.
type File struct {
	Accounts map[string]Account `toml:"accounts"`
}

// FingerprintLen is the prefix length used to identify a key without exposing it.
const FingerprintLen = 12

// DefaultPath returns ~/.fuse/accounts.toml.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".fuse")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "accounts.toml"), nil
}

// ErrNoAccount means the named account does not exist in the file.
var ErrNoAccount = errors.New("account not found")

// Load reads and parses the accounts file. Missing file -> empty map (not error).
func Load(path string) (*File, error) {
	f := &File{Accounts: map[string]Account{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return nil, err
	}
	if err := toml.Unmarshal(data, f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if f.Accounts == nil {
		f.Accounts = map[string]Account{}
	}
	return f, nil
}

// Lookup returns the named account or ErrNoAccount.
func (f *File) Lookup(name string) (Account, error) {
	a, ok := f.Accounts[name]
	if !ok {
		return Account{}, ErrNoAccount
	}
	return a, nil
}

// Fingerprint returns the first FingerprintLen chars of the key, or the whole
// key if shorter. Used to compare against an inbound x-api-key prefix.
func Fingerprint(key string) string {
	if len(key) <= FingerprintLen {
		return key
	}
	return key[:FingerprintLen]
}

// FingerprintMatches reports whether the inbound key's prefix matches the named
// account's stored key prefix. Empty inbound returns false.
func (f *File) FingerprintMatches(name, inboundKey string) bool {
	a, err := f.Lookup(name)
	if err != nil {
		return false
	}
	if inboundKey == "" || a.APIKey == "" {
		return false
	}
	return Fingerprint(a.APIKey) == Fingerprint(inboundKey)
}
