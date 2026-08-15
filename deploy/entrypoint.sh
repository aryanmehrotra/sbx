#!/bin/sh
# Turn the mounted service account into a kubeconfig, then run sbx.
#
# kubectl has no in-cluster mode of its own: given no kubeconfig it looks for one and fails,
# even inside a pod where the credentials are right there on disk. Writing them out is five
# lines and keeps the provider identical whether it runs on a laptop or in the cluster.
set -eu

SA=/var/run/secrets/kubernetes.io/serviceaccount

if [ -r "$SA/token" ]; then
  kubectl config set-cluster in-cluster \
    --server="https://${KUBERNETES_SERVICE_HOST}:${KUBERNETES_SERVICE_PORT}" \
    --certificate-authority="$SA/ca.crt" --embed-certs=true >/dev/null

  kubectl config set-credentials sa --token="$(cat "$SA/token")" >/dev/null

  kubectl config set-context in-cluster \
    --cluster=in-cluster --user=sa --namespace="$(cat "$SA/namespace")" >/dev/null

  kubectl config use-context in-cluster >/dev/null
fi

exec /usr/local/bin/sbx "$@"
