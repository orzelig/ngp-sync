#!/usr/bin/env bash
# Renamed: the browser is NGP.app now, so the launcher is scripts/ngp.sh.
# This compatibility shim forwards everything there.
exec "$(dirname "$0")/ngp.sh" "$@"
