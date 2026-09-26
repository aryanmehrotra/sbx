package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// Pulling images before anybody needs them.
//
// The first `sbx create` on a fresh machine is a download, and the download is most of it.
// That is fine on a laptop and wrong in CI, where the pull lands inside the timed part of
// the job and looks like sbx being slow. `sbx prewarm` moves it to a step that can be cached.
//
// It reports what it did rather than a spinner, because the one question worth answering is
// which images were already there - an unchanged CI cache should print "already present" for
// everything, and a run that pulls when it should not is the cache being broken.

// Prewarm fetches images so a later create does not have to.
func Prewarm(ctx context.Context, p provider.Provider, w io.Writer, images []string) error {
	pl, err := provider.PullerFor(p)
	if err != nil {
		return err
	}

	if wm, ok := p.(provider.Warmer); ok {
		return warm(ctx, wm, w, images)
	}

	// Present-checking is a capability too, and a provider that can pull but cannot inspect
	// still works here - it just pulls, and docker's own pull is a no-op when the digest is
	// already local.
	has, _ := p.(provider.Builder)

	var (
		pulled  int
		already int
		failed  []string
	)

	for _, img := range images {
		if has != nil {
			if ok, err := has.HasImage(ctx, img); err == nil && ok {
				fmt.Fprintf(w, "  %-64s already present\n", img)

				already++

				continue
			}
		}

		start := time.Now()

		if err := pl.Pull(ctx, img); err != nil {
			fmt.Fprintf(w, "  %-64s FAILED\n", img)

			failed = append(failed, fmt.Sprintf("%s: %v", img, err))

			continue
		}

		fmt.Fprintf(w, "  %-64s pulled in %s\n", img, time.Since(start).Round(100*time.Millisecond))

		pulled++
	}

	fmt.Fprintf(w, "\n%d pulled, %d already present", pulled, already)

	if len(failed) > 0 {
		fmt.Fprintf(w, ", %d failed\n", len(failed))

		// Named, not counted. A prewarm that half-worked leaves a create to fail later on
		// exactly the image that could not be fetched, and the reason belongs here.
		for _, f := range failed {
			fmt.Fprintf(w, "  %s\n", f)
		}

		return fmt.Errorf("%d of %d images could not be pulled", len(failed), len(images))
	}

	fmt.Fprintln(w)

	return nil
}

// warm is Prewarm for a provider whose create needs more than the pull (provider.Warmer): on
// firecracker, the image's root filesystem, which for a large image is most of a first create.
func warm(ctx context.Context, wm provider.Warmer, w io.Writer, images []string) error {
	var (
		built   int
		already int
		failed  []string
	)

	for _, img := range images {
		start := time.Now()

		did, err := wm.Warm(ctx, img)

		switch {
		case err != nil:
			fmt.Fprintf(w, "  %-64s FAILED\n", img)

			failed = append(failed, fmt.Sprintf("%s: %v", img, err))
		case did:
			fmt.Fprintf(w, "  %-64s built in %s\n", img, time.Since(start).Round(100*time.Millisecond))

			built++
		default:
			fmt.Fprintf(w, "  %-64s already present\n", img)

			already++
		}
	}

	fmt.Fprintf(w, "\n%d built, %d already present", built, already)

	if len(failed) > 0 {
		fmt.Fprintf(w, ", %d failed\n", len(failed))

		for _, f := range failed {
			fmt.Fprintf(w, "  %s\n", f)
		}

		return fmt.Errorf("%d of %d images could not be prewarmed", len(failed), len(images))
	}

	fmt.Fprintln(w)

	return nil
}
