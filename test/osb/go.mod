// The OpenSandbox conformance harness and benchmark. A module of its own so the root go.mod
// stays dependency-free: the upstream SDK is allowed here and nowhere else (see the design,
// docs/superpowers/specs/2026-09-25-opensandbox-compat-design.md).
module github.com/aryanmehrotra/sbx/test/osb

go 1.24

require github.com/alibaba/OpenSandbox/sdks/sandbox/go v1.1.0

require golang.org/x/sync v0.7.0 // indirect
