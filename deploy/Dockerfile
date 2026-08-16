# The activator image.
#
# It carries kubectl because the Kubernetes provider shells out to it - the same choice the
# Docker provider makes, and for the same reason: a failing command can be copied out of a
# log and run by hand. The cost is an apk package rather than a client library and the API
# machinery behind it, which for a binary whose whole argument is that it stays small is the
# trade worth making.
FROM alpine:3.20

RUN apk add --no-cache kubectl

# kubectl does not read in-cluster credentials on its own, so the entrypoint writes a
# kubeconfig from the mounted service account before handing over.
COPY entrypoint.sh /entrypoint.sh
COPY sbx /usr/local/bin/sbx

ENTRYPOINT ["/entrypoint.sh"]
