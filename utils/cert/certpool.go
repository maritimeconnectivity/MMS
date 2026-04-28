package cert

import (
	"crypto/x509"
	"fmt"
	"os"
)

// LoadCertPool reads a PEM-encoded CA file and returns a CertPool.
// Returns (nil, nil) if caPath is empty.
func LoadCertPool(caPath string) (*x509.CertPool, error) {
	if caPath == "" {
		return nil, nil
	}
	pool := x509.NewCertPool()
	certFile, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("could not read CA file %q: %w", caPath, err)
	}
	if !pool.AppendCertsFromPEM(certFile) {
		return nil, fmt.Errorf("could not parse PEM certificates from CA file %q", caPath)
	}
	return pool, nil
}
