# Deploying sbx as a service, for a platform that takes a repository.
#
# `sbx serve` fronts sandboxes on the machine it runs on, so this image is only useful where
# it can reach a container runtime: a VM with the docker socket mounted, or a cluster where
# deploy/activator.yaml gives it a ServiceAccount. A platform that runs your container with no
# socket and no volume mounts will start this and watch it exit, saying which sockets it looked
# at - which is the correct outcome, and better than a daemon that comes up fronting nothing.
#
# deploy/Dockerfile is the cluster image and carries kubectl. This one is the plain one.

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/sbx .

FROM alpine:3.20
COPY --from=build /out/sbx /usr/local/bin/sbx

# The tunnel endpoint, on whatever port the platform hands us. It refuses to start without
# SBX_CONNECT_TOKEN, and --behind-proxy is the statement that something in front terminates
# TLS - which is true of every platform that gives you one HTTP port.
ENV PORT=8080
EXPOSE 8080

ENTRYPOINT ["/bin/sh", "-c", "exec sbx serve --connect-addr=:${PORT} --behind-proxy \"$@\"", "--"]
