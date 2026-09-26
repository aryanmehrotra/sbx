package provider

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/execdctl"
	"github.com/aryanmehrotra/sbx/internal/fc"
)

// keyedImage is an image whose own ENV sets execd's secrets - as a snapshot committed from a
// sandbox does (ENV EXECD_ACCESS_TOKEN=), or a hostile image would.
type keyedImage struct{ tarEngine }

func (keyedImage) Inspect(context.Context, string) (fc.ImageConfig, error) {
	return fc.ImageConfig{ID: "sha256:" + strings.Repeat("cd", 32), Cmd: []string{"redis-server"},
		Env: []string{"PATH=/usr/bin", AgentTokenEnv + "=image-token", execdctl.EnvControlSecret + "=image-secret"}}, nil
}

// execd reads its token and control secret with getenv, which takes the FIRST occurrence. The
// image's ENV came first, so an image-chosen token was the one execd answered until the first
// re-key - and again after every cold boot - and a caller's EXECD_CONTROL_SECRET made the re-key
// itself fail. sbx's own values are now the only ones in a guest's environment.
func TestAVMBootsWithSbxsSecretsNotTheImagesOrTheCallers(t *testing.T) {
	r := newRig(t)
	ext := &initExt4{}
	r.p.ext4 = ext
	r.p.rootfs.Engine = keyedImage{}

	svc := redis
	svc.Env = map[string]string{"A": "1", execdctl.EnvControlSecret: "caller-secret"}

	ref := r.create(t, "keys", svc)
	vm := r.vm(t, ref)

	if len(ext.inits) != 1 {
		t.Fatalf("agent drives built: %d", len(ext.inits))
	}

	env := ext.inits[0].Env
	if got := envValues(env, AgentTokenEnv); !slices.Equal(got, []string{vm.AccessToken}) {
		t.Errorf("%s = %q, want only sbx's %q", AgentTokenEnv, got, vm.AccessToken)
	}

	if got := envValues(env, execdctl.EnvControlSecret); !slices.Equal(got, []string{vm.ControlSecret}) {
		t.Errorf("%s = %q, want only sbx's", execdctl.EnvControlSecret, got)
	}

	if got := envValues(env, "A"); !slices.Equal(got, []string{"1"}) {
		t.Errorf("an ordinary variable was lost: %q", got)
	}
}
