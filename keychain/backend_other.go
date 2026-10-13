//go:build !darwin && !linux && !windows

package keychain

// A platform john has no store for. Secrets stay in the file, which is what
// every platform did before this package existed.
func platformBackend() Backend { return none{} }

func missingBackendAdvice() string {
	return "john has no keychain backend for this platform"
}
