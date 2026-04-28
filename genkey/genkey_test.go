package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"
)

func TestGenkey_HappyPath(t *testing.T) {
	dir := t.TempDir()

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	privateKeyBytes, err := x509.MarshalECPrivateKey(privateKey)
	require.NoError(t, err)

	path := writeTempFile(t, dir, "ecdsa-private-key.pem", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privateKeyBytes}))

	libp2pKey, _, err := crypto.ECDSAKeyPairFromKey(privateKey)
	require.NoError(t, err)

	wantID, err := peer.IDFromPrivateKey(libp2pKey)
	require.NoError(t, err)

	gotID, err := genkey(path)
	require.NoError(t, err)

	require.Equal(t, wantID.String(), gotID)
}

func TestGenkey_ErrorCases(t *testing.T) {
	dir := t.TempDir()

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	rsaPKCS8, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	require.NoError(t, err)

	tests := []struct {
		name    string
		path    string
		wantErr string
	}{
		{
			name:    "missing file",
			path:    filepath.Join(dir, "missing.pem"),
			wantErr: "could not open the provided private key file",
		},
		{
			name:    "invalid PEM data",
			path:    writeTempFile(t, dir, "invalid-pem.pem", []byte("not pem content")),
			wantErr: "could not decode PEM data from the provided private key file",
		},
		{
			name:    "unsupported PEM block type",
			path:    writeTempFile(t, dir, "unsupported-type.pem", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("unused")})),
			wantErr: "unsupported PEM block type \"RSA PRIVATE KEY\" in provided private key file",
		},
		{
			name:    "invalid private key bytes",
			path:    writeTempFile(t, dir, "invalid-key.pem", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("definitely not a private key")})),
			wantErr: "could not parse the provided private key file",
		},
		{
			name:    "PKCS8 key is not ECDSA",
			path:    writeTempFile(t, dir, "rsa-pkcs8.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rsaPKCS8})),
			wantErr: "could not assert parsed PKCS#8 key as *ecdsa.PrivateKey",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := genkey(tt.path)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func writeTempFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()

	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	return path
}
