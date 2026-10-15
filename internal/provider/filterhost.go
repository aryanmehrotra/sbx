package provider

import (
	"errors"
	"fmt"
)

// Whether `sbx serve` on this machine holds a sandbox's egress filter in its own process, asked
// before any sandbox exists - the same shape as checkpointHost / CheckpointHostOK next door.
//
// Both forms of filter enforce the allow-list. Only the hosted one can hold a credential vault,
// because the vault holds a certificate authority's private key and the plaintext of every
// credential (internal/credvault explains the custody argument). So this is the one host fact
// `credentialProxy` turns on, and a caller should be told it at the create rather than find out
// from a sandbox that came back Failed.

// ErrFilterNeedsNativeDocker is the refusal for a filter the daemon holds itself.
//
// The filter is a listener `sbx serve` opens ON the sandbox's bridge gateway, and the daemon runs
// wherever you are - which on a VM-backed docker is not where the bridge is. Colima and Docker
// Desktop keep docker's bridges inside the Linux VM, as do rootless docker (its own netns) and a
// remote DOCKER_HOST (another machine). There the same filter runs as a container on the bridge
// instead, which is on the right side of that boundary by construction and cannot be trusted with
// a key: it sits on the sandbox's own network and is configured over an endpoint with no
// redaction. So `credentialProxy` is refused here rather than honoured in the weaker arrangement.
var ErrFilterNeedsNativeDocker = errors.New("`sbx serve` cannot hold this sandbox's egress " +
	"filter in its own process on this machine, so a credential vault here would keep the " +
	"sandbox's private key and every secret in a container on the sandbox's own bridge. That is " +
	"what a VM-backed docker looks like - colima and Docker Desktop keep docker's bridges inside " +
	"the VM, while `sbx serve` runs out here. Run the daemon on a host whose docker is native to " +
	"use credentialProxy, or create the sandbox without it. sbx doctor reports this as `hosted " +
	"egress filter`")

// HostsEgressFilters implements FilterHoster for docker.
//
// It asks about docker's OWN bridge, not a sandbox's: a sandbox's no-NAT bridge does not exist
// until the sandbox does, and the whole point of answering here is to answer before anything is
// created. They are two bridges of one engine, so either the engine's bridges are interfaces of
// this machine or none of them is - which is exactly the question. Where that guess is kind (a
// local engine's 172.17.0.1 bindable while DOCKER_HOST points somewhere else) it errs toward
// accepting, and refuseWithoutAHostedFilter then refuses on the provider's own report.
func (d *dockerProvider) HostsEgressFilters() error {
	gw, err := d.docker("network", "inspect", "bridge",
		"--format", "{{(index .IPAM.Config 0).Gateway}}")
	if err != nil || gw == "" {
		// No engine to judge, so no refusal: whatever asks next reports a dead daemon better
		// than a credential-vault message would, and nothing has been promised either way.
		return nil
	}

	return filterHost(gw)
}

// filterHost is the predicate: can this machine bind that bridge gateway? Same probe the create
// path makes per sandbox (bindable), worded for the caller that is about to refuse a vault.
func filterHost(gw string) error {
	if err := bindGateway(gw); err != nil {
		return fmt.Errorf("%w.\nThe gateway of docker's own bridge here is %s, which is not an "+
			"address this machine holds (%v) - check that with `ifconfig | grep %s`",
			ErrFilterNeedsNativeDocker, gw, err, gw)
	}

	return nil
}

// EgressFilterHostOK reports whether `sbx serve` would hold its filters itself on the docker
// engine this machine would use, before a provider is built. `sbx doctor` shows the same answer.
func EgressFilterHostOK() error {
	ep, err := resolveDockerHost("")
	if err != nil {
		return nil // no engine to judge; whatever asks next reports that better
	}

	return (&dockerProvider{endpoint: ep}).HostsEgressFilters()
}
