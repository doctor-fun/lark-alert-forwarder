# lark-alert-forwarder

[English](README.md) | [中文](README.zh-CN.md)

Grafana's default webhook JSON cannot be sent to Lark / Feishu as-is. This service turns it into an interactive app-bot card and handles button callbacks.

```text
Grafana alert
  -> POST /grafana/feishu
  -> Lark interactive card
  -> card.action.trigger
  -> POST /feishu/events
  -> reply in the original thread
```

Sources that can only set a URL (for example DataWorks) can use `POST /dataworks/alert`, with the token and routing in query parameters.

## Features

- Grafana / DataWorks webhook → Lark interactive card
- Card callbacks such as claim, escalate, and reassign
- Optional on-call routing, voice escalation, and read-only AI attribution
- Falls back to a custom bot webhook when the app bot is not configured

## Quick start

```bash
export FEISHU_WEBHOOK="https://open.feishu.cn/open-apis/bot/v2/hook/xxx"
export GRAFANA_FORWARDER_TOKEN="change-me"
go run .
```

Docker:

```bash
docker build -t lark-alert-forwarder .
docker run --rm -p 8080:8080 \
  -e FEISHU_WEBHOOK="https://open.feishu.cn/open-apis/bot/v2/hook/xxx" \
  -e GRAFANA_FORWARDER_TOKEN="change-me" \
  lark-alert-forwarder
```

Health check: `GET /healthz`.

## Grafana contact point

- URL: `https://<your-forwarder>/grafana/feishu`
- Method: `POST`
- Auth: `Authorization: Bearer <GRAFANA_FORWARDER_TOKEN>`
  (`X-Forwarder-Token` is also accepted)

Do not point Grafana 11 at Lark directly. The default webhook payload has no `msg_type`.

## App bot vs custom webhook

Set all of these to send cards through the Lark OpenAPI so buttons can call back:

- `FEISHU_APP_ID`
- `FEISHU_APP_SECRET`
- `FEISHU_CHAT_ID`

Also recommended:

- `FEISHU_VERIFICATION_TOKEN`
- `FEISHU_ENCRYPT_KEY`

If only `FEISHU_WEBHOOK` is set, the service uses a custom bot webhook and claim becomes a URL button.

## Environment variables

Keep secrets in environment variables or a Secret. Do not commit them.

| Variable | Purpose |
| --- | --- |
| `FEISHU_WEBHOOK` | Custom bot webhook. Required when the app bot is not configured |
| `FEISHU_APP_ID` / `FEISHU_APP_SECRET` / `FEISHU_CHAT_ID` | App bot |
| `FEISHU_P1_CHAT_ID` | Optional. Send P1/P2 to a separate chat |
| `FEISHU_VERIFICATION_TOKEN` / `FEISHU_ENCRYPT_KEY` | Verify / decrypt Lark callbacks |
| `GRAFANA_FORWARDER_TOKEN` | Token for Grafana / DataWorks calls |
| `LISTEN_ADDR` | Default `:8080` |
| `SERVICE_CHAT_ROUTES` | `service=oc_xxx;other=oc_yyy` per-service routing |
| `DIRTY_WORK_CANDIDATES` | `Alice\|ou_xxx,Bob\|ou_yyy`. No built-in default |
| `USER_FEEDBACK_ONCALL_CHAT_ID` / `USER_FEEDBACK_ONCALL_CANDIDATES` | Feedback-group on-call hints |
| `COPILOT_RUNNER_URL` / `COPILOT_RUNNER_AGENT` | Delegate AI attribution to an external runner |
| `COPILOT_RUNNER_COMMAND` | Fork a local command when no runner URL is set |
| `SMTP_*` / `EMAIL_FALLBACK_TO` | Email the full attribution report |
| `ALIYUN_VOICE_*` | L2 voice escalation. See `docs/aliyun-voice-alerting.md` |

DataWorks example:

```text
https://<your-forwarder>/dataworks/alert?token=<GRAFANA_FORWARDER_TOKEN>&service=data-platform&severity=P1&alertname=DataSyncFailed&env=prod
```

## Docs

- [Alert Copilot](docs/alert-copilot.md)
- [Alert Copilot email](docs/alert-copilot-email.md)
- [Aliyun voice alerting](docs/aliyun-voice-alerting.md)
- On-call file example: `configs/alert-oncall.example.yaml`

## License

MIT
