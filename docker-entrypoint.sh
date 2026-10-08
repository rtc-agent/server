#!/bin/sh
set -e

# Docker entrypoint for admin-server
# Automatically generates JWT keys if they don't exist

KEYS_DIR="/app/etc/keys"
PRIVATE_KEY="${KEYS_DIR}/admin-private.pem"
PUBLIC_KEY="${KEYS_DIR}/admin-public.pem"

# Check if keys exist
if [ ! -f "${PRIVATE_KEY}" ] || [ ! -f "${PUBLIC_KEY}" ]; then
    echo "JWT keys not found, generating new key pair..."
    ./rtc-agent admin keygen --output-dir "${KEYS_DIR}" --algorithm RS256
    echo "JWT keys generated successfully."
else
    echo "JWT keys found at ${KEYS_DIR}"
fi

# Execute the main command
exec "$@"
