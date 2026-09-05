#!/usr/bin/env sh
set -eu

payload="$(mktemp)"
prompt="$(mktemp)"
trap 'rm -f "$payload" "$prompt"' EXIT

cat >"$payload"

force_mode="${COPILOT_WORKER_FORCE:-0}"

if [ "$force_mode" = "1" ]; then
  cat >"$prompt" <<EOF
You are an on-call alert copilot. Do read-only attribution from the alert context below.

Rules when force mode is on:
- Read-only commands only. No writes.
- Allowed examples: local log query scripts, git log / git diff in a local checkout.
- Forbidden: kubectl apply/delete/patch, git push, changing Grafana/Nacos/databases, mutating HTTP calls.
- Do not print secrets, tokens, AccessKeys, or full environment dumps.

Output for a chat thread:
- Keep it short.
- Organize as Facts / Judgement / Next steps / References.
- Mark each fact with its evidence source.
- If evidence is missing, write "unknown". Do not invent.

Alert context JSON:
$(cat "$payload")
EOF
else
  cat >"$prompt" <<EOF
You are an on-call alert copilot. Do read-only attribution from the alert context below.

Requirements:
- Analysis only. Do not execute shell commands.
- Keep the reply short and structured as Facts / Judgement / Next steps / References.
- Do not print secrets, tokens, AccessKeys, or full environment dumps.
- If evidence is missing, write "unknown". Do not invent.

Alert context JSON:
$(cat "$payload")
EOF
fi

cursor_bin="${CURSOR_AGENT_BIN:-cursor-agent}"
workspace="${COPILOT_WORKSPACE:-.}"
model="${COPILOT_MODEL:-}"

if ! command -v "$cursor_bin" >/dev/null 2>&1; then
  echo "cursor-agent not found: $cursor_bin" >&2
  exit 127
fi

set -- --print --mode ask --trust --workspace "$workspace"
if [ "$force_mode" = "1" ]; then
  set -- "$@" --force
fi
if [ -n "$model" ]; then
  set -- "$@" --model "$model"
fi
set -- "$@" "$(cat "$prompt")"

"$cursor_bin" "$@"
