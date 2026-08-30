# The activator image.
#
# It carries kubectl because the Kubernetes provider shells out to it - the same choice the
# Docker provider makes, and for the same reason: a failing command can be copied out of a
# log and run by hand. The cost is an apk package rather than a client library and the API
# machinery behind it, which for a binary whose whole argument is that it stays small is the
# trade worth making.
#
# Build it with deploy/ as the CONTEXT, not the repo root - it copies entrypoint.sh and sbx
# from beside itself, and the binary has to be cross-compiled for the cluster first:
#
#	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o deploy/sbx .
#	docker build -t sbx-activator:dev deploy/
#
# activator.yaml asks for sbx-activator:dev with imagePullPolicy: IfNotPresent, so on minikube
# build it inside the cluster's docker (eval $(minikube docker-env)) and nothing has to be
# pushed anywhere. Dockerfile.release is the multi-arch one CI publishes.
FROM alpine:3.20

RUN apk add --no-cache kubectl

# kubectl does not read in-cluster credentials on its own, so the entrypoint writes a
# kubeconfig from the mounted service account before handing over.
COPY entrypoint.sh /entrypoint.sh
COPY sbx /usr/local/bin/sbx

ENTRYPOINT ["/entrypoint.sh"]
