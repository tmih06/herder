#!/usr/bin/env bash
# demo/demo.sh — Herder v0.1 end-to-end demo on one machine.
#
# Proves the full story from issue #8's acceptance criteria:
#   labeled issue -> claimed task -> docker sandbox -> visible agent ->
#   human message round-trip -> validation gate -> pushed branch ->
#   open PR -> updated issue labels -> done
# plus restart resilience (kill -9 the daemon mid-run) and
# duplicate-webhook dedup.
#
# How it works without a real agent account: the worker image
# (demo/Dockerfile.worker) ships demo/codex, a deterministic fake agent
# that consumes the seeded contract, waits for a message containing
# "finish", writes FIX.md, commits on the task branch, and exits.
#
# Requirements: go, docker, gh (authed), herdr, python3, curl, git.
# Everything the demo creates lives under a temp dir and is cleaned up
# on exit (pass --keep to preserve it for inspection).
#
# Usage: demo/demo.sh [--keep]

set -euo pipefail

KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
DEMO_DIR=$(mktemp -d /tmp/herder-demo.XXXXXX)
PORT=18787
IMAGE=herder-demo-worker
DELIVERY_ID="demo-$(date +%s)"

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok()   { printf '   \033[32m✓\033[0m %s\n' "$*"; }
die()  { printf '\033[31m!! %s\033[0m\n' "$*" >&2; exit 1; }

# wait_for <desc> <seconds> <command...>: poll until the command exits 0.
wait_for() {
	local desc=$1 timeout=$2; shift 2
	local deadline=$((SECONDS + timeout))
	until "$@" >/dev/null 2>&1; do
		[ $SECONDS -ge $deadline ] && die "timeout waiting for: $desc"
		sleep 2
	done
	ok "$desc"
}

task_status() {
	# No `grep -q`: it exits on first match and SIGPIPEs `task inspect`
	# mid-output — under `set -o pipefail` the pipeline then fails even
	# though the status matched. Read all input instead.
	"$DEMO_DIR/herder" --config "$CFG" task inspect "$TASK_ID" 2>/dev/null | grep "^status: $1" >/dev/null
}

REPO="" TASK_ID="" DAEMON_PID=""
cleanup() {
	set +e
	[ -n "$DAEMON_PID" ] && kill "$DAEMON_PID" 2>/dev/null
	if [ -n "$TASK_ID" ]; then
		# The pane and container outlive `task done` — close them
		# explicitly (reconcile skips DONE tasks by design).
		for ws in $(herdr workspace list 2>/dev/null | python3 -c \
			'import json,sys; print(" ".join(w["workspace_id"] for w in json.load(sys.stdin)["result"]["workspaces"] if w["label"]=="herder-'"$TASK_ID"'"))' 2>/dev/null); do
			herdr workspace close "$ws" >/dev/null 2>&1
		done
		docker rm -f "herder-$TASK_ID" >/dev/null 2>&1
	fi
	[ -n "$REPO" ] && gh repo delete "$REPO" --yes >/dev/null 2>&1
	[ $KEEP -eq 0 ] && rm -rf "$DEMO_DIR" || echo "kept: $DEMO_DIR"
}
trap cleanup EXIT

# ---------- preflight ----------
say "Preflight"
for bin in go docker gh herdr python3 curl git; do
	command -v "$bin" >/dev/null || die "missing: $bin"
done
docker info >/dev/null 2>&1 || die "docker daemon not reachable"
gh auth status >/dev/null 2>&1 || die "gh not authenticated (gh auth login)"
ok "toolchain present"

# ---------- build ----------
say "Build herder + worker image"
(cd "$REPO_ROOT" && go build -o "$DEMO_DIR/herder" ./cmd/herder)
ok "herder binary"
docker build -q -t "$IMAGE" -f "$SCRIPT_DIR/Dockerfile.worker" "$SCRIPT_DIR" >/dev/null
ok "worker image $IMAGE (fake codex inside)"

# ---------- scratch repo ----------
say "Create scratch GitHub repo + issue"
OWNER=$(gh api user -q .login)
REPO="$OWNER/herder-demo-$(date +%H%M%S)"
gh repo create "$REPO" --public --add-readme >/dev/null
# Stage labels must exist before delivery can move them (gh issue edit
# --add-label fails on unknown labels, and that failure is fatal).
for l in agent-ready agent-running agent-review completed; do
	gh label create "$l" --repo "$REPO" --color BFD4F2 >/dev/null
done
ISSUE=$(gh issue create --repo "$REPO" --title "Demo: write FIX.md" \
	--body "Create FIX.md in the repo root and commit it on the task branch." \
	--label agent-ready | grep -o '[0-9]*$')
ok "repo $REPO, issue #$ISSUE (label agent-ready)"

# ---------- config via herder init ----------
say "Scaffold config (herder init) + demo overrides"
CFG="$DEMO_DIR/herder.yaml"
"$DEMO_DIR/herder" --config "$CFG" init >/dev/null
sed -i \
	-e "s|listen: 127.0.0.1:8787|listen: 127.0.0.1:$PORT|" \
	-e "s|path: ~/.local/state/herder/herder.db|path: $DEMO_DIR/herder.db|" \
	-e "s|owner/repo|$REPO|g" \
	-e "s|provider: docker|provider: docker\n      image: $IMAGE|" \
	-e "s|go test ./\.\.\.|timeout 120 test -f /workspace/FIX.md|" \
	"$CFG"
