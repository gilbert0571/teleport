/*
Copyright 2022 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package keys defines common interfaces for Teleport client keys.
package keys

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"

	"github.com/gravitational/trace"
	"golang.org/x/crypto/ssh"

	"github.com/gravitational/teleport/api/utils/sshutils/ppk"
)

const (
	PKCS1PrivateKeyType      = "RSA PRIVATE KEY"
	PKCS8PrivateKeyType      = "PRIVATE KEY"
	ECPrivateKeyType         = "EC PRIVATE KEY"
	pivYubiKeyPrivateKeyType = "PIV YUBIKEY PRIVATE KEY"
)

type cryptoPublicKeyI interface {
	Equal(x crypto.PublicKey) bool
}

// PrivateKey implements crypto.Signer with additional helper methods. The underlying
// private key may be a standard crypto.Signer implemented in the standard library
// (aka *rsa.PrivateKey, *ecdsa.PrivateKey, or ed25519.PrivateKey), or it may be a
// custom implementation for a non-standard private key, such as a hardware key.
type PrivateKey struct {
	crypto.Signer
	// sshPub is the public key in ssh.PublicKey form.
	sshPub ssh.PublicKey
	// keyPEM is PEM-encoded private key data which can be parsed with ParsePrivateKey.
	keyPEM []byte
}

// NewPrivateKey returns a new PrivateKey for the given crypto.Signer.
func NewPrivateKey(signer crypto.Signer, keyPEM []byte) (*PrivateKey, error) {
	sshPub, err := ssh.NewPublicKey(signer.Public())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return &PrivateKey{
		Signer: signer,
		sshPub: sshPub,
		keyPEM: keyPEM,
	}, nil
}

// SSHPublicKey returns the ssh.PublicKey representiation of the public key.
func (k *PrivateKey) SSHPublicKey() ssh.PublicKey {
	return k.sshPub
}

// SSHPublicKey returns the ssh.PublicKey representiation of the public key.
func (k *PrivateKey) MarshalSSHPublicKey() []byte {
	return ssh.MarshalAuthorizedKey(k.sshPub)
}

// PrivateKeyPEM returns PEM encoded private key data. This may be data necessary
// to retrieve the key, such as a YubiKey serial number and slot, or it can be a
// PKCS marshaled private key.
//
// The resulting PEM encoded data should only be decoded with ParsePrivateKey to
// prevent errors from parsing non PKCS marshaled keys, such as a PIV key.
func (k *PrivateKey) PrivateKeyPEM() []byte {
	return k.keyPEM
}

// TLSCertificate parses the given TLS certificate(s) paired with the private key
// to rerturn a tls.Certificate, ready to be used in a TLS handshake.
func (k *PrivateKey) TLSCertificate(certPEMBlock []byte) (tls.Certificate, error) {
	cert := tls.Certificate{
		PrivateKey: k.Signer,
	}

	var skippedBlockTypes []string
	for {
		var certDERBlock *pem.Block
		certDERBlock, certPEMBlock = pem.Decode(certPEMBlock)
		if certDERBlock == nil {
			break
		}
		if certDERBlock.Type == "CERTIFICATE" {
			cert.Certificate = append(cert.Certificate, certDERBlock.Bytes)
		} else {
			skippedBlockTypes = append(skippedBlockTypes, certDERBlock.Type)
		}
	}

	if len(cert.Certificate) == 0 {
		if len(skippedBlockTypes) == 0 {
			return tls.Certificate{}, trace.BadParameter("tls: failed to find any PEM data in certificate input")
		}
		return tls.Certificate{}, trace.BadParameter("tls: failed to find \"CERTIFICATE\" PEM block in certificate input after skipping PEM blocks of the following types: %v", skippedBlockTypes)
	}

	// Check that the certificate's public key matches this private key.
	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return tls.Certificate{}, trace.Wrap(err)
	}

	if keyPub, ok := k.Public().(cryptoPublicKeyI); !ok {
		return tls.Certificate{}, trace.BadParameter("private key does not contain a valid public key")
	} else if !keyPub.Equal(x509Cert.PublicKey) {
		return tls.Certificate{}, trace.BadParameter("private key does not match certificate's public key")
	}

	return cert, nil
}

// PPKFile returns a PuTTY PPK-formatted keypair
func (k *PrivateKey) PPKFile() ([]byte, error) {
	rsaKey, ok := k.Signer.(*rsa.PrivateKey)
	if !ok {
		return nil, trace.BadParameter("cannot use private key of type %T as rsa.PrivateKey", k)
	}
	ppkFile, err := ppk.ConvertToPPK(rsaKey, k.MarshalSSHPublicKey())
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return ppkFile, nil
}

// RSAPrivateKeyPEM returns a PEM encoded RSA private key for the given key.
// If the given key is not an RSA key, then an error will be returned.
//
// This is used by some integrations which currently only support raw RSA private keys,
// like Kubernetes, MongoDB, and PPK files for windows.
func (k *PrivateKey) RSAPrivateKeyPEM() ([]byte, error) {
	if _, ok := k.Signer.(*rsa.PrivateKey); !ok {
		return nil, trace.BadParameter("cannot get rsa key PEM for private key of type %T", k.Signer)
	}
	return k.keyPEM, nil
}

// LoadPrivateKey returns the PrivateKey for the given key file.
func LoadPrivateKey(keyFile string) (*PrivateKey, error) {
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, trace.ConvertSystemError(err)
	}

	priv, err := ParsePrivateKey(keyPEM)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return priv, nil
}

// ParsePrivateKey returns the PrivateKey for the given key PEM block.
func ParsePrivateKey(keyPEM []byte) (*PrivateKey, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, trace.BadParameter("expected PEM encoded private key")
	}

	switch block.Type {
	case PKCS1PrivateKeyType:
		cryptoSigner, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return NewPrivateKey(cryptoSigner, keyPEM)
	case ECPrivateKeyType:
		cryptoSigner, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return NewPrivateKey(cryptoSigner, keyPEM)
	case PKCS8PrivateKeyType:
		priv, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		cryptoSigner, ok := priv.(crypto.Signer)
		if !ok {
			return nil, trace.BadParameter("x509.ParsePKCS8PrivateKey returned an invalid private key of type %T", priv)
		}
		return NewPrivateKey(cryptoSigner, keyPEM)
	case pivYubiKeyPrivateKeyType:
		priv, err := parseYubiKeyPrivateKeyData(block.Bytes)
		if err != nil {
			return nil, trace.Wrap(err, "failed to parse YubiKey private key")
		}
		return NewPrivateKey(priv, keyPEM)
	default:
		return nil, trace.BadParameter("unexpected private key PEM type %q", block.Type)
	}
}

// LoadKeyPair returns the PrivateKey for the given private and public key files.
func LoadKeyPair(privFile, sshPubFile string) (*PrivateKey, error) {
	privPEM, err := os.ReadFile(privFile)
	if err != nil {
		return nil, trace.ConvertSystemError(err)
	}

	marshalledSSHPub, err := os.ReadFile(sshPubFile)
	if err != nil {
		return nil, trace.ConvertSystemError(err)
	}

	priv, err := ParseKeyPair(privPEM, marshalledSSHPub)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return priv, nil
}

// ParseKeyPair returns the PrivateKey for the given private and public key PEM blocks.
func ParseKeyPair(privPEM, marshalledSSHPub []byte) (*PrivateKey, error) {
	priv, err := ParsePrivateKey(privPEM)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Verify that the private key's public key matches the expected public key.
	if !bytes.Equal(ssh.MarshalAuthorizedKey(priv.SSHPublicKey()), marshalledSSHPub) {
		return nil, trace.CompareFailed("the given private and public keys do not form a valid keypair")
	}

	return priv, nil
}

// LoadX509KeyPair parse a tls.Certificate from a private key file and certificate file.
// This should be used instead of tls.LoadX509KeyPair to support non-raw private keys, like PIV keys.
func LoadX509KeyPair(certFile, keyFile string) (tls.Certificate, error) {
	keyPEMBlock, err := os.ReadFile(keyFile)
	if err != nil {
		return tls.Certificate{}, trace.ConvertSystemError(err)
	}

	certPEMBlock, err := os.ReadFile(certFile)
	if err != nil {
		return tls.Certificate{}, trace.ConvertSystemError(err)
	}

	tlsCert, err := X509KeyPair(certPEMBlock, keyPEMBlock)
	if err != nil {
		return tls.Certificate{}, trace.Wrap(err)
	}

	return tlsCert, nil
}

// X509KeyPair parse a tls.Certificate from a private key PEM and certificate PEM.
// This should be used instead of tls.X509KeyPair to support non-raw private keys, like PIV keys.
func X509KeyPair(certPEMBlock, keyPEMBlock []byte) (tls.Certificate, error) {
	priv, err := ParsePrivateKey(keyPEMBlock)
	if err != nil {
		return tls.Certificate{}, trace.Wrap(err)
	}

	tlsCert, err := priv.TLSCertificate(certPEMBlock)
	if err != nil {
		return tls.Certificate{}, trace.Wrap(err)
	}

	return tlsCert, nil
}
