package osb

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/aryanmehrotra/sbx/internal/egress"
)

// Where the sandbox finds the certificate its filter signs with, and the variables that name it.
const (
	// vaultCAPath is the container path the authority is mounted read-only at.
	vaultCAPath = "/etc/sbx/credential-vault-ca.pem"

	// vaultCAEnv names that path for an entrypoint or a harness that wants to add the authority
	// to the image's own trust store - `update-ca-certificates` on Debian, `update-ca-trust` on
	// Fedora - which is what makes every client in the sandbox trust it, not only the ones that
	// read an environment variable.
	vaultCAEnv = "SBX_CREDENTIAL_VAULT_CA"

	// nodeExtraCAEnv is Node's, and it ADDS to the root set rather than replacing it.
	//
	// Only additive variables are set here. SSL_CERT_FILE and REQUESTS_CA_BUNDLE replace the
	// roots outright, so pointing either at this one certificate would leave the sandbox unable
	// to verify any other host - every https call that is not a bound one would start failing,
	// which is a worse break than the one this feature fixes. A caller that wants those set can
	// build a bundle from the image's own roots plus this file, which is why the path is named
	// in the environment as well as mounted.
	nodeExtraCAEnv = "NODE_EXTRA_CA_CERTS"
)

// credentialVaultCA mints the sandbox's authority and writes it where the create can mount it.
//
// It returns the host path and the environment the sandbox needs, or empty values when there is
// nothing to deliver: no registry, no policy, or a policy with no names of its own. The
// certificate is public material - the private key never leaves the daemon's memory - so writing
// it beside the sandbox's record is not a secret on disk.
func (s *Server) credentialVaultCA(id string, credentialProxy bool,
	pol *egress.Policy,
) (hostPath string, env map[string]string, err error) {
	// Only a sandbox that asked for a credential proxy, because only that sandbox can ever have a
	// vault - and a certificate in the trust store of one that cannot is an authority nothing in
	// the system will ever sign with.
	if s.credentials == nil || !credentialProxy || pol == nil {
		return "", nil, nil
	}

	pem, err := s.credentials.Certificate(id, *pol)
	if err != nil {
		return "", nil, fmt.Errorf("minting this sandbox's credential authority: %w", err)
	}

	if len(pem) == 0 {
		return "", nil, nil
	}

	dir := filepath.Join(s.store.dir, "credential-vault")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, fmt.Errorf("making a place for this sandbox's credential authority: %w", err)
	}

	hostPath = filepath.Join(dir, id+"-ca.pem")

	// 0644, because it is mounted into a container that does not run as this user and has to be
	// able to read it. A certificate is public; the key it belongs to is not written at all.
	if err := os.WriteFile(hostPath, pem, 0o644); err != nil {
		return "", nil, fmt.Errorf("writing this sandbox's credential authority: %w", err)
	}

	return hostPath, map[string]string{
		vaultCAEnv:     vaultCAPath,
		nodeExtraCAEnv: vaultCAPath,
	}, nil
}

// removeCredentialVaultCA drops the written certificate when the sandbox goes. The key is
// destroyed with the vault; this is the public half, and leaving it would accumulate one file per
// sandbox ever created.
func (s *Server) removeCredentialVaultCA(id string) {
	_ = os.Remove(filepath.Join(s.store.dir, "credential-vault", id+"-ca.pem"))
}
