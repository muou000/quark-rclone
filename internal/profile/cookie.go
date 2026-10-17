package profile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
)

// credentialFileName is the DPAPI credential blob beside config.json. It is
// the only file in the store that carries the cookie.
const credentialFileName = "cookie.bin"

// The credential blob is versioned (ADR 0003): a fixed magic so a foreign or
// truncated file is recognisable, then one format byte, then the DPAPI
// ciphertext.
var credentialMagic = []byte{'Q', 'D', 'M'}

const credentialFormatV1 = 1

// ErrNoCredential reports that no credential has been stored yet — a fresh
// install before the first import.
var ErrNoCredential = errors.New("no stored credential")

// SaveCookie encrypts the cookie with DPAPI CurrentUser and atomically
// replaces the blob (ADR 0003). The cookie never touches the profile JSON,
// a command line, a log, or anything else this package writes.
func (s *Store) SaveCookie(cookie string) error {
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		return errors.New("refusing to store an empty credential")
	}
	cipher, err := protectData([]byte(cookie))
	if err != nil {
		return err
	}
	blob := make([]byte, 0, len(credentialMagic)+1+len(cipher))
	blob = append(blob, credentialMagic...)
	blob = append(blob, credentialFormatV1)
	blob = append(blob, cipher...)
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(s.CredPath(), blob, 0o600)
}

// LoadCookie decrypts and returns the stored credential.
func (s *Store) LoadCookie() (string, error) {
	raw, err := os.ReadFile(s.CredPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoCredential
	}
	if err != nil {
		return "", fmt.Errorf("read credential: %w", err)
	}
	if len(raw) < len(credentialMagic)+1 || !bytes.HasPrefix(raw, credentialMagic) {
		return "", &CorruptError{Path: s.CredPath(), Size: len(raw), Err: errors.New("not a QuarkDriveMount credential blob")}
	}
	switch format := raw[len(credentialMagic)]; format {
	case credentialFormatV1:
		plain, err := unprotectData(raw[len(credentialMagic)+1:])
		if err != nil {
			return "", &CorruptError{Path: s.CredPath(), Size: len(raw), Err: err}
		}
		return string(plain), nil
	default:
		return "", fmt.Errorf("credential blob format %d is not supported by this build", format)
	}
}

// DeleteCookie removes the credential blob, e.g. when the user signs out.
// A missing file is not an error: deleting twice must stay idempotent.
func (s *Store) DeleteCookie() error {
	err := os.Remove(s.CredPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
