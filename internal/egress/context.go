package egress

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed filter.go
var filterSource string

//go:embed standalone_main.go.txt
var mainSource string

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

	filter := strings.Replace(filterSource, "package egress\n", "package main\n", 1)
	if !strings.HasPrefix(filter, "package main\n") {
		return nil, fmt.Errorf("egress: filter.go does not start with its package clause")
	}

	dockerfile := fmt.Sprintf(`FROM %s AS build
WORKDIR /b
COPY go.mod filter.go main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /filter .

FROM %s
COPY --from=build /filter /filter
ENTRYPOINT ["/filter"]
`, builder, runtime)

	return map[string]string{
		"Dockerfile": dockerfile,
		"go.mod":     "module sbxegress\n\ngo " + GoVersion + "\n",
		"filter.go":  filter,
		"main.go":    mainSource,
	}, nil
}
