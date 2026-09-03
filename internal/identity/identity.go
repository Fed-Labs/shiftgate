package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/persistence"
)

type Identity struct {
	Machine model.MachineIdentity
	private ed25519.PrivateKey
	public  ed25519.PublicKey
}

func Ensure(directory string) (*Identity, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create identity directory: %w", err)
	}
	privatePath := filepath.Join(directory, "identity-key.pem")
	publicPath := filepath.Join(directory, "identity-public.pem")
	private, public, err := loadKeyPair(privatePath, publicPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		public, private, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("generate machine identity: %w", err)
		}
		if err := writeKeyPair(privatePath, publicPath, private, public); err != nil {
			return nil, err
		}
	}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("read hostname: %w", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		return nil, fmt.Errorf("marshal public key: %w", err)
	}
	digest := sha256.Sum256(publicDER)
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	return &Identity{
		Machine: model.MachineIdentity{
			ID:           hex.EncodeToString(digest[:]),
			Name:         hostname,
			PublicKeyPEM: string(publicPEM),
		},
		private: private,
		public:  public,
	}, nil
}

func (i *Identity) Sign(message []byte) []byte {
	return ed25519.Sign(i.private, message)
}

func (i *Identity) Verify(message, signature []byte) bool {
	return ed25519.Verify(i.public, message, signature)
}

func Verify(publicKeyPEM string, message, signature []byte) error {
	public, err := parsePublicKey(publicKeyPEM)
	if err != nil {
		return err
	}
	if !ed25519.Verify(public, message, signature) {
		return errors.New("signature verification failed")
	}
	return nil
}

func PublicKeyID(publicKeyPEM string) (string, error) {
	block, _ := pem.Decode([]byte(publicKeyPEM))
	if block == nil || block.Type != "PUBLIC KEY" {
		return "", errors.New("invalid public key PEM")
	}
	if _, err := parsePublicKey(publicKeyPEM); err != nil {
		return "", err
	}
	digest := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(digest[:]), nil
}

func parsePublicKey(publicKeyPEM string) (ed25519.PublicKey, error) {
	block, _ := pem.Decode([]byte(publicKeyPEM))
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("invalid public key PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	public, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("public key is not Ed25519")
	}
	return public, nil
}

func (i *Identity) EnsureTLSCertificate(directory string, validity time.Duration) (certificatePath, keyPath string, err error) {
	certificatePath = filepath.Join(directory, "tls-cert.pem")
	keyPath = filepath.Join(directory, "tls-key.pem")
	if certificateCurrent(certificatePath, i.Machine.ID, 7*24*time.Hour) {
		return certificatePath, keyPath, nil
	}
	if validity < 30*24*time.Hour {
		validity = 365 * 24 * time.Hour
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return "", "", err
	}
	hostname := i.Machine.Name
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   hostname,
			Organization: []string{"SHIFT machine " + i.Machine.ID[:16]},
		},
		NotBefore:             time.Now().Add(-5 * time.Minute),
		NotAfter:              time.Now().Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{hostname, "localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		SubjectKeyId:          []byte(i.Machine.ID),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, i.public, i.private)
	if err != nil {
		return "", "", fmt.Errorf("create TLS certificate: %w", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(i.private)
	if err != nil {
		return "", "", err
	}
	if err := persistence.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return "", "", err
	}
	if err := persistence.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600); err != nil {
		return "", "", err
	}
	return certificatePath, keyPath, nil
}

func certificateCurrent(path, machineID string, minimumRemaining time.Duration) bool {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(encoded)
	if block == nil {
		return false
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	return time.Until(certificate.NotAfter) > minimumRemaining && len(certificate.Subject.Organization) > 0 && certificate.Subject.Organization[0] == "SHIFT machine "+machineID[:16]
}

func loadKeyPair(privatePath, publicPath string) (ed25519.PrivateKey, ed25519.PublicKey, error) {
	privatePEM, err := os.ReadFile(privatePath)
	if err != nil {
		return nil, nil, err
	}
	publicPEM, err := os.ReadFile(publicPath)
	if err != nil {
		return nil, nil, err
	}
	privateBlock, _ := pem.Decode(privatePEM)
	publicBlock, _ := pem.Decode(publicPEM)
	if privateBlock == nil || publicBlock == nil {
		return nil, nil, errors.New("invalid identity PEM")
	}
	privateAny, err := x509.ParsePKCS8PrivateKey(privateBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	publicAny, err := x509.ParsePKIXPublicKey(publicBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	private, ok := privateAny.(ed25519.PrivateKey)
	if !ok {
		return nil, nil, errors.New("identity private key is not Ed25519")
	}
	public, ok := publicAny.(ed25519.PublicKey)
	if !ok || !private.Public().(ed25519.PublicKey).Equal(public) {
		return nil, nil, errors.New("identity key pair does not match")
	}
	return private, public, nil
}

func writeKeyPair(privatePath, publicPath string, private ed25519.PrivateKey, public ed25519.PublicKey) error {
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return err
	}
	publicDER, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		return err
	}
	if err := persistence.WriteFile(privatePath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600); err != nil {
		return err
	}
	return persistence.WriteFile(publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0o644)
}
