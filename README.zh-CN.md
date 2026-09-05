# lark-alert-forwarder

[English](README.md) | [中文](README.zh-CN.md)

Grafana 的默认 webhook JSON 不能直接发给飞书。这个服务把它转成飞书应用机器人互动卡片，并处理卡片按钮回调。

## 设计初衷

Grafana 能发 webhook，飞书能收机器人消息，中间却缺一层，直接对接会卡住两件事：

1. **格式对不上。** Grafana 默认 payload 没有飞书要的 `msg_type`，Contact Point 直连飞书 webhook 会失败，或变成一坨没人看的 JSON。
2. **进群不等于有人处理。** 告警刷在群里，没有认领、不知道谁在看、没人点就石沉大海。需要卡片上的「我来处理」，并在原线程留下处理人。

所以这个服务故意做薄：只负责收告警、转成互动卡片、处理按钮点击。值班表、AI 归因、电话升级、定时催办都是可选旁路——不配就不走，旁路挂了也不能挡住 Grafana → 飞书这条主链路。

## 架构

Forwarder 主链路只做三件事：收告警、转成飞书卡片、处理卡片上的按钮点击。值班、归因、电话、定时催办都是可选旁路，不配就不走。

![架构图](docs/architecture.png)

`/grafana/feishu` 是告警进线。`/feishu/events` 是飞书开放平台的事件订阅回调：人点卡片后飞书回打过来，不是第二条告警入口。在开放平台保存这个 URL 时，会先有一次 `url_verification` 握手。定时问题处理不走 Grafana，是本服务自己扫多维表格，再发催办卡。

源文件：[architecture.puml](docs/architecture.puml)

告警打进来：

1. Grafana 用 webhook Contact Point 打 `POST /grafana/feishu`，带 `Authorization: Bearer`。不要让 Grafana 直连飞书，默认 payload 没有 `msg_type`。
2. DataWorks 等正文不是 Grafana webhook 的来源打 `POST /dataworks/alert`，适配后再走同一条发卡片链路。路由（`service` / `severity` 等）和 token 可以放在查询参数里，因为这类来源的 body 字段不统一。
3. Forwarder 校验 token，读 label，按 `SERVICE_CHAT_ROUTES`、`FEISHU_P1_CHAT_ID` 选群。
4. 配了应用机器人就走 OpenAPI 发互动卡片；只配了 `FEISHU_WEBHOOK` 就退化成自定义机器人，按钮变成普通链接。
5. 配了 `ALERT_BACKEND_URL` 时，还会去重/建工单、拿值班人。

人点卡片（飞书回打 `/feishu/events`）：

1. 值班人点的是飞书卡片。飞书开放平台把 `card.action.trigger` POST 到本服务，请求里带着点击人身份。
2. Forwarder 用 Verification Token / Encrypt Key 验过，确认是飞书来的。
3. 「我来处理」走 OpenAPI 在原线程回复并记下处理人；「AI 归因」调可选 runner，群里留摘要，全文可走邮件。

升级（可选）：

1. Escalator 周期性看未认领的告警。
2. 过 L1：在原线程 @ 值班池。
3. 过 L2：再 @ leader，并按白名单严重度打阿里云 TTS 电话。

定时问题处理（可选）：

告警之外，还可以盯一张飞书多维表格里的待办。不配表格、不配应用机器人就不跑；挂了也不挡告警主链路。人点催办卡上的按钮，仍然走 `/feishu/events`。

1. 群里用卡片把问题分给候选人（轮询或点名）。
2. **超时催办**：超过 `DIRTY_WORK_TIMEOUT_REMINDER_AFTER`（默认 48h）还没完成，按间隔再发一张卡。
3. **议题催办**：按某个字段筛未完成记录（例如某次发布），定时发到指定群。

## 告警卡片

群里看到的是一张飞书互动卡片，不是 Grafana 原文。标题栏按状态染色：**FIRING 红**、**RESOLVED 绿**、其它橙。

![告警卡片示意](docs/alert-card.png)

| 区域 | 内容 |
| --- | --- |
| 标题 | `[Grafana告警] FIRING <alertname>` |
| 字段 | 服务、环境、级别、状态 |
| 正文 | 摘要、详情；重复触发会多一行「已合并到 incident」 |
| 人员 | 配了值班才会出现责任人 / 抄送 |
| 动作 | 应用机器人：`我来处理`、`AI 归因`、`打开大盘`。接了工单后端才有 `已修复`、`标为误报`、屏蔽时长 |
| 页脚 | `incident #N`（接了工单时） |

