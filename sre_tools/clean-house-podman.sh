#!/usr/bin/env bash
# reset-podman-fresh.sh
# WARNING: This script DELETES ALL Podman data on your system!
# Use only if you really want a complete fresh start.

set -euo pipefail

echo "=== WARNING: This will DELETE EVERYTHING in Podman ==="
echo "Containers • Pods • Images • Networks • Volumes • Build cache"
echo ""
echo "Type YES (all caps) and press Enter to continue, or Ctrl+C to abort."
read -r confirmation

if [[ "$confirmation" != "YES" ]]; then
    echo "Aborted. Nothing was deleted."
    exit 0
fi

echo ""
echo "Starting full Podman reset..."

# 1. Stop and remove all running / stopped containers
echo "→ Stopping and removing all containers..."
podman stop --all --time 10 2>/dev/null || true
podman rm --all --force --volumes 2>/dev/null || true

# 2. Remove all pods (including any lingering ones)
echo "→ Removing all pods..."
podman pod rm --all --force 2>/dev/null || true

# 3. Prune everything unused (containers, images, networks, dangling volumes, build cache)
echo "→ Pruning system (containers, images, networks, cache)..."
podman system prune --all --force --volumes 2>/dev/null || true

# 4. Force-remove any remaining images (very aggressive)
echo "→ Removing all images..."
podman rmi --all --force 2>/dev/null || true

# 5. Prune volumes again (in case any survived)
echo "→ Pruning volumes..."
podman volume prune --force 2>/dev/null || true
podman volume rm --all --force 2>/dev/null || true

# 6. Clean networks
echo "→ Pruning networks..."
podman network prune --force 2>/dev/null || true

# 7. (Optional but powerful) Full nuclear reset if prune didn't catch everything
#    WARNING: This also removes the storage directories → use only if above fails
#    Uncomment the next block only if you're still seeing issues after step 6
# echo "→ Performing full system reset (deletes storage dirs)..."
# podman system reset --force 2>/dev/null || true
#    Note: On some older Podman versions this command may not exist — skip if it errors.

echo ""
echo "=== Podman reset complete ==="
echo "Your environment is now completely fresh."
echo ""

# Quick status checks
echo "Current status:"
podman ps -a          # should show nothing
podman images         # should be empty
podman volume ls      # should be empty
podman network ls     # should show only default ones (podman, bridge, etc.)
podman pod ls         # should be empty

echo ""
echo "You can now re-run your UniFi start script (./start-unifi-new.sh)."
echo "It will pull fresh images and create new volumes."
echo "If you see port conflicts again → check with: sudo ss -tuln | grep :8080"
echo "Done!"