#!/bin/sh
# Starts the server for the e2e run. It first removes the bundled apps, so the run proves the server installs
# them itself at startup rather than relying on anything the test setup did.
BIN="$(dirname "$0")/../../backend/bin/servicelater"
"$BIN" app uninstall incident-management core-itsm >/dev/null 2>&1 || true
exec "$BIN"
