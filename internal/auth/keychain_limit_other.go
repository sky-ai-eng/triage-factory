//go:build !windows

package auth

// platformChunkLimit is 0 (chunking disabled): the macOS and Linux keychains
// store a value in one entry exactly as given.
const platformChunkLimit = 0
