/*
 * Copyright 2026 Maritime Connectivity Platform Consortium
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

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
