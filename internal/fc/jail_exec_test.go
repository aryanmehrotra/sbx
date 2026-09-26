package fc

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/fc/fcfake"
)

// fakeJailerEnv makes this test binary a jailer: it records its argv, does what v1.17's jailer
// leaves behind that sbx relies on - the API socket bound at /api.sock of the chroot it computes
// from its own flags, the console on the stdout it inherited - and serves fcfake there until
// killed. No root, no chroot, no KVM: what it proves is sbx's half of the contract.
const fakeJailerEnv = "SBX_FC_FAKE_JAILER_OUT"

func TestMain(m *testing.M) {
	if out := os.Getenv(fakeJailerEnv); out != "" {
		fakeJailer(out)
		return
	}

	// Exec'd by the REAL jailer as its "firecracker" (TestTheRealJailer): the jailer clears the
	// environment, so this mode is told by argv - the jailer always passes --id first. After the fake jailer: its argv starts --id too.
	if len(os.Args) > 1 && os.Args[1] == "--id" {
		jailedFirecracker()
		return
	}

	os.Exit(m.Run())
}

func fakeJailer(out string) {
	args := os.Args[1:]
	_ = os.WriteFile(out, []byte(strings.Join(args, "\x00")), 0o600)

	flag := func(name string) string {
		if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
			return args[i+1]
		}

		return ""
	}

	exe, _ := filepath.EvalSymlinks(flag("--exec-file"))
	root := filepath.Join(flag("--chroot-base-dir"), filepath.Base(exe), flag("--id"), "root")

	// Relative, from inside the root: what the jailed VMM's "/api.sock" is, and short enough
	// for macOS's 104-byte socket paths whatever the root's own length.
	if err := os.Chdir(root); err != nil {
		os.Exit(3)
	}

	if _, err := fcfake.Start(APISockName); err != nil {
		os.Exit(4)
	}

	os.Stdout.WriteString("jailed console\n")

	select {}
}

// jailedFirecracker is this test binary run by the real jailer in firecracker's place: it binds
// the API socket it was given - inside the chroot - and says on its console who and where it is.
func jailedFirecracker() {
	sock := ""
	if i := slices.Index(os.Args, "--api-sock"); i >= 0 && i+1 < len(os.Args) {
		sock = os.Args[i+1]
	}

	_, hostEtc := os.Stat("/etc/passwd")
	os.Stdout.WriteString("jailed firecracker uid=" + strconv.Itoa(os.Getuid()) + " gid=" + strconv.Itoa(os.Getgid()) +
		" host-etc=" + strconv.FormatBool(hostEtc == nil) + "\n")

	if _, err := fcfake.Start(sock); err != nil {
		os.Stderr.WriteString("jailed firecracker: " + err.Error() + "\n")
		os.Exit(2)
	}

	select {}
}
