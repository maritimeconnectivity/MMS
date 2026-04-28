package main

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"os"

	"github.com/charmbracelet/log"
	"github.com/libp2p/go-libp2p/core/crypto"

	"github.com/libp2p/go-libp2p/core/peer"
)

func genkey(privKeyFilePath string) (string, error) {
	privKeyFile, err := os.ReadFile(privKeyFilePath)
	if err != nil {
		return "", fmt.Errorf("could not open the provided private key file: %w", err)
	}
	keyData, _ := pem.Decode(privKeyFile)
	if keyData == nil {
		return "", fmt.Errorf("could not decode PEM data from the provided private key file")
	}
	if keyData.Type != "EC PRIVATE KEY" && keyData.Type != "PRIVATE KEY" {
		return "", fmt.Errorf("unsupported PEM block type %q in provided private key file", keyData.Type)
	}
	privKey, err := x509.ParseECPrivateKey(keyData.Bytes)
	if err != nil {
		privKeyPkcs8, pkcs8Err := x509.ParsePKCS8PrivateKey(keyData.Bytes)
		if pkcs8Err != nil {
			return "", fmt.Errorf("could not parse the provided private key file: EC parse error: %v, PKCS#8 parse error: %w", err, pkcs8Err)
		}
		v, ok := privKeyPkcs8.(*ecdsa.PrivateKey)
		if !ok {
			return "", fmt.Errorf("could not assert parsed PKCS#8 key as *ecdsa.PrivateKey")
		}
		privKey = v
	}

	privEc, _, err := crypto.ECDSAKeyPairFromKey(privKey)
	if err != nil {
		return "", fmt.Errorf("could not parse the ECDSA private key from the file: %w", err)
	}

	id, err := peer.IDFromPrivateKey(privEc)
	if err != nil {
		return "", fmt.Errorf("could not generate peer ID from the provided private key: %w", err)
	}

	return id.String(), nil
}

func main() {
	privKeyFilePath := flag.String("privkey", "", "Path to a file containing a private key.")
	flag.Parse()

	if *privKeyFilePath == "" {
		log.Fatal("Need to pass in privkey")
	}

	id, err := genkey(*privKeyFilePath)

	if err != nil {
		log.Fatal("Failed to generate peer ID from private key:", err)
	}

	fmt.Printf("%s\n", id)
}
