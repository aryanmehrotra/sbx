# A throwaway service that answers one question: does this platform's ingress pass a WebSocket
# upgrade through to the container?
#
# It is sbx carrying one local TCP port - the same code path a real database would use - with a
# netcat echo behind it instead of a database. If `sbx connect` reaches this and bytes come
# back, the transport works on that platform and the only thing left to change is what is
# listening on the port.

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/sbx .

FROM alpine:3.20
RUN apk add --no-cache socat
COPY --from=build /out/sbx /usr/local/bin/sbx

ENV PORT=8080

# socat is the workload; sbx carries it. Both in one container because that is the shape the
# real thing has: a platform gives you one container and one HTTP port.
ENTRYPOINT ["/bin/sh", "-c", "socat TCP-LISTEN:7777,fork,reuseaddr EXEC:/bin/cat & exec sbx serve --connect-addr=:${PORT} --behind-proxy --front=echo=7777"]
