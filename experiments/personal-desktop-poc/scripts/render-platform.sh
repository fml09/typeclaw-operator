#!/bin/sh
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
template_dir="$script_dir/../manifests"

: "${DESKTOP_NAMESPACE:=personal-desktop-poc}"
: "${GATEWAY_IMAGE:?set GATEWAY_IMAGE to the pushed gateway image}"
: "${TYPECLAW_INSTANCE_UID:?set TYPECLAW_INSTANCE_UID to the target TypeClawInstance UID}"
: "${ALLOW_INSECURE_DEV_AUTH:=false}"
: "${GOLDEN_IMAGE_NAME:=ubuntu-2404-cloud-golden}"
: "${GOLDEN_IMAGE_URL:=https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img}"
: "${GOLDEN_DISK_SIZE:=32Gi}"
: "${STORAGE_CLASS:=longhorn}"

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

require_safe_value() {
  value=$1
  label=$2
  case "$value" in
    ''|*[!A-Za-z0-9._/:@+=-]*) echo "$label contains unsupported template characters" >&2; exit 2 ;;
  esac
}

require_quantity() {
  value=$1
  label=$2
  require_safe_value "$value" "$label"
  case "$value" in
    *[0-9]*) ;;
    *) echo "$label must contain a numeric Kubernetes quantity" >&2; exit 2 ;;
  esac
}

render() {
  sed \
    -e "s|\${DESKTOP_NAMESPACE}|$DESKTOP_NAMESPACE|g" \
    -e "s|\${GATEWAY_IMAGE}|$GATEWAY_IMAGE|g" \
    -e "s|\${TYPECLAW_INSTANCE_UID}|$TYPECLAW_INSTANCE_UID|g" \
    -e "s|\${ALLOW_INSECURE_DEV_AUTH}|$ALLOW_INSECURE_DEV_AUTH|g" \
    -e "s|\${GOLDEN_IMAGE_NAME}|$GOLDEN_IMAGE_NAME|g" \
    -e "s|\${GOLDEN_IMAGE_URL}|$GOLDEN_IMAGE_URL|g" \
    -e "s|\${GOLDEN_DISK_SIZE}|$GOLDEN_DISK_SIZE|g" \
    -e "s|\${STORAGE_CLASS}|$STORAGE_CLASS|g" \
    "$1"
}

require_dns_label "$DESKTOP_NAMESPACE" DESKTOP_NAMESPACE
require_dns_label "$GOLDEN_IMAGE_NAME" GOLDEN_IMAGE_NAME
require_dns_subdomain "$STORAGE_CLASS" STORAGE_CLASS
require_safe_value "$GATEWAY_IMAGE" GATEWAY_IMAGE
require_safe_value "$TYPECLAW_INSTANCE_UID" TYPECLAW_INSTANCE_UID
require_safe_value "$GOLDEN_IMAGE_URL" GOLDEN_IMAGE_URL
require_quantity "$GOLDEN_DISK_SIZE" GOLDEN_DISK_SIZE
case "$ALLOW_INSECURE_DEV_AUTH" in true|false) ;; *) echo "ALLOW_INSECURE_DEV_AUTH must be true or false" >&2; exit 2 ;; esac

render "$template_dir/platform.yaml.tmpl"
printf '%s\n' '---'
render "$template_dir/golden-image.yaml.tmpl"