"$DEMO_DIR/herder" --config "$CFG" config validate >/dev/null
ok "config valid (repo=$REPO, image=$IMAGE, gate=test -f FIX.md)"

# ---------- daemon ----------
say "Start daemon"
"$DEMO_DIR/herder" --config "$CFG" daemon >"$DEMO_DIR/daemon.log" 2>&1 &
DAEMON_PID=$!
wait_for "daemon /v1/health" 30 curl -sf "http://127.0.0.1:$PORT/v1/health"

# ---------- webhook: labeled issue -> claimed task ----------
say "Deliver labeled-issue webhook"
PAYLOAD=$(python3 -c "
import json
print(json.dumps({'action':'labeled',
 'issue':{'number':$ISSUE,'title':'Demo: write FIX.md','body':'Create FIX.md and commit it.',
          'labels':[{'name':'agent-ready'}]},
 'repository':{'full_name':'$REPO'}}))")
RESP=$(curl -s -X POST "http://127.0.0.1:$PORT/v1/webhooks/github" \
	-H "X-GitHub-Event: issues" -H "X-GitHub-Delivery: $DELIVERY_ID" \
	-H "Content-Type: application/json" -d "$PAYLOAD")
TASK_ID=$(printf '%s' "$RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("task_id") or "")')
[ -n "$TASK_ID" ] || die "webhook not accepted: $RESP"
ok "task claimed: $TASK_ID"

say "Replay same webhook (duplicate delivery)"
DUP=$(curl -s -X POST "http://127.0.0.1:$PORT/v1/webhooks/github" \
	-H "X-GitHub-Event: issues" -H "X-GitHub-Delivery: $DELIVERY_ID" \
	-H "Content-Type: application/json" -d "$PAYLOAD")
printf '%s' "$DUP" | python3 -c 'import json,sys; exit(0 if json.load(sys.stdin).get("decision")=="duplicate" else 1)' \
	&& ok "duplicate delivery deduped" \
	|| die "expected duplicate, got: $DUP"

# ---------- scheduler: provision + launch ----------
say "Scheduler provisions sandbox and launches agent"
wait_for "task RUNNING (sandbox + agent up)" 240 task_status RUNNING
ok "agent visible: herdr agent attach herder-$TASK_ID"

# ---------- kill -9 restart resilience ----------
say "kill -9 the daemon, restart, verify state survives"
kill -9 "$DAEMON_PID" 2>/dev/null; sleep 1
"$DEMO_DIR/herder" --config "$CFG" daemon >>"$DEMO_DIR/daemon.log" 2>&1 &
DAEMON_PID=$!
task_status RUNNING && ok "task still RUNNING, agent session survived the kill" || {
	# Give reconcile one tick before judging — a transient liveness
	# probe right at daemon startup must not read as a lost task.
	sleep 5
	if task_status RUNNING; then
		ok "task still RUNNING, agent session survived the kill"
	else
		"$DEMO_DIR/herder" --config "$CFG" task inspect "$TASK_ID" >&2 || true
		tail -20 "$DEMO_DIR/daemon.log" >&2 || true
		die "task lost after restart"
	fi
}
# ---------- human message round-trip ----------
say "Human -> agent message (task tell)"
"$DEMO_DIR/herder" --config "$CFG" task tell "$TASK_ID" "human checkpoint: please finish the task now" >/dev/null
wait_for "agent wrote FIX.md" 120 test -f "$DEMO_DIR/sandboxes/$TASK_ID/FIX.md"
ok "round-trip complete: message reached the agent, work committed"

# ---------- validation gate ----------
say "Validation gate"
"$DEMO_DIR/herder" --config "$CFG" task validate "$TASK_ID" >/dev/null
task_status REVIEWING \
	&& ok "gate passed -> REVIEWING" \
	|| die "validation did not reach REVIEWING"

# ---------- delivery ----------
say "Deliver: push branch + open PR + move issue labels"
"$DEMO_DIR/herder" --config "$CFG" task deliver "$TASK_ID" >/dev/null
gh pr list --repo "$REPO" --json number --jq '.[0].number' | grep '[0-9]' >/dev/null \
	&& ok "PR open on $REPO" \
	|| die "no PR found"
gh issue view "$ISSUE" --repo "$REPO" --json labels --jq '[.labels[].name]|join(" ")' \
	| grep 'agent-review' >/dev/null && ok "issue labeled agent-review" \
	|| die "issue labels not advanced"

# ---------- done ----------
say "Mark done"
"$DEMO_DIR/herder" --config "$CFG" task done "$TASK_ID" >/dev/null
task_status DONE \
	&& ok "task DONE" \
	|| die "task not DONE"

say "DEMO PASSED"
echo "labeled issue -> task -> sandbox -> agent -> human round-trip -> validation -> PR -> labels -> done"
echo "plus: kill -9 restart resilience, duplicate-webhook dedup"
