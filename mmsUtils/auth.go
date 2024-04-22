package mmsUtils

import (
	"crypto/ecdsa"
	"crypto/x509"
	"fmt"
	"log"
	"net/http"
	"nhooyr.io/websocket"
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
