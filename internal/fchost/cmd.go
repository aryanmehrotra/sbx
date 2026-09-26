package fchost

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aryanmehrotra/sbx/internal/osb"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// Usage is `sbx fc`'s help.
const Usage = `sbx fc - the Firecracker microVM backend on hosts without /dev/kvm

  sbx fc backend                 which backend a microVM would use here, and why
  sbx fc vm status [--json]      the helper VM and the daemon inside it
  sbx fc vm start [--cpus N] [--memory GiB] [--disk GiB]
                                 create it on first use, start it, install this sbx, start its daemon
  sbx fc vm stop                 stop it; its disk and every microVM sandbox in it stay
  sbx fc vm rm --yes             delete it, and every microVM sandbox in it

  sizing also reads SBX_FC_VM_CPUS / SBX_FC_VM_MEMORY / SBX_FC_VM_DISK (default 2 / 2 / 20)
  SBX_FC_VM_DRIVER=lima|colima picks the tool on macOS when both are installed
`

// Main is `sbx fc ...`.
func Main(version string, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(Usage)

		return nil
	}

	switch args[0] {
	case "backend":
		b := HostBackend()
		fmt.Printf("%s", b.Kind)

		if b.Helper != "" {
			fmt.Printf(" (%s)", b.Helper)
		}

		fmt.Printf("\n  %s\n", b.Reason)

		if b.Next != "" {
			fmt.Printf("  next: %s\n", b.Next)
		}

		return nil
	case "vm":
		return vmMain(version, args[1:])
	case "call":
		// The far side of Remote (remote.go), run inside the helper VM. Not in Usage: it is
		// sbx talking to itself.
		p, err := provider.For(Firecracker, "", "")
		if err != nil {
			return err
		}

		err = Call(context.Background(), p, args[1:], os.Stdin, os.Stdout, os.Stderr)

		var ee *provider.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.Code)
		}

		return err
	default:
		return fmt.Errorf("unknown `sbx fc %s`\n\n%s", args[0], Usage)
	}
}

func helperManager(out io.Writer) (*Manager, error) {
	b := HostBackend()

	switch b.Kind {
	case HelperVM:
		return NewManager(b.Helper, out)
	case Direct:
		return nil, errors.New("this host has /dev/kvm and runs Firecracker directly; there is no helper VM to manage")
	default:
		return nil, Refusal(b)
	}
}

func signalled() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func vmMain(version string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("sbx fc vm needs a verb: status, start, stop or rm\n\n%s", Usage)
	}

	verb := args[0]

	fs := flag.NewFlagSet("fc vm "+verb, flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable")
	cpus := fs.Int("cpus", 0, "CPUs for the helper VM (on create; default $SBX_FC_VM_CPUS or 2)")
	mem := fs.Float64("memory", 0, "GiB of memory (default $SBX_FC_VM_MEMORY or 2)")
	disk := fs.Int("disk", 0, "GiB of disk (default $SBX_FC_VM_DISK or 20)")
	yes := fs.Bool("yes", false, "confirm `rm`, which deletes every microVM sandbox in the VM")

	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	m, err := helperManager(os.Stderr)
	if err != nil {
		return err
	}

	if *cpus > 0 {
		m.Config.CPUs = *cpus
	}

	if *mem > 0 {
		m.Config.MemoryGiB = *mem
	}

	if *disk > 0 {
		m.Config.DiskGiB = *disk
	}

	if err := m.Config.Validate(); err != nil {
		return err
	}

	ctx, stop := signalled()
	defer stop()

	switch verb {
	case "status":
		r, err := m.Report(ctx)
		if err != nil {
			return err
		}

		if *asJSON {
			fmt.Println(r.JSON())

			return nil
		}

		fmt.Printf("%s (%s): %s\n", r.Name, r.Driver, r.State)

		if r.State == Running {
			fmt.Printf("  sbx serve --provider firecracker inside: %s\n", orUnknown(r.Daemon))
		}

		return nil
	case "start":
		ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()

		if err := m.Ensure(ctx, EnsureOptions{Version: version}); err != nil {
			return err
		}

		m.say("%s is running with sbx serve --provider firecracker inside; "+
			"run `sbx serve --provider firecracker` here to reach its sandboxes", m.Config.Name)

		return nil
	case "stop":
		return m.Stop(ctx)
	case "rm":
		if !*yes {
			return fmt.Errorf("`sbx fc vm rm` deletes %s and every microVM sandbox in it; add --yes", m.Config.Name)
		}

		return m.Remove(ctx)
	default:
		return fmt.Errorf("unknown `sbx fc vm %s`: want status, start, stop or rm", verb)
	}
}

// stringList is a repeatable flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)

	return nil
}

