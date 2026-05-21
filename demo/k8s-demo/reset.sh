#!/usr/bin/env bash
# reset.sh — wipe all AgentRequests so the demo starts from a clean slate.
# Run this between demo attempts or after a stuck lock.
set -euo pipefail

GATEWAY_PORT="${GATEWAY_PORT:-18080}"
BASE="http://localhost:$GATEWAY_PORT"

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RESET='\033[0m'

echo -e "${YELLOW}Resetting demo state...${RESET}"

# Fetch all AgentRequests
REQUESTS=$(curl -sf "$BASE/agent-requests?namespace=default" 2>/dev/null || echo "[]")
COUNT=$(echo "$REQUESTS" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))" 2>/dev/null || echo 0)

if [[ "$COUNT" -eq 0 ]]; then
  echo -e "${GREEN}No AgentRequests to clean up.${RESET}"
  exit 0
fi

echo "Found $COUNT AgentRequest(s). Advancing stuck ones to Completed..."

export _RESET_BASE="$BASE"
echo "$REQUESTS" | python3 -c "
import sys, json, subprocess, os

requests = json.load(sys.stdin)
base = os.environ.get('_RESET_BASE', 'http://localhost:18080')

for req in requests:
    name = req['metadata']['name']
    phase = req.get('status', {}).get('phase', '')

    if phase in ('Completed', 'Failed', 'Denied', 'Expired'):
        print(f'  {name}: {phase} (already terminal, skipping)')
        continue

    print(f'  {name}: {phase} -> advancing to Completed')

    if phase == 'Pending':
        # Deny pending requests (no lock held yet)
        r = subprocess.run(
            ['curl', '-sf', '-X', 'POST',
             f'{base}/agent-requests/{name}/deny?namespace=default',
             '-H', 'Content-Type: application/json',
             '-d', '{\"reason\":\"reset by demo script\"}'],
            capture_output=True, text=True
        )
        print(f'    done' if r.returncode == 0
              else f'    deny failed: {r.stderr.strip() or r.stdout.strip()}')
        continue

    if phase == 'Approved':
        # Must go Approved -> Executing first to release the lock
        r = subprocess.run(
            ['curl', '-sf', '-X', 'POST',
             f'{base}/agent-requests/{name}/executing?namespace=default'],
            capture_output=True, text=True
        )
        if r.returncode != 0:
            print(f'    executing failed: {r.stderr.strip() or r.stdout.strip()}')
            continue

    r = subprocess.run(
        ['curl', '-sf', '-X', 'POST',
         f'{base}/agent-requests/{name}/completed?namespace=default',
         '-H', 'Content-Type: application/json',
         '-d', '{\"outcome\":\"success\",\"summary\":\"reset by demo script\"}'],
        capture_output=True, text=True
    )
    if r.returncode == 0:
        print(f'    done')
    else:
        print(f'    completed failed: {r.stderr.strip() or r.stdout.strip()}')
"

echo -e "${GREEN}Reset complete. Demo is ready.${RESET}"