自定义机器人模式下，按钮会退化成跳转链接，点了也不知道是谁。责任人还会另收一张更短的私聊卡，提醒先回群里点「我来处理」。`AI 归因` 的完整报告走邮件，群线程只留一句根因猜测。

## 功能

- Grafana / DataWorks webhook → 飞书互动卡片
- `我来处理`、升级、重新指派等卡片回调
- 可选值班路由、电话升级、只读 AI 归因
- 可选定时问题处理：扫多维表格，超时 / 按议题再催
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

## 配置飞书

有两种接法。要「我来处理」识别点击人、并在原线程回复，必须用应用机器人。自定义机器人只能发卡片，按钮只能做成跳转链接。

### 自定义机器人（只发不回）

1. 打开目标飞书群 → 设置 → 群机器人 → 添加 **自定义机器人**。
2. 复制 webhook，配到 `FEISHU_WEBHOOK`。
3. Grafana 照常打到本服务，不要直接打这个 webhook。

限制：飞书自定义机器人没有可靠的卡片回调身份，点「我来处理」无法知道是谁。

### 应用机器人（推荐）

在 [飞书开放平台](https://open.feishu.cn/app) 创建**企业自建应用**。

1. **添加应用能力** → 启用 **机器人**。
2. **权限管理** 申请并发布（管理员审批）：
   - `im:message` / `im:message:send_as_bot`：以机器人身份发卡片、回线程
   - `im:chat:readonly`：列出机器人所在群，方便拿 `chat_id`
   - 要用邮件归因时再加 `contact:user.email:readonly`，见 [Alert Copilot 邮件](docs/alert-copilot-email.md)
3. **事件订阅**：
   - 请求网址：`https://<your-forwarder>/feishu/events`（必须公网 HTTPS，飞书会先发 `url_verification`）
   - 打开加密，记下 Verification Token、Encrypt Key
   - 订阅 `card.action.trigger`（卡片按钮）
   - 若要用群内指令或反馈值班，再订 `im.message.receive_v1`
4. **版本管理** 发布新版本，等租户管理员通过。
5. 把机器人拉进告警群。
6. 取群 `chat_id`（`oc_` 开头）：
   - 用 tenant token 调 `GET /open-apis/im/v1/chats`
   - 或先订 `im.message.receive_v1`，在群里 @ 机器人，日志/回调里会带 `chat_id`

然后配环境变量：

```bash
export FEISHU_APP_ID="cli_xxx"
export FEISHU_APP_SECRET="xxx"
export FEISHU_CHAT_ID="oc_xxx"
export FEISHU_VERIFICATION_TOKEN="xxx"
export FEISHU_ENCRYPT_KEY="xxx"
export GRAFANA_FORWARDER_TOKEN="change-me"
```

`FEISHU_P1_CHAT_ID` 可选，P1/P2 另投一个群。多个服务分流用 `SERVICE_CHAT_ROUTES=api=oc_aaa;data=oc_bbb`。

改完权限或事件后必须重新发布应用版本，只保存草稿不会生效。

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
| `DIRTY_WORK_CANDIDATES` | `Alice\|ou_xxx,Bob\|ou_yyy`。无默认值；有多维表格时以表为准 |
| `DIRTY_WORK_BITABLE_APP_TOKEN` 及表/字段 | 待办多维表格。不配则定时催办不启动 |
| `DIRTY_WORK_TIMEOUT_REMINDER_CHAT_ID` / `_AFTER` / `_INTERVAL` | 超时未完成再催。默认 48h / 10m |
| `DIRTY_WORK_TOPIC_REMINDER` | `议题名\|oc_xxx\|30m` 按字段筛未完成再催 |
| `USER_FEEDBACK_ONCALL_CHAT_ID` / `USER_FEEDBACK_ONCALL_CANDIDATES` | 反馈群值班提示 |
| `COPILOT_RUNNER_URL` / `COPILOT_RUNNER_AGENT` | 把 AI 归因交给外部 runner |
| `COPILOT_RUNNER_COMMAND` | 未配 URL 时 fork 本地命令 |
| `SMTP_*` / `EMAIL_FALLBACK_TO` | 把完整归因报告发邮件 |
| `ALIYUN_VOICE_*` | L2 电话升级，见 `docs/aliyun-voice-alerting.md` |

DataWorks 等非 Grafana 来源示例（body 字段不统一，路由放查询参数）：

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