// ServeMain is `sbx serve --provider firecracker` on a helper-VM host. It takes the flags of
// `sbx serve` that mean something here and passes the daemon's own on to the one in the VM.
func ServeMain(version string, args []string) error {
	opt, err := parseServe(args, os.Getenv, osb.DefaultStateDir)
	if err != nil {
		return err
	}

	opt.Version = version

	m, err := helperManager(os.Stdout)
	if err != nil {
		return err
	}

	ctx, stop := signalled()
	defer stop()

	return m.Front(ctx, opt)
}

// parseServe is ServeMain's flags as FrontOptions. It touches no VM - which is what lets a test
// run it on a machine that has one to start - and nothing but keyDir's key file.
func parseServe(args []string, getenv func(string) string, keyDir func() (string, error)) (FrontOptions, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	_ = fs.String("provider", Firecracker, "firecracker")
	idle := fs.String("idle", "", "sleep a service after this long with no bytes (passed to the VM's daemon)")
	ready := fs.String("ready", "", "give up waking a service after this long (passed on)")
	refresh := fs.String("refresh", "", "how often the VM's daemon looks for sandboxes (passed on)")
	osbAddr := fs.String("osb-addr", getenv("SBX_OSB_ADDR"), "serve the OpenSandbox API here, proxied into the VM")
	osbKey := fs.String("osb-key", "", "require this OPEN-SANDBOX-API-KEY (default $SBX_OSB_KEY)")
	hostPaths := fs.String("osb-host-paths", getenv("SBX_OSB_HOST_PATHS"), "passed on; paths as the VM sees them")

	noJailer := fs.Bool("osb-insecure-no-jailer", false, "passed on: serve the API with SBX_FC_JAILER=off in the VM (unconfined root VMMs)")

	// Defined to be refused by name rather than as unknown flags: each is a thing a person could
	// reasonably type here, and "flag provided but not defined" does not say why it is not.
	noKey := fs.Bool("osb-insecure-no-key", false, "refused on the helper-VM path")
	poolFreeze := fs.Bool("osb-pool-freeze", false, "refused on the helper-VM path")

	var only, pools stringList
	fs.Var(&only, "only", "passed on to the VM's daemon")
	fs.Var(&pools, "osb-pool", "refused on the helper-VM path")

	if err := fs.Parse(args); err != nil {
		return FrontOptions{}, err
	}

	switch {
	case *noKey:
		// The API reaches this machine as an ssh forward on its loopback, and containers on a
		// VM-backed engine (colima, Docker Desktop) reach this loopback: keyless, any of them could
		// drive every API sandbox in the VM.
		return FrontOptions{}, errors.New("--osb-insecure-no-key is refused on the helper VM path: the API " +
			"is forwarded to this machine's loopback, which every container on a VM-backed engine can reach. " +
			"Drop it - a key is generated into ~/.sbx/osb/key, where sbx mcp and the SDKs find it")
	case len(pools) > 0 || *poolFreeze || strings.TrimSpace(getenv("SBX_OSB_POOL")) != "":
		return FrontOptions{}, errors.New("--osb-pool / --osb-pool-freeze / SBX_OSB_POOL: the warm pool is not " +
			"carried into the helper VM yet, and it is refused rather than dropped. Serve without it (creates are " +
			"cold), or run the pool on a Linux host with /dev/kvm")
	}

	var pass []string

	keyNote := "key from --osb-key / SBX_OSB_KEY"

	if *osbAddr != "" {
		if *osbKey == "" {
			*osbKey = getenv("SBX_OSB_KEY")
		}

		// As on Linux: a key the operator gave, else one generated once and kept 0600 - here, on
		// the machine the API is served on, where the clients that read ~/.sbx/osb/key run. The
		// in-VM daemon gets it through its root-only environment file, never a command line.
		if *osbKey == "" {
			dir, err := keyDir()
			if err != nil {
				return FrontOptions{}, err
			}

			key, path, _, err := osb.LoadOrCreateKey(dir)
			if err != nil {
				return FrontOptions{}, err
			}

			// Where, never what: a key in a log is a key in every log shipper downstream of it.
			keyNote = "key in " + path

			*osbKey = key
		}

		if *noJailer {
			pass = append(pass, "--osb-insecure-no-jailer")
		}
	}

	for _, f := range []struct{ name, val string }{
		{"idle", *idle}, {"ready", *ready}, {"refresh", *refresh}, {"osb-host-paths", *hostPaths},
	} {
		if f.val != "" {
			pass = append(pass, "--"+f.name, f.val)
		}
	}

	for _, o := range only {
		pass = append(pass, "--only", o)
	}

	return FrontOptions{
		EnsureOptions: EnsureOptions{OSBKey: *osbKey, Serve: pass},
		OSBAddr:       *osbAddr,
		keyNote:       keyNote,
	}, nil
}
