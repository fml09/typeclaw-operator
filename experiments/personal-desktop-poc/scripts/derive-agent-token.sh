#!/bin/sh
set -eu

: "${OWNER_ISSUER:?set OWNER_ISSUER to the exact OIDC issuer}"
: "${OWNER_SUBJECT:?set OWNER_SUBJECT to the stable OIDC subject}"
: "${TYPECLAW_INSTANCE_UID:?set TYPECLAW_INSTANCE_UID to the exact TypeClawInstance UID}"
: "${POC_AGENT_TOKEN_KEY:?set POC_AGENT_TOKEN_KEY to the Gateway signing key}"

if [ "${#POC_AGENT_TOKEN_KEY}" -lt 32 ]; then
  echo "POC_AGENT_TOKEN_KEY must contain at least 32 bytes" >&2
  exit 2
fi

for value in "$OWNER_ISSUER" "$OWNER_SUBJECT" "$TYPECLAW_INSTANCE_UID" "$POC_AGENT_TOKEN_KEY"; do
  cleaned=$(printf '%s' "$value" | tr -d '\r\n')
  if [ "$cleaned" != "$value" ]; then
    echo "identity values and POC_AGENT_TOKEN_KEY must each be a single line" >&2
    exit 2
  fi
done

printf 'agent-v1\n%s\n%s\n%s\n' "$OWNER_ISSUER" "$OWNER_SUBJECT" "$TYPECLAW_INSTANCE_UID" \
  | openssl dgst -sha256 -hmac "$POC_AGENT_TOKEN_KEY" -binary \
  | od -An -tx1 \
  | tr -d ' \n'
printf '\n'
