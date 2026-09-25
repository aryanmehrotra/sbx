package egress

import (
	_ "embed"
	"fmt"
	"strings"
)

// The sources the container filter is built from. Every one is a file of this package, compiled
// by its own tests here, and copied verbatim apart from the package clause.
var (
	//go:embed filter.go
	filterSource string

	//go:embed policy.go
	policySource string

	//go:embed control.go
	controlSource string

	//go:embed standalone_main.go.txt
	mainSource string
)

// GoVersion is the toolchain the generated context asks for. Kept beside the sources it builds
// so a go.mod bump and this move together.
const GoVersion = "1.26"

// BuildContext returns the files of a docker build context that compiles this package's filter
// into a standalone binary: name -> contents.
//
// The filter is copied verbatim apart from its package clause. That is the whole point of doing
// it this way rather than writing a small proxy for the container to run: there is one
// implementation of "which hosts may this sandbox reach", it is the one the unit tests and the
// fuzz targets cover, and a change to it cannot reach the host-side filter without reaching the
// container one in the same commit.
//
// builder and runtime are pinned images. They are arguments rather than constants so the pin
// lives with the other pins the repo checks, instead of in the middle of the daemon.
func BuildContext(builder, runtime string) (map[string]string, error) {
	if builder == "" || runtime == "" {
		return nil, fmt.Errorf("egress: build context needs both a builder and a runtime image")
	}

	files := map[string]string{
		"Dockerfile": fmt.Sprintf(`FROM %s AS build
WORKDIR /b
COPY go.mod *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /filter .

FROM %s
COPY --from=build /filter /filter
ENTRYPOINT ["/filter"]
`, builder, runtime),
		"go.mod":  "module sbxegress\n\ngo " + GoVersion + "\n",
		"main.go": mainSource,
	}

	for name, src := range map[string]string{
		"filter.go": filterSource, "policy.go": policySource, "control.go": controlSource,
	} {
		body := strings.Replace(src, "package egress\n", "package main\n", 1)
		if !strings.HasPrefix(body, "package main\n") {
			return nil, fmt.Errorf("egress: %s does not start with its package clause", name)
		}

		files[name] = body
	}

	return files, nil
}
