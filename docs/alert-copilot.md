# Alert Copilot

`matrix-alert-forwarder` is the entry point for alert interactions. It receives Grafana webhooks, sends Feishu interactive cards, and handles card callbacks. Alert Copilot extends that path in phases.

## Phase 1: read-only attribution

The first phase runs inside the forwarder and is deliberately conservative:

```text
Grafana alert -> Feishu card -> AI 归因 button -> card.action.trigger
  -> rule-based Analyzer -> Feishu thread reply
```

The analyzer uses only context already present in the Grafana payload:

- alert name, service, environment, severity, status
- summary and description annotations
- first alert start time
- panel or dashboard link
- operator who clicked the button

It returns a thread reply with facts, a first judgement, next steps, and references. It does not call cloud APIs and does not perform remediation.

## Phase 2: evidence worker

The next phase keeps the forwarder thin. The forwarder can delegate analysis in two ways:

1. **HTTP runner (recommended)**: send the alert context to an external agent runner.

   ```bash
   COPILOT_RUNNER_URL="http://runner.example.svc:8090"
   COPILOT_RUNNER_AGENT="attribution-agent"
   COPILOT_ANALYSIS_TIMEOUT="2m"
   ```

   The forwarder sends a JSON payload (`{"task_id":"...","context":{...}}`) to `POST {url}/agents/{agent}/invoke`. The runner returns a structured `AgentReport` and the forwarder maps it back onto the existing thread reply format.

2. **Local command (legacy)**: invoke a local script through `COPILOT_RUNNER_COMMAND`. Used only on single-host deployments where the runner is not reachable.

   ```bash
   COPILOT_RUNNER_COMMAND="sh /scripts/copilot_worker.sh"
   COPILOT_ANALYSIS_TIMEOUT="2m"
   ```

If the runner is unreachable, times out, or returns a non-success status, the forwarder falls back to the built-in rule-based analyzer.

The repository includes `scripts/copilot_worker.sh` as a Cursor CLI template for the legacy command path. It calls `cursor-agent --print --mode ask` with a read-only prompt.

By default the worker stays in text-only mode and does not execute shell commands. Set `COPILOT_WORKER_FORCE=1` to allow a small read-only command set such as local log queries and `git log` / `git diff`. Writes are forbidden.

For production, prefer running the Cursor CLI in a dedicated worker image or sidecar with scoped, read-only credentials. The forwarder image only needs to know the runner command, the timeout, and whether to enable force mode.

Recommended worker inputs:

- `incident_id`: deterministic key from alert name, service, env, and start time
- `service`, `env`, `severity`, `alertname`
- `starts_at`, `ends_at`, and a bounded query window
- dashboard, panel, and generator URLs
- original Feishu message ID for thread replies

Recommended evidence sources:

- Prometheus or Grafana for error rate, latency, QPS, and runtime metrics
- SLS logs for `ERROR`, `panic`, `timeout`, downstream RPC failures, and high-frequency signatures
- Yunxiao pipeline facts for recent deploys, failed stages, image tags, and rollout timing
- API smoke tests for user-path validation after a suspected recovery

The report format should stay stable:

```text
事实:
- evidence with source and time window

判断:
- explanation tied to evidence

建议:
- safe next action or escalation
```

Operational guardrails:

- Every source call has a short timeout and a bounded result size.
- Empty or failed evidence queries are reported as unknown, not treated as proof.
- Secrets stay in the worker runtime environment and are never copied into cards, logs, docs, or task payloads.
- Copilot failure must never block the original Grafana alert delivery.

## Phase 3: approved executor

Remediation must be explicit and audited. Copilot can propose actions, but execution happens only after a Feishu approval click.

Allowed first actions:

- rerun a Yunxiao pipeline
- run production or gray smoke tests
- create a patch or MR draft
- notify a service owner in the thread

Restricted actions that require a second confirmation and an allowlist:

- production rollback
- ACK scale or restart
- Nacos or Kubernetes config changes
- database writes

Each action request should contain:

- `incident_id`
- `proposal_id`
- action type and parameters
- requester and approver
- expiry time
- idempotency key

The executor must write an audit record before and after execution. Failed actions should report the reason to the Feishu thread and should not automatically retry high-risk operations.
