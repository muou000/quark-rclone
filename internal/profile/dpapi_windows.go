//go:build windows

package profile

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// cryptProtectUIForbidden keeps DPAPI from ever showing a prompt: a mount
// session must not pop UI from a background process.
const cryptProtectUIForbidden = 0x1

// protectData encrypts plain with Windows DPAPI in CurrentUser scope (ADR
// 0003): only this Windows account on this machine can decrypt it.
func protectData(plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return nil, fmt.Errorf("dpapi protect: empty input")
	}
	in := windows.DataBlob{Size: uint32(len(plain)), Data: &plain[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, cryptProtectUIForbidden, &out); err != nil {
		return nil, fmt.Errorf("dpapi protect: %w", err)
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(out.Data))
	// Copy out of the CoTaskMem allocation before freeing it.
	cipher := make([]byte, out.Size)
	copy(cipher, unsafe.Slice(out.Data, out.Size))
	return cipher, nil
}

// unprotectData decrypts a DPAPI CurrentUser blob.
func unprotectData(cipher []byte) ([]byte, error) {
	if len(cipher) == 0 {
		return nil, fmt.Errorf("dpapi unprotect: empty input")
	}
	in := windows.DataBlob{Size: uint32(len(cipher)), Data: &cipher[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, cryptProtectUIForbidden, &out); err != nil {
		return nil, fmt.Errorf("dpapi unprotect: %w", err)
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(out.Data))
	plain := make([]byte, out.Size)
	copy(plain, unsafe.Slice(out.Data, out.Size))
	return plain, nil
}
