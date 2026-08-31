#!/bin/sh
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
template="$script_dir/../manifests/personal-desktop.yaml.tmpl"

: "${OWNER_ISSUER:?set OWNER_ISSUER to the exact OIDC issuer}"
: "${OWNER_SUBJECT:?set OWNER_SUBJECT to the stable OIDC subject}"
: "${TYPECLAW_INSTANCE_UID:?set TYPECLAW_INSTANCE_UID to the exact TypeClawInstance UID}"
: "${OWNER_HASH_KEY:?set OWNER_HASH_KEY to the same secret used by the gateway}"
: "${DESKTOP_NAMESPACE:=personal-desktop-poc}"
: "${GOLDEN_IMAGE_NAME:=ubuntu-2404-cloud-golden}"
: "${STORAGE_CLASS:=longhorn}"
: "${DESKTOP_DISK_SIZE:=32Gi}"
: "${DESKTOP_CPU_CORES:=4}"
: "${DESKTOP_MEMORY:=4Gi}"
: "${DESKTOP_NODE_NAME:?set DESKTOP_NODE_NAME to a KVM-capable Kubernetes node}"
: "${DESKTOP_SSH_AUTHORIZED_KEY:?set DESKTOP_SSH_AUTHORIZED_KEY to a dedicated ed25519 public key}"

if [ "${#OWNER_HASH_KEY}" -lt 32 ]; then
  echo "OWNER_HASH_KEY must contain at least 32 bytes" >&2
  exit 2
fi

require_single_line() {
  value=$1
  label=$2
  cleaned=$(printf '%s' "$value" | tr -d '\r\n')
  if [ "$cleaned" != "$value" ]; then
    echo "$label must be a single-line exact identity value" >&2
    exit 2
  fi
}

require_dns_label() {
  value=$1
  label=$2
  case "$value" in
    ''|*[!a-z0-9-]*|-*|*-) echo "$label must be a lowercase DNS label" >&2; exit 2 ;;
  esac
  if [ "${#value}" -gt 63 ]; then
    echo "$label must contain at most 63 characters" >&2
    exit 2
  fi
}

require_dns_subdomain() {
  value=$1
  label=$2
  case "$value" in
    ''|*[!a-z0-9.-]*|.*|*.|*..*) echo "$label must be a lowercase DNS subdomain" >&2; exit 2 ;;
  esac
  if [ "${#value}" -gt 253 ]; then
    echo "$label must contain at most 253 characters" >&2
    exit 2
  fi
  remaining=$value
  while [ -n "$remaining" ]; do
    case "$remaining" in
      *.*) component=${remaining%%.*}; remaining=${remaining#*.} ;;
      *) component=$remaining; remaining='' ;;
    esac
    require_dns_label "$component" "$label component"
  done
}

require_quantity() {
  value=$1
  label=$2
  case "$value" in
    ''|*[!0-9A-Za-z.]*) echo "$label must be a Kubernetes-style quantity" >&2; exit 2 ;;
  esac
  case "$value" in
    *[0-9]*) ;;
    *) echo "$label must contain a numeric Kubernetes quantity" >&2; exit 2 ;;
  esac
}

require_dns_label "$DESKTOP_NAMESPACE" DESKTOP_NAMESPACE
require_dns_label "$GOLDEN_IMAGE_NAME" GOLDEN_IMAGE_NAME
require_dns_subdomain "$STORAGE_CLASS" STORAGE_CLASS
require_single_line "$OWNER_ISSUER" OWNER_ISSUER
require_single_line "$OWNER_SUBJECT" OWNER_SUBJECT
require_single_line "$TYPECLAW_INSTANCE_UID" TYPECLAW_INSTANCE_UID
require_single_line "$DESKTOP_SSH_AUTHORIZED_KEY" DESKTOP_SSH_AUTHORIZED_KEY
require_quantity "$DESKTOP_DISK_SIZE" DESKTOP_DISK_SIZE
require_quantity "$DESKTOP_MEMORY" DESKTOP_MEMORY
require_dns_subdomain "$DESKTOP_NODE_NAME" DESKTOP_NODE_NAME
if ! printf '%s\n' "$DESKTOP_SSH_AUTHORIZED_KEY" | grep -Eq '^ssh-ed25519 [A-Za-z0-9+/=]+ typeclaw-desktop-poc$'; then
  echo "DESKTOP_SSH_AUTHORIZED_KEY must be a dedicated ed25519 key ending in typeclaw-desktop-poc" >&2
  exit 2
fi
case "$DESKTOP_CPU_CORES" in ''|*[!0-9]*) echo "DESKTOP_CPU_CORES must be an integer" >&2; exit 2 ;; esac
case "$DESKTOP_CPU_CORES" in 0|00*) echo "DESKTOP_CPU_CORES must be greater than zero" >&2; exit 2 ;; esac

owner_key=$(printf 'v1\n%s\n%s\n%s\n' "$OWNER_ISSUER" "$OWNER_SUBJECT" "$TYPECLAW_INSTANCE_UID" \
  | openssl dgst -sha256 -hmac "$OWNER_HASH_KEY" -binary \
  | od -An -tx1 \
  | tr -d ' \n' \
  | cut -c1-20)

if [ "${#owner_key}" -ne 20 ]; then
  echo "failed to derive the desktop owner key" >&2
  exit 2
fi

DESKTOP_NAME="pd-$owner_key"

sed \
  -e "s|\${DESKTOP_NAMESPACE}|$DESKTOP_NAMESPACE|g" \
  -e "s|\${DESKTOP_NAME}|$DESKTOP_NAME|g" \
  -e "s|\${OWNER_KEY}|$owner_key|g" \
  -e "s|\${GOLDEN_IMAGE_NAME}|$GOLDEN_IMAGE_NAME|g" \
  -e "s|\${STORAGE_CLASS}|$STORAGE_CLASS|g" \
  -e "s|\${DESKTOP_DISK_SIZE}|$DESKTOP_DISK_SIZE|g" \
  -e "s|\${DESKTOP_CPU_CORES}|$DESKTOP_CPU_CORES|g" \
  -e "s|\${DESKTOP_MEMORY}|$DESKTOP_MEMORY|g" \
  -e "s|\${DESKTOP_NODE_NAME}|$DESKTOP_NODE_NAME|g" \
  -e "s|\${DESKTOP_SSH_AUTHORIZED_KEY}|$DESKTOP_SSH_AUTHORIZED_KEY|g" \
  "$template"
