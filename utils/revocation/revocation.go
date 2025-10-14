/*
 * Copyright 2024 Maritime Connectivity Platform Consortium
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

package revocation

import (
	"bytes"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/crypto/ocsp"
)

func PerformOCSPCheck(clientCert *x509.Certificate, issuingCert *x509.Certificate, httpClient *http.Client) error {
	ocspUrl := clientCert.OCSPServer[0]
	ocspReq, err := ocsp.CreateRequest(clientCert, issuingCert, nil)
	if err != nil {
		return fmt.Errorf("could not create OCSP request for the given client cert: %w", err)
	}
	resp, err := httpClient.Post(ocspUrl, "application/ocsp-request", bytes.NewBuffer(ocspReq))
	if err != nil {
		return fmt.Errorf("could not send OCSP request: %w", err)
	}
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("getting OCSP response failed: %w", err)
	}
	if err = resp.Body.Close(); err != nil {
		return fmt.Errorf("could not close response body: %w", err)
	}
	ocspResp, err := ocsp.ParseResponse(respBytes, nil)
	if err != nil {
		return fmt.Errorf("parsing OCSP response failed: %w", err)
	}
	if ocspResp.SerialNumber.Cmp(clientCert.SerialNumber) != 0 {
		return fmt.Errorf("the serial number in the OCSP response does not correspond to the serial number of the certificate being checked")
	}
	if ocspResp.Certificate == nil {
		if err = ocspResp.CheckSignatureFrom(issuingCert); err != nil {
			return fmt.Errorf("the signature on the OCSP response is not valid: %w", err)
		}
	}
	if (ocspResp.Certificate != nil) && !ocspResp.Certificate.Equal(issuingCert) {
		return fmt.Errorf("the certificate embedded in the OCSP response does not match the configured issuing CA")
	}
	if ocspResp.Status != ocsp.Good {
		return fmt.Errorf("the given client certificate has been revoked")
	}
	return nil
}

func PerformCRLCheck(clientCert *x509.Certificate, httpClient *http.Client, issuingCert *x509.Certificate) error {
	crlURL := clientCert.CRLDistributionPoints[0]
	resp, err := httpClient.Get(crlURL)
	if err != nil {
		return fmt.Errorf("could not send CRL request: %w", err)
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("getting CRL response body failed: %w", err)
	}
	if err = resp.Body.Close(); err != nil {
		return fmt.Errorf("failed to close CRL response body: %w", err)
	}
	crl, err := x509.ParseRevocationList(respBody)
	if err != nil {
		return fmt.Errorf("could not parse received CRL: %w", err)
	}
	if err = crl.CheckSignatureFrom(issuingCert); err != nil {
		return fmt.Errorf("signature on CRL is not valid: %w", err)
	}
	now := time.Now().UTC()
	for _, rev := range crl.RevokedCertificateEntries {
		if (rev.SerialNumber.Cmp(clientCert.SerialNumber) == 0) && (rev.RevocationTime.UTC().Before(now)) {
			return fmt.Errorf("the given client certificate has been revoked")
		}
	}
	return nil
}
