# lark-alert-forwarder

[English](README.md) | [中文](README.zh-CN.md)

Grafana 的默认 webhook JSON 不能直接发给飞书。这个服务把它转成飞书应用机器人互动卡片，并处理卡片按钮回调。

```text
Grafana 告警
  -> POST /grafana/feishu
  -> 飞书互动卡片
  -> card.action.trigger
  -> POST /feishu/events
  -> 在原消息线程里回复
```

只会填 URL、不能加请求头的来源（例如 DataWorks）可以走 `POST /dataworks/alert`，用查询参数带 token 和路由信息。

## 功能

- Grafana / DataWorks webhook → 飞书互动卡片
- `我来处理`、升级、重新指派等卡片回调
- 可选值班路由、电话升级、只读 AI 归因
- 未配置应用机器人时，回退到自定义机器人 webhook

## 快速开始

```bash
export FEISHU_WEBHOOK="https://open.feishu.cn/open-apis/bot/v2/hook/xxx"
export GRAFANA_FORWARDER_TOKEN="change-me"
go run .
```

Docker：

```bash
docker build -t lark-alert-forwarder .
docker run --rm -p 8080:8080 \
  -e FEISHU_WEBHOOK="https://open.feishu.cn/open-apis/bot/v2/hook/xxx" \
  -e GRAFANA_FORWARDER_TOKEN="change-me" \
  lark-alert-forwarder
```

健康检查：`GET /healthz`。

## Grafana 通知渠道

- URL：`https://<your-forwarder>/grafana/feishu`
- Method：`POST`
- Auth：`Authorization: Bearer <GRAFANA_FORWARDER_TOKEN>`
  （也接受 `X-Forwarder-Token`）

不要让 Grafana 11 直接打飞书。默认 webhook 没有飞书要的 `msg_type`。

## 应用机器人 vs 自定义机器人

同时配置这些变量后，会走飞书 OpenAPI，卡片按钮可以回调回来：

- `FEISHU_APP_ID`
- `FEISHU_APP_SECRET`
- `FEISHU_CHAT_ID`

另外建议配置：

- `FEISHU_VERIFICATION_TOKEN`
- `FEISHU_ENCRYPT_KEY`

只配 `FEISHU_WEBHOOK` 时走自定义机器人，`我来处理` 会变成普通链接按钮。

## 环境变量

密钥只放环境变量或 Secret，不要写进仓库。

| 变量 | 用途 |
| --- | --- |
| `FEISHU_WEBHOOK` | 自定义机器人 webhook。未配应用机器人时必填 |
| `FEISHU_APP_ID` / `FEISHU_APP_SECRET` / `FEISHU_CHAT_ID` | 应用机器人 |
| `FEISHU_P1_CHAT_ID` | 可选。P1/P2 另投一个群 |
| `FEISHU_VERIFICATION_TOKEN` / `FEISHU_ENCRYPT_KEY` | 校验 / 解密飞书回调 |
| `GRAFANA_FORWARDER_TOKEN` | Grafana / DataWorks 调用本服务的 token |
| `LISTEN_ADDR` | 默认 `:8080` |
| `SERVICE_CHAT_ROUTES` | `service=oc_xxx;other=oc_yyy` 按服务分流 |
| `DIRTY_WORK_CANDIDATES` | `Alice\|ou_xxx,Bob\|ou_yyy`，无默认值 |
| `USER_FEEDBACK_ONCALL_CHAT_ID` / `USER_FEEDBACK_ONCALL_CANDIDATES` | 反馈群值班提示 |
| `COPILOT_RUNNER_URL` / `COPILOT_RUNNER_AGENT` | 把 AI 归因交给外部 runner |
| `COPILOT_RUNNER_COMMAND` | 未配 URL 时 fork 本地命令 |
| `SMTP_*` / `EMAIL_FALLBACK_TO` | 把完整归因报告发邮件 |
| `ALIYUN_VOICE_*` | L2 电话升级，见 `docs/aliyun-voice-alerting.md` |

DataWorks 示例：

```text
https://<your-forwarder>/dataworks/alert?token=<GRAFANA_FORWARDER_TOKEN>&service=data-platform&severity=P1&alertname=DataSyncFailed&env=prod
```

## 文档

- [Alert Copilot](docs/alert-copilot.md)
- [Alert Copilot 邮件](docs/alert-copilot-email.md)
- [阿里云语音告警](docs/aliyun-voice-alerting.md)
- 值班配置示例：`configs/alert-oncall.example.yaml`

## License

MIT
