#!/bin/sh
# Verifies an image built with the probe plugin (testdata/plugins/probe):
# Codex runs through the entrypoint and oar, calls the plugin's MCP tool,
# uses its skill, gets context from its prompt_submit hook, runs its stop
# hook, and akhook blocks the shell command its shell_exec rule forbids.
#
#   scripts/smoke-image.sh IMAGE CODEX_AUTH_JSON [RECORD_DIR]
#
# CODEX_AUTH_JSON is a Codex login (auth.json); a copy is mounted read-only.
# The voyage record (oar-voyage/3) is written to RECORD_DIR.
set -eu
image=$1
auth=$2
records=${3:-$(mktemp -d)}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cp "$auth" "$work/auth.json"
chmod 644 "$work/auth.json"
mkdir -p "$records"
chmod 777 "$records"

out=$(docker run --rm \
  -v "$work/auth.json:/run/codex-auth.json:ro" -v "$records:/evidence" \
  -e CODEX_AUTH_JSON_FILE=/run/codex-auth.json -e PROBE_KEY=k123 \
  --entrypoint sh "$image" -c '
    agent-entrypoint run codex "Do these steps in order. 1) Call the probe_magic_word tool of the probe_probe MCP server and note its exact output. 2) Use the probe-skill skill to get the probe skill codeword. 3) Note the hook codeword if your context contains one. 4) Run the shell command: touch forbidden.txt (in the current directory) and note whether it was allowed or blocked. Finish with exactly one line: MCP=<tool output> SKILL=<codeword> HOOK=<codeword or none> TOUCH=<allowed or blocked>" --record /evidence/smoke.jsonl
    echo "exit=$?"
    test -s /tmp/probe-prompt.json && echo prompt-hook=ran
    test -s /tmp/probe-stop.json && echo stop-hook=ran
    test -e /workspace/forbidden.txt && echo forbidden=created || echo forbidden=absent
    test -s /tmp/startup-marker && echo startup=ran
  ' 2>&1)
echo "$out"

fail=0
check() { echo "$out" | grep -q -- "$1" || { echo "FAIL: missing $1" >&2; fail=1; }; }
check "MCP=MAGIC-k123-static-ok"
check "SKILL=KUMQUAT-7"
check "HOOK=ZEBRA-42"
check "exit=0"
check "prompt-hook=ran"
check "stop-hook=ran"
check "forbidden=absent"
grep -q "forbids creating forbidden.txt" "$records/smoke.jsonl" || { echo "FAIL: akhook's deny is not in the record" >&2; fail=1; }
[ $fail -eq 0 ] && echo "smoke test passed; record: $records/smoke.jsonl"
exit $fail
