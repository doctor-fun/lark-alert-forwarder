# Alert Copilot — Email Delivery (Phase 7-D)

Phase 7-D moves the **full attribution report** out of the Feishu interactive
card and into an HTML email, sent to the operator who clicked the **AI 归因**
button. The Feishu thread keeps only a slim summary card with:

- Operator + service/env/severity meta (matches the original alert colour)
- A one-sentence "根因猜测" preview
- A `📧 完整报告已发邮件至 <operator>@…` banner
- The legacy `我来处理` callback button + dashboard link

The full facts/judgement/next-steps/references list lives in the email,
where users get readable HTML and can scroll/forward like any normal
incident report.

## Why email?

The interactive card renders the long-form report as Markdown inside a
fixed-width container. Once the report grows past ~6–8 bullets the card
becomes a wall of monospace text that is hard to scan on mobile and
impossible to forward to a customer / vendor. Email gives us:

- Real HTML with semantic headings, lists, code blocks, links.
- Standard reply / forward / archive workflows.
- A persistent record outside Feishu's chat retention rules.
- Trivial extension to per-team CC lists when we onboard more services.

## Activation

Email delivery is **opt-in**: it kicks in only when `SMTP_HOST` is set.
Without it the forwarder behaves exactly as before (full card in thread).

```bash
# alert-forwarder env
SMTP_HOST=smtp.exmail.qq.com         # Tencent EXMail / Aliyun EXMail / Postfix etc.
SMTP_PORT=465                        # 465 for SMTPS, 587 for STARTTLS
SMTP_USERNAME=alerts@example.com
SMTP_PASSWORD=<from K8s Secret>
SMTP_FROM_NAME="Alert Copilot"
SMTP_USE_TLS=true                    # default; set "false" for STARTTLS:587

# Optional fallback inbox when operator email cannot be resolved
EMAIL_FALLBACK_TO=oncall@example.com,sre@example.com
```

Recommended secret layout:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: matrix-alert-forwarder-smtp
  namespace: prod
type: Opaque
stringData:
  SMTP_PASSWORD: "<paste here>"
```

Reference it from the deployment:

```yaml
env:
  - name: SMTP_PASSWORD
    valueFrom:
      secretKeyRef: { name: matrix-alert-forwarder-smtp, key: SMTP_PASSWORD }
  - name: SMTP_HOST
    value: "smtp.exmail.qq.com"
  # …other SMTP_* and EMAIL_FALLBACK_TO
```

## Required Feishu scope

To resolve the operator's mailbox we call `GET /open-apis/contact/v3/users/{open_id}`,
which requires one of the following scopes on the **forwarder's** Feishu app:

- `contact:user.email:readonly` (preferred — only exposes email)
- `contact:user.base:readonly`  (also exposes name/avatar)

Add it from the developer console:

1. Open https://open.feishu.cn/app and pick the app whose APP_ID matches `FEISHU_APP_ID`.
2. **Permissions & Scopes** → search `contact:user.email:readonly` → **Apply**.
3. **Version Management** → publish a new version → tenant admin approves.
4. (Optional) Restart the forwarder pod so any cached tenant tokens are
   refreshed; tokens auto-refresh within ~2 hours but a restart is
   instant.

Without the scope the forwarder logs `feishu contact: app missing scope …`
and falls back to `EMAIL_FALLBACK_TO`. If both are missing it logs the
failure and posts the legacy full card so attribution is **never lost**.

## Failure semantics

| Failure                         | Behaviour                                                  |
| ------------------------------- | ---------------------------------------------------------- |
| `SMTP_HOST` unset               | Legacy full-card path. Logged once at startup.             |
| SMTP credentials wrong          | Per-call error logged. Posts legacy full card to thread.   |
| Feishu scope missing            | Logged with `errFeishuScopeMissing`. Tries fallback list.  |
| Operator has no enterprise mail | Falls back to `email` field, then to `EMAIL_FALLBACK_TO`.  |
| All recipients empty            | Posts legacy full card to thread; no silent drop.          |
| HTML render error               | Returns error; legacy path runs. Should never happen with valid `AnalysisReport`. |

## Local test

```bash
# Render an email without sending it
cat <<'EOF' | go test -v ./... -run TestRenderReportEmailIncludesAllSections
EOF

# Smoke an SMTP route end-to-end (e.g. against MailHog on :1025)
SMTP_HOST=127.0.0.1 SMTP_PORT=1025 SMTP_USERNAME=test SMTP_PASSWORD=test \
  SMTP_USE_TLS=false ./matrix-alert-forwarder
```
