// Package fpoc implements the GitLab fleeting provider for OpenStack: it
// creates, tracks and deletes Nova instances so GitLab Runner's
// docker-autoscaler executor can use them as autoscaled CI workers.
package fpoc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// PrivPub is a private key that can produce its public half. It exists so the
// plugin can handle a generated and an operator-supplied key uniformly.
type PrivPub interface {
	crypto.PrivateKey
	Public() crypto.PublicKey
}

// initSSHKey prepare dynamic ssh key for flatcar instances
func (g *InstanceGroup) initSSHKey(_ context.Context, log hclog.Logger, settings *provider.Settings) error {
	var key PrivPub
	var err error

	if len(settings.Key) == 0 {
		log.Info("Generating dynamic SSH key...")

		rsaKey, err := rsa.GenerateKey(rand.Reader, 4096)
		if err != nil {
			return fmt.Errorf("generating private key: %w", err)
		}
		key = rsaKey
		settings.Key = pem.EncodeToMemory(
			&pem.Block{
				Type:  "RSA PRIVATE KEY",
				Bytes: x509.MarshalPKCS1PrivateKey(rsaKey),
			},
		)

		log.Debug("Key generated")
	} else {
		var ok bool

		priv, err := ssh.ParseRawPrivateKey(settings.Key)
		if err != nil {
			return fmt.Errorf("reading private key: %w", err)
		}

		key, ok = priv.(PrivPub)
		if !ok {
			return fmt.Errorf("key doesn't export PublicKey()")
		}
	}

	log.Debug("Extracting public key...")
	sshPubKey, err := ssh.NewPublicKey(key.Public())
	if err != nil {
		return fmt.Errorf("generating private key: %w", err)
	}

	g.sshPubKey = string(ssh.MarshalAuthorizedKey(sshPubKey))
	log.With("public_key", g.sshPubKey).Debug("Extracted public key")

	return nil
}
