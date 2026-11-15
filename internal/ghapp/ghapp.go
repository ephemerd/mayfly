package ghapp

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	"github.com/luthermonson/mayfly/internal/config"
)

type Credentials struct {
	AppID          int64
	InstallationID int64
	PrivateKeyPEM  []byte
}

func Load(cfg config.GitHub) (*Credentials, error) {
	if cfg.AppID == 0 {
		return nil, errors.New("github: app_id is 0")
	}
	if cfg.InstallationID == 0 {
		return nil, errors.New("github: installation_id is 0")
	}
	if cfg.PrivateKeyPath == "" {
		return nil, errors.New("github: private_key_path is empty")
	}
	b, err := os.ReadFile(cfg.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("github: read private key: %w", err)
	}
	if err := validatePEM(b); err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	return &Credentials{
		AppID:          cfg.AppID,
		InstallationID: cfg.InstallationID,
		PrivateKeyPEM:  b,
	}, nil
}

func validatePEM(b []byte) error {
	block, _ := pem.Decode(b)
	if block == nil {
		return errors.New("private key is not PEM-encoded")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		if _, err := x509.ParsePKCS1PrivateKey(block.Bytes); err != nil {
			return fmt.Errorf("invalid RSA private key: %w", err)
		}
	case "PRIVATE KEY":
		if _, err := x509.ParsePKCS8PrivateKey(block.Bytes); err != nil {
			return fmt.Errorf("invalid PKCS8 private key: %w", err)
		}
	default:
		return fmt.Errorf("unexpected PEM type %q", block.Type)
	}
	return nil
}
