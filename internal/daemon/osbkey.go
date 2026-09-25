package daemon

import (
	"fmt"

	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/osb"
)

// osbKey settles the key the API requires. A given key wins; with none, one is generated and
// stored in ~/.sbx/osb/key (reused on restart) so that `sbx mcp` and the SDKs on this machine
// can find it. No key at all only with --osb-insecure-no-key, on loopback, and said loudly:
// loopback is not private on a VM-backed engine, where every container reaches the host's
// 127.0.0.1 through the VM's gateway.
func (d *daemon) osbKey(addr, key string) (string, error) {
	if d.osbNoKey {
		if key != "" {
			return "", fmt.Errorf("--osb-insecure-no-key and a key (--osb-key or SBX_OSB_KEY) " +
				"contradict each other - drop one")
		}

		if !loopbackOnly(addr) {
			return "", fmt.Errorf("--osb-insecure-no-key is loopback only, and --osb-addr %s is "+
				"not - bind 127.0.0.1, and drop --osb-insecure-no-key to have a key generated", addr)
		}

		logs.Default.Warn("", "", "the OpenSandbox API on %s has NO KEY (--osb-insecure-no-key). "+
			"Containers on a VM-backed engine (colima, Docker Desktop) reach this host's loopback "+
			"through host.docker.internal, so any sandbox - including one running untrusted code - "+
			"can create, read and run commands in every other API sandbox", addr)

		return "", nil
	}

	if key != "" {
		return key, nil
	}

	dir, err := osb.DefaultStateDir()
	if err != nil {
		return "", err
	}

	key, path, created, err := osb.LoadOrCreateKey(dir)
	if err != nil {
		return "", err
	}

	// Where, never what: a key in a log is a key in every log shipper downstream of it.
	verb := "using the"
	if created {
		verb = "generated a new"
	}

	logs.Default.Info("", "", "OpenSandbox API key: %s key in %s (sbx mcp reads it; SDKs: "+
		"export OPEN_SANDBOX_API_KEY=\"$(cat %s)\")", verb, path, path)

	return key, nil
}
