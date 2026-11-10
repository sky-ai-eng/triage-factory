//go:build windows

package auth

// platformChunkLimit is go-keyring's Windows cap on one entry's value
// (keyring_windows.go rejects len(password) > 2560 with ErrSetDataTooBig).
const platformChunkLimit = 2560
