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

package auth

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"github.com/maritimeconnectivity/MMS/mmtp"
	"log"
	"net/http"
	"nhooyr.io/websocket"
	"strconv"
	"strings"
)

func AuthenticateAgent(request *http.Request, agentMrn string, c *websocket.Conn) (x509.SignatureAlgorithm, bool, error) {
	// If TLS is enabled, we should verify the certificate from the Agent
	if request.TLS != nil && agentMrn != "" {
		uidOid := []int{0, 9, 2342, 19200300, 100, 1, 1}

		if len(request.TLS.PeerCertificates) < 1 {
			if wsErr := c.Close(websocket.StatusPolicyViolation, "A valid client certificate must be provided for authenticated connections"); wsErr != nil {
				log.Println(wsErr)
			}
			return x509.UnknownSignatureAlgorithm, false, fmt.Errorf("client certificate validation failed, websocket closed")
		}

		//Determine signature Algorithm used by client and check if valid
		signatureAlgorithm, err := getSignatureAlgorithm(request)
		if err != nil {
			if wsErr := c.Close(websocket.StatusPolicyViolation, err.Error()); wsErr != nil {
				log.Println(wsErr)
			}
			return x509.UnknownSignatureAlgorithm, false, err
		}

		// https://stackoverflow.com/a/50640119
		for _, n := range request.TLS.PeerCertificates[0].Subject.Names {
			if n.Type.Equal(uidOid) {
				if v, ok := n.Value.(string); ok {
					if !strings.EqualFold(v, agentMrn) {
						if wsErr := c.Close(websocket.StatusUnsupportedData, "The MRN given in the Connect message does not match the one in the certificate that was used for authentication"); wsErr != nil {
							log.Println(wsErr)
						}
						return x509.UnknownSignatureAlgorithm, false, fmt.Errorf("connect message MRN does not match certificate MRN, websocket closed")
					}
				}
			}
		}
		//All checks complete, so authenticated
		return signatureAlgorithm, true, nil
	}
	return x509.UnknownSignatureAlgorithm, false, nil
}

func getSignatureAlgorithm(request *http.Request) (x509.SignatureAlgorithm, error) {
	pubKeyLen := 0
	switch pubKey := request.TLS.PeerCertificates[0].PublicKey.(type) {
	case *ecdsa.PublicKey:
		if pubKeyLen = pubKey.Params().BitSize; pubKeyLen < 256 {
			return 0, fmt.Errorf("the public key length of the provided client certificate cannot be less than 256 bits")
		}
	default:
		return 0, fmt.Errorf("the provided client certificate does not use an allowed public key algorithm")
	}

	var signatureAlgorithm x509.SignatureAlgorithm
	switch pubKeyLen {
	case 256:
		signatureAlgorithm = x509.ECDSAWithSHA256
	case 384:
		signatureAlgorithm = x509.ECDSAWithSHA384
	case 512:
		signatureAlgorithm = x509.ECDSAWithSHA512
	default:
		return 0, fmt.Errorf("the public key length of the provided client certificate is not supported")
	}
	return signatureAlgorithm, nil
}

func VerifySignatureOnMessage(mmtpMessage *mmtp.MmtpMessage, signatureAlgorithm x509.SignatureAlgorithm, request *http.Request) error {
	appMessage := mmtpMessage.GetProtocolMessage().GetSendMessage().GetApplicationMessage()

	// verify signature on message
	signatureBytes, err := base64.StdEncoding.DecodeString(appMessage.GetSignature())
	if err != nil {
		return fmt.Errorf("signature could be not decoded from base64: %w", err)
	}

	toBeVerified := make([]byte, 0)
	switch content := appMessage.GetHeader().GetSubjectOrRecipient().(type) {
	case *mmtp.ApplicationMessageHeader_Subject:
		toBeVerified = append(toBeVerified, content.Subject...)
	case *mmtp.ApplicationMessageHeader_Recipients:
		for _, r := range content.Recipients.GetRecipients() {
			toBeVerified = append(toBeVerified, r...)
		}
	}

	toBeVerified = append(toBeVerified, strconv.FormatInt(appMessage.GetHeader().GetExpires(), 10)...)
	toBeVerified = append(toBeVerified, appMessage.GetHeader().GetSender()...)

	if appMessage.GetHeader().GetQosProfile() != "" {
		toBeVerified = append(toBeVerified, appMessage.Header.GetQosProfile()...)
	}

	toBeVerified = append(toBeVerified, strconv.Itoa(int(appMessage.GetHeader().GetBodySizeNumBytes()))...)
	toBeVerified = append(toBeVerified, appMessage.GetBody()...)

	if signatureAlgorithm == x509.UnknownSignatureAlgorithm {
		return fmt.Errorf("a suitable signature algorithm could not be found for verifying signature on message")
	}

	if err = request.TLS.PeerCertificates[0].CheckSignature(signatureAlgorithm, toBeVerified, signatureBytes); err != nil {
		// return an error saying that the signature is not valid over the body of the message
		return fmt.Errorf("the signature on the message could not be verified: %w", err)
	}
	return nil
}
