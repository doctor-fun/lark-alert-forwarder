# Matrix API 告警链路说明

本文档记录 `matrix-api` 从 Grafana 告警到飞书应用机器人卡片的完整配置，方便后续排查、迁移和扩展。

## 架构图

- PlantUML 源文件：`docs/architecture.puml`
- SVG 渲染图：`docs/architecture.svg`

## 关键入口

| 项目 | 值 |
| --- | --- |
| Grafana | `https://grafana-cn-0564r6g2h02.grafana.aliyuncs.com/` |
| Forwarder 外部地址 | `https://metis.hnivj.com/alert-forwarder` |
| Grafana webhook 接口 | `https://metis.hnivj.com/alert-forwarder/grafana/feishu` |
| 飞书事件回调接口 | `https://metis.hnivj.com/alert-forwarder/feishu/events` |
| 健康检查 | `https://metis.hnivj.com/alert-forwarder/healthz` |

## 飞书应用机器人

| 项目 | 值 |
| --- | --- |
| 应用类型 | 飞书应用机器人 |
| App ID | `cli_a9658fda2a30dbc6` |
| 默认告警群 | `P0告警群` |
| Chat ID | `oc_5ae95c63bea92cb283f2e6cf7f96f5f4` |
| P1/P2 专用群 | `oc_aa64c4b8609162db4f93fd122876df82` |
| 事件订阅 | `card.action.trigger` |

敏感参数不写入仓库，部署时通过环境变量注入：

| 环境变量 | 说明 |
| --- | --- |
| `FEISHU_APP_SECRET` | 飞书应用密钥，敏感 |
| `FEISHU_VERIFICATION_TOKEN` | 飞书事件订阅 Verification Token，敏感 |
| `FEISHU_ENCRYPT_KEY` | 飞书事件订阅 Encrypt Key，敏感 |

## Forwarder 环境变量

| 环境变量 | 当前用途 |
| --- | --- |
| `FEISHU_APP_ID` | 飞书应用 ID |
| `FEISHU_APP_SECRET` | 飞书应用密钥 |
| `FEISHU_CHAT_ID` | 默认目标飞书群 ID，当前为 `oc_5ae95c63bea92cb283f2e6cf7f96f5f4` |
| `FEISHU_P1_CHAT_ID` | 可选，P1/P2 告警专用飞书群 ID，当前为 `oc_aa64c4b8609162db4f93fd122876df82`；为空时 P1/P2 仍发送到 `FEISHU_CHAT_ID` |
| `FEISHU_VERIFICATION_TOKEN` | 校验飞书事件回调 |
| `FEISHU_ENCRYPT_KEY` | 解密飞书事件回调 |
| `GRAFANA_FORWARDER_TOKEN` | Grafana 调用 forwarder 的鉴权 token |
| `LISTEN_ADDR` | 服务监听地址，建议 `:8080` |
| `FEISHU_WEBHOOK` | 兼容旧自定义机器人模式；应用机器人模式下不是必需 |

鉴权说明：

- Grafana 11 不能稳定传递自定义 `X-Forwarder-Token` 请求头。
- 当前 forwarder 同时支持：
  - `X-Forwarder-Token: <GRAFANA_FORWARDER_TOKEN>`
  - `Authorization: Bearer <GRAFANA_FORWARDER_TOKEN>`
- Grafana Contact Point 当前使用 `Authorization: Bearer`。

群路由说明：

- Grafana payload 的 `commonLabels.severity` 为 `P1` 或 `P2` 时，forwarder 优先发送到 `FEISHU_P1_CHAT_ID`。
- 其他级别（例如 `P0`）发送到 `FEISHU_CHAT_ID`。
- 如果 `FEISHU_P1_CHAT_ID` 未配置，所有级别都发送到 `FEISHU_CHAT_ID`，保持旧行为。

## Grafana 配置

### 数据源

| 数据源 | UID |
| --- | --- |
| Prometheus | `prom-c148a5bdfaa0d4ce0a05015fe302de652` |

### 大盘

| 大盘 | UID | URL |
| --- | --- | --- |
| Matrix-API Overview | `matrix-api` | `https://grafana-cn-0564r6g2h02.grafana.aliyuncs.com/d/matrix-api/matrix-api-overview` |

告警规则通过以下注解关联到具体大盘和面板：

```text
__dashboardUid__ = matrix-api
__panelId__ = <panel id>
```

### Contact Point

| 项目 | 值 |
| --- | --- |
| Name | `Matrix API Alert Forwarder` |
| UID | `matrix-api-alert-forwarder` |
| Type | `webhook` |
| URL | `https://metis.hnivj.com/alert-forwarder/grafana/feishu` |
| Auth | `Bearer` |

### Notification Policy

| 项目 | 值 |
| --- | --- |
| 匹配条件 | `service = matrix-api` |
| 接收渠道 | `Matrix API Alert Forwarder` |
| 分组 | `alertname`, `service` |
| group_wait | `30s` |
| group_interval | `5m` |
| repeat_interval | `1h` |

### 当前告警规则

Grafana API 当前查询到的 `matrix-api-production` 规则如下：

| 规则 | 条件摘要 | 持续时间 | 级别 | 绑定面板 |
| --- | --- | --- | --- | --- |
| `MatrixAPI5xxDetected` | 单 route 5xx 错误率 `> 2%`，且该 route QPS `> 1` | `3m` | `P1` | `SRE 候选：接口错误率（按路由，带流量门槛）` / panel `28` |
| `MatrixAPIHighP99Latency` | 单 route P99 `> 1s`，且该 route QPS `> 1` | `5m` | `P1` | `SRE 候选：接口 P99 延迟（按路由，带流量门槛）` / panel `29` |
| `MatrixAPIDownstreamUnimplemented` | `matrix_api_downstream_errors_total{error_type="Unimplemented"} > 0` | `2m` | `P1` | `下游错误（按服务+类型）` / panel `14` |
| `MatrixAPIDownstreamInfraErrors` | 下游 `Unavailable|DeadlineExceeded|ResourceExhausted|Canceled` 错误速率 `> 0.05/s` | `3m` | `P1` | `下游错误（按服务+类型）` / panel `14` |
| `MatrixAPIAuthVerifyFailedSpike` | `verify_failed` 占认证请求比例 `> 5%` | `3m` | `P1` | `认证失败率` / panel `4` |
| `MatrixAPILoginFailureRateP2` | 登录 mode 维度失败率 `> 3%`，且该 mode QPS `> 0.05` | `10m` | `P2` | `SRE 登录成功率（有流量）` / panel `30` |
| `MatrixAPIOrderFailureRateP2` | 订单 action/channel 维度失败率 `> 3%`，且该维度 QPS `> 0.02` | `10m` | `P2` | `SRE 订单成功率（有流量）` / panel `34` |
| `MatrixAPIPaymentCallbackFailureRateP2` | 支付回调 channel 维度失败率 `> 3%`，且该 channel QPS `> 0.01` | `10m` | `P2` | `SRE 支付回调成功率（有流量）` / panel `35` |
| `MatrixAPIDownstreamErrorRateP2` | 下游 target 错误率 `> 5%`，且 target QPS `> 0.1` | `5m` | `P2` | `SRE 下游错误率（按服务）` / panel `31` |
| `MatrixAPIDownstreamInfraErrorMoMP2` | 下游基础设施错误当前 5m 比前一非重叠 5m `> 3x`，且当前速率 `> 0.03/s` | `10m` | `P2` | `下游错误（按服务+类型）` / panel `14` |
| `MatrixAPIH5ActivityErrorRateP2` | H5 `current`/`detail` 的 `result=error` 占比 `> 5%`，且该 api 5m 请求数 `> 50`（不含 miss/reject/hit） | `5m` | `P2` | `SRE 下游错误率（按服务）` / panel `31`（暂无专用 H5 面板） |
| `MatrixAPIPodRestartSpikeP2` | 任一 `matrix-api` Pod 15 分钟内发生容器重启 | `1m` | `P2` | `SRE Pod 重启` / panel `32` |
| `MatrixAPITrafficSuddenDropP0` | 总 QPS 相对 15 分钟前 `< 50%`，且绝对值掉 `> 80 req/s`，且 15 分钟前 QPS `> 120` | `5m` | `P0` | `QPS（按状态码）` / panel `7` |
| `MatrixAPITrafficSuddenSpikeP0` | 总 QPS 相对 15 分钟前 `> 1.5x`，且绝对值涨 `> 40 req/s`，且当前 QPS `> 80` | `3m` | `P0` | `QPS（按状态码）` / panel `7` |
| `MatrixAPIOrderCreateSuccessZeroAppleP0` | Apple/iOS 预建单 10 分钟内次数 `>= 8` 且成功 `< 1` | `3m` | `P0` | `SRE 订单成功率（有流量）` / panel `34` |
| `MatrixAPIOrderCreateSuccessZeroGoogleP0` | Google/Android 预建单 10 分钟内次数 `>= 8` 且成功 `< 1` | `3m` | `P0` | `SRE 订单成功率（有流量）` / panel `34` |

#### H5 特殊活动相关规则（OpenSpec `add-h5-special-activity` / T705）

| 规则 UID | 规则名 | PromQL 摘要 | 组 | 静音建议 |
| --- | --- | --- | --- | --- |
| `matrix-api-h5-activity-error-rate-p2` | `MatrixAPIH5ActivityErrorRateP2` | `(rate error / rate total) by (api=~"current\|detail") > 0.05` AND `increase(total[5m]) > 50` | `matrix-api-production` | 飞书卡片静音 30m |
| `app-srv-audience-membership-p99-p2` | `AppSrvAudienceMembershipP99P2` | `histogram_quantile(0.99, rate(app_srv_audience_membership_duration_seconds_bucket[5m])) > 0.1` | `app-srv-production` | 飞书卡片静音 30m |

- Notification Policy：`service=app-srv` 已接到 `Matrix API Alert Forwarder`（与 `matrix-api` / `activity-srv` 同 Contact Point）。
- **Deferred（activity-srv）**：`ListLiveH5Activities` 5m 错误率告警未建。FACT：activity-srv 仓内无 Prometheus（T109 仅日志），现网无 `activity_srv_h5_activity_op_total` series；不发明假指标。待补 metric 或复用既有 SLS 日志告警模式后再加。
- 指标现状：生产 Prometheus 在规则创建时尚未见到 `matrix_api_h5_activity_request_total` / `app_srv_audience_membership_duration_seconds_*`（灰度未全量）；规则 `noDataState=OK`，无数据不告警。

当前规则都带有：

- `service=matrix-api`
- `env=prod`
- `severity=P0/P1/P2`（`P0` 进 P0 告警群，未认领走 L1/L2 电话）
- `__dashboardUid__=matrix-api`
- `__panelId__=<panel id>`

当前状态：

- `MatrixAPI5xxDetected` 已从原始 5xx QPS 升级为 route 维度错误率，并加入最小流量门槛。
- `MatrixAPIHighP99Latency` 已从全局 P99 升级为 route 维度 P99，并加入最小流量门槛。
- 已新增登录失败率、订单失败率、支付回调失败率、下游错误率、Pod 重启 5 条 P2 规则，作为症状补充和原因定位。
- 已新增下游基础设施错误环比 P2：按 `target + error_type` 比较相邻非重叠 5 分钟窗口，并用绝对错误速率门槛过滤低基数噪音。
- Grafana 告警规格升级后，业务失败率已拆成 `MatrixAPILoginFailureRateP2`、`MatrixAPIOrderFailureRateP2`、`MatrixAPIPaymentCallbackFailureRateP2` 三条独立规则。
- 下游 Unimplemented、基础设施错误、认证失败这 3 条存量 P1 暂时保留，后续可根据噪音和事故复盘再降级。
- `MatrixAPIAuthVerifyFailedSpike` 已经是比例类规则，比原始错误计数更接近 SRE 告警形态。
- Grafana 运行态校验时，上述新增/更新规则 `health=ok`，且当前均为 `inactive`。
- 2026-09-04 补了 4 条 P0：总 QPS 15 分钟环比陡降/陡增，以及 Apple(iOS)/Google(Android) 预建单成功率跌 0。`severity=P0` 会进 P0 群并走电话升级；原 `MatrixAPIOrderFailureRateP2`（失败率 `> 3%`）仍保留作早期观察。
- 2026-09-05 把 `MatrixAPITrafficSuddenDropP0` 从「掉 30% / 40 req/s / 基线 80 / 持续 3m」收紧到「掉 50% / 80 req/s / 基线 120 / 持续 5m」。回放近 36 小时，旧阈值会在每天约 11:00 的投放脉冲回落上误报（09-04 179→105、09-05 274→155，期间 5xx ≈ 0）；新阈值这两次都不会响，真实腰斩/入口打丢仍会 P0。
- 2026-11-09（T705）新增 `MatrixAPIH5ActivityErrorRateP2` 与 `AppSrvAudienceMembershipP99P2`：H5 C 端只告 `result=error`，带 5m 请求数 `> 50` 门槛；人群包 P99 `> 100ms` 持续 10m。运行态 `health=ok` / `inactive`。activity-srv `ListLiveH5Activities` 错误率规则因无 Prometheus 指标延期。

### 当前可复用面板与指标

`Matrix-API Overview` 大盘 UID 为 `matrix-api`，当前有 36 个面板。和本次告警增强最相关的面板如下：

| Panel ID | 面板 | 主要指标形态 | 可支持的告警方向 |
| --- | --- | --- | --- |
| `3` | `P99 延迟` | `elf_http_request_duration_seconds_bucket` 全局 P99 | 全局延迟告警 |
| `4` | `认证失败率` | `matrix_api_auth_verify_total{result,reason}` | 认证失败比例 |
| `5` | `下游成功率` | `matrix_api_downstream_requests_total{result}` | 下游整体成功率 |
| `6` | `支付回调成功率` | `matrix_api_payment_callback_total{result}`，成功结果包含 `success`、`refund_success`、`vip_expire_success`、`vip_expire_skipped`、`skipped` | 支付回调业务成功率 |
| `14` | `下游错误（按服务+类型）` | `matrix_api_downstream_errors_total{target,error_type}` | 下游原因类告警 |
| `21` | `接口 P99 延迟（按路由）` | `elf_http_request_duration_seconds_bucket{route}` | 按 route 延迟告警 |
| `22` | `接口错误率（按路由 5xx）` | `elf_http_requests_total{route,code}` 错误比例 | 按 route 可用性告警 |
| `23` | `登录 QPS（按方式）` | `matrix_api_login_total{mode,result}` | 登录业务成功率 |
| `24` | `订单操作（按动作+渠道）` | `matrix_api_order_total{action,channel,result}` | 订单业务成功率 |
| `26` | `订单耗时 P99（按动作+渠道）` | `matrix_api_order_duration_seconds_bucket{action,channel}` | 订单链路延迟 |
| `28` | `SRE 候选：接口错误率（按路由，带流量门槛）` | route 5xx 比例 + route QPS `> 1` | P1 可用性规则候选 |
| `29` | `SRE 候选：接口 P99 延迟（按路由，带流量门槛）` | route P99 + route QPS `> 1` | P1/P2 延迟规则候选 |
| `30` | `SRE 登录成功率（有流量）` | 登录成功率 + mode QPS `> 0.05` | 登录失败率 P2 |
| `31` | `SRE 下游错误率（按服务）` | 下游错误数 / 下游请求数 | 依赖原因告警候选 |
| `32` | `SRE Pod 重启` | `kube_pod_container_status_restarts_total` 15 分钟增量 | Pod 重启原因告警候选 |
| `33` | `SRE Panic/Fatal 日志入口` | 跳转 `Matrix-API Logs` | panic/fatal 日志排查入口 |
| `34` | `SRE 订单成功率（有流量）` | 订单成功率 + action/channel QPS `> 0.02` | 订单失败率 P2 |
| `35` | `SRE 支付回调成功率（有流量）` | 支付回调成功率 + channel QPS `> 0.01` | 支付回调失败率 P2 |
| `36` | `SRE 业务量（登录/订单/支付）` | 登录、订单、支付回调 5 分钟 QPS | 成功率分母背景 |

这说明第一阶段无需等全量新埋点，可以先基于现有指标落地：

- 把 `MatrixAPI5xxDetected` 升级为 panel `22` 对应的“按 route 5xx 比例 + 最小流量门槛”。
- 把 `MatrixAPIHighP99Latency` 升级为 panel `21` 对应的“按 route P99 + 请求量门槛”，保留全局 P99 作为总览。
- 基于 `matrix_api_login_total`、`matrix_api_order_total`、`matrix_api_payment_callback_total` 建立登录、订单、支付回调的 P2 业务失败率告警；成功率面板使用流量门槛，避免“无流量=0%”误判。
- 保留 panel `14` 的下游错误作为原因告警，但重新评估 P1/P2 分级。

### Prometheus 指标盘点

最近 6 小时 Prometheus series 盘点结果：

| 指标 | 当前标签 | 观察结论 |
| --- | --- | --- |
| `matrix_api_login_total` | `mode`, `result` | 已覆盖 `visitor`、`firebase` 登录方式；最近窗口只观察到 `success`，需要确认失败路径是否会产生 `error` series |
| `matrix_api_order_total` | `action`, `channel`, `result` | 已覆盖 `create`、`restore`、`verify` 和 `apple`、`google`、`tiktok`，且有 `success/error` |
| `matrix_api_order_duration_seconds_bucket` | `action`, `channel`, `le` | 已具备订单动作和渠道维度 P99 延迟 |
| `matrix_api_payment_callback_total` | `channel`, `result` | 已覆盖 Apple 支付回调，存在 `success/error/vip_expire_success` |
| `matrix_api_downstream_requests_total` | `target`, `method`, `result` | 已覆盖 `user-srv`、`order-srv`、`media-srv`、`activity-srv`、`comment-srv`、`matrix-backend`、`gorush` |
| `matrix_api_downstream_errors_total` | `target`, `method`, `error_type` | 最近窗口观察到 `Canceled`、`DeadlineExceeded`、`Internal`、`unknown`，可支撑下游原因类告警 |
| `matrix_api_auth_verify_total` | `result`, `reason` | 已覆盖认证验证结果和原因 |
| `elf_http_requests_total` | `service`, `route`, `method`, `code` | 已具备按 route 的 5xx 比例和最小流量门槛规则基础 |
| `elf_http_request_duration_seconds_bucket` | `service`, `route`, `method`, `le` | 已具备按 route 的 P99 延迟规则基础 |
| `kube_pod_container_status_restarts_total` | `namespace`, `pod`, `container` | 已具备 Pod 重启类原因告警基础 |

当前明确缺口：

- Firebase 绑定没有观察到独立 `bind` 类业务指标；短期可用 `matrix_api_login_total{mode="firebase"}` 和 `/v1/firebase/login` HTTP 指标近似观察，长期建议增加独立 operation。
- IAP 验单可先用 `matrix_api_order_total{action="verify",channel=~"apple|google"}`，如果要区分 Apple/Google 第三方返回错误，需要补充归一化 `error_type`。
- 没有观察到 `matrix-api` 维度的 MySQL、Redis、Firebase、Google API 独立依赖指标；这些依赖目前不能像 gRPC 下游一样直接按 `dependency/error_type` 告警。
- panic/fatal 日志更适合从 SLS 日志告警或日志面板补齐，当前 Prometheus 指标盘点未发现对应 runtime error metric。

## 告警设计补充原则

### DDIA 视角：关注数据流与系统不变量

DDIA 更关注数据在系统中的流动是否可靠、完整、一致。告警设计不应只回答“服务是否还活着”，还要回答“关键数据是否按预期完成了状态转换”。

建议补充：

- 对关键业务链路建立业务成功率指标，例如游客登录、Firebase 绑定、支付验单、订单创建，避免只有 HTTP 5xx 才报警。
- 对异步链路关注消费延迟、积压、重试和写入失败，例如队列 lag、消费失败率、死信数量、外部依赖写入失败。
- 对状态转换建立不变量检查，例如“支付成功后订单最终变为已支付”、“游客绑定三方后 user_id 不丢失”。
- 指标 label 控制基数，不把 `user_id`、`order_no`、token、request_id 放进 Prometheus label；这些值保留在日志和 trace 中排查。
- 规则使用滑动窗口和错误比例，不用单点值直接触发，降低短抖动噪音。

落地优先级：先为 `matrix-api` 的登录、支付、订单三类入口补业务成功率和失败原因分布，再补跨服务链路的积压/重试类指标。

### Google SRE 视角：围绕 SLI/SLO 和错误预算

SRE 视角下，告警要为用户体验负责。能叫醒人的告警必须说明错误预算正在被快速消耗，并且处理人有明确动作。

建议补充：

- 先定义每个服务的核心 SLI：可用性、成功率、P99 延迟、核心接口业务成功率。
- P1 告警绑定 SLO 或错误预算，例如 5xx 比例、成功率跌破阈值、P99 持续超标，而不是“任何异常日志都 P1”。
- 引入多窗口 burn rate：短窗口发现快速故障，长窗口发现慢性劣化，减少单阈值误报。
- 每条 P1 告警必须可行动，飞书卡片中至少包含大盘链接、服务名、环境、触发阈值、最近日志/trace 排查入口和 Runbook。
- 非立即处理的问题降级为 P2/P3、日报或工单，例如低频错误、单用户异常、偶发外部超时。

落地优先级：`MatrixAPI5xxDetected` 后续可从 “5xx QPS > 0” 升级为“按路由的 5xx 比例 + 最小流量门槛 + 持续窗口”，再补短/长窗口 burn rate。

### 凤凰架构视角：云原生韧性与可观测闭环

凤凰架构强调可观测性服务于故障隔离、故障恢复和架构韧性。告警要能把入口、依赖、资源、发布变更串成一条排障路径。

建议补充：

- 指标、日志、链路追踪互相跳转：Grafana 面板能定位到 route/service，再通过 `x-request-id` 查 SLS 和 trace。
- 按服务边界拆分责任标签：`service`、`env`、`severity`、`route`、`dependency`，让通知策略能稳定路由到对应责任人。
- 区分入口故障和依赖故障：`matrix-api` 5xx 上升时，同时观察 `user-srv`/`order-srv` gRPC 错误、DB/Redis、Firebase/Google 等外部依赖。
- 关注韧性机制本身：超时、重试、熔断、限流、降级命中率都应有指标和告警。
- 发布后自动观察：灰度/生产部署后固定观察 5xx、P99、panic/error、核心接口成功率和 Pod 重启。

落地优先级：先给 `matrix-api` 大盘补“依赖错误率/超时”和“发布后健康观察”面板，再让告警卡片带上对应 panel URL。

### 告警分层：先症状，后原因

三种视角合在一起，`matrix-api` 的告警可以按“用户症状 -> 系统原因 -> 处置线索”分层：

1. 症状告警：直接代表用户体验受损，例如可用性下降、核心接口成功率下降、P99 超预算、支付/登录业务失败率上升。这类才应该进入 P1/P2 飞书群。
2. 原因告警：解释症状的可能来源，例如依赖超时、DB/Redis 错误、Pod 重启、panic、队列积压、限流/熔断命中。这类默认 P2，只有影响面足够大时升 P1。
3. 线索指标：帮助排障但不单独叫醒人，例如单个用户失败、低频错误日志、短时间 QPS 波动、少量外部依赖 4xx。这类进入大盘、日报或工单。

这样做的目标是避免“每个错误都报警”，同时保证真正影响用户的故障能第一时间被叫醒。

### 建议补充的告警清单

优先补下面这些规则，顺序按对用户体验和排障闭环的价值排序：

| 规则名 | 触发条件建议 | 级别 | 来源视角 | 说明 |
| --- | --- | --- | --- | --- |
| `MatrixAPIAvailabilityBurnRate` | 5xx 比例超过 SLO burn rate，且请求量超过最小门槛 | P1 | Google SRE | 替代单纯 `5xx QPS > 0`，避免低流量误报 |
| `MatrixAPIHighLatencyBurnRate` | P99 或 P95 延迟持续超过延迟预算 | P1/P2 | Google SRE | 以用户等待时间为核心，短窗口 P1、长窗口 P2 |
| `MatrixAPICoreBusinessFailureRate` | 登录、用户绑定、订单创建、验单等核心业务成功率低于阈值 | P1/P2 | DDIA | 发现“HTTP 成功但业务失败”的数据流问题 |
| `MatrixAPIDependencyErrorRateHigh` | `user-srv`、`order-srv`、DB、Redis、Firebase 等依赖错误率/超时率升高 | P2 | 凤凰架构 | 给症状告警提供根因方向，可按依赖标签路由 |
| `MatrixAPIPanicOrFatalLogDetected` | panic/fatal 日志持续出现，或 5 分钟内超过阈值 | P1/P2 | 凤凰架构 | panic 伴随 5xx 时 P1，单点低频可 P2 |
| `MatrixAPIPodRestartSpike` | Pod 重启数、OOMKilled、CrashLoopBackOff 异常 | P2 | 凤凰架构 | 用于定位资源、发布、配置类问题 |
| `MatrixAPIAsyncBacklogHigh` | 消息积压、消费延迟、重试次数、死信数超过阈值 | P2 | DDIA | 适合后续异步链路，关注最终一致性是否失效 |
| `MatrixAPIErrorBudgetTrend` | 1h/6h/24h 错误预算消耗持续偏高 | P2/日报 | Google SRE | 不一定立刻叫醒，但用于稳定性治理 |

### PromQL 规则形态建议

没有确认最终指标名之前，规则先按形态设计，接入时再替换成实际 metric：

```promql
# 可用性：按路由统计 5xx 比例，并设置最小流量门槛
(
  sum by (route) (rate(http_requests_total{service="matrix-api",status=~"5.."}[5m]))
/
  sum by (route) (rate(http_requests_total{service="matrix-api"}[5m]))
) > 0.02
and
sum by (route) (rate(http_requests_total{service="matrix-api"}[5m])) > 1
```

```promql
# 延迟：按路由统计 P99，避免只看全局均值
histogram_quantile(
  0.99,
  sum by (route, le) (rate(http_request_duration_seconds_bucket{service="matrix-api"}[5m]))
) > 1
```

```promql
# 依赖：区分 dependency 和 error_type，便于飞书卡片给出排障方向
sum by (dependency, error_type) (
  rate(matrix_api_dependency_requests_total{service="matrix-api",result="error"}[5m])
)
/
sum by (dependency, error_type) (
  rate(matrix_api_dependency_requests_total{service="matrix-api"}[5m])
) > 0.05
```

```promql
# 业务成功率：关注数据状态是否按预期完成转换
1 - (
  sum by (operation) (rate(matrix_api_business_events_total{result="success"}[5m]))
/
  sum by (operation) (rate(matrix_api_business_events_total[5m]))
) > 0.03
```

### 多窗口 burn rate 建议

如果先按 99.9% 月度可用性 SLO 估算，可采用双窗口策略：

| 目标 | 窗口 | 建议级别 | 用途 |
| --- | --- | --- | --- |
| 快速故障 | `5m` + `1h` | P1 | 大面积故障、发布事故、依赖雪崩 |
| 慢性劣化 | `30m` + `6h` | P2 | 小比例持续失败、容量逼近、外部依赖不稳定 |
| 治理趋势 | `2h` + `24h` | 日报/工单 | 错误预算长期消耗偏高 |

后续如果为 `matrix-api` 明确定义了 SLO，应把阈值从固定错误率改成 SLO 对应的 burn rate，而不是长期维护拍脑袋阈值。

### Label 与基数约束

告警相关指标建议保留这些低基数标签：

- `service`：固定为 `matrix-api`。
- `env`：`prod`、`gray`。
- `route`：使用路由模板，例如 `/api/user/firebase-login`，不要使用真实 URL。
- `operation`：业务动作，例如 `visitor_login`、`firebase_bind`、`order_create`、`iap_verify`。
- `dependency`：依赖名称，例如 `user-srv`、`order-srv`、`mysql`、`redis`、`firebase`。
- `error_type`：归一化错误类型，例如 `timeout`、`grpc_error`、`db_error`、`business_reject`。

不要放入 Prometheus label 的字段：

- `user_id`、`order_no`、`request_id`、token、手机号、邮箱、设备 ID。
- 原始错误文本、SQL、URL query、第三方返回全文。

这些高基数字段应留在 SLS 日志或 trace 中，告警卡片只提供跳转入口和必要的过滤条件。

### 飞书卡片信息补强

为了让 P1 告警具备可行动性，卡片建议至少包含：

- 发生了什么：规则名、服务、环境、级别、当前值、阈值、持续时间。
- 影响哪里：route、operation、dependency、错误率、请求量、受影响窗口。
- 为什么可能发生：最近一次发布、Pod 重启、panic/fatal、依赖错误率、限流/熔断命中。
- 去哪里看：绑定 panel URL、服务总览大盘、SLS 查询入口、trace 查询入口、Runbook。
- 谁在处理：保留 `我来处理`，认领后在原消息话题回复处理人和时间。

其中“AI 归因”可以优先围绕这些线索生成结论：发布变更、错误率突增的 route、Top dependency、Top error_type、Pod 重启、panic 日志、SLS 最新错误样本。

### 噪音控制规则

新增规则前建议先过一遍噪音检查：

1. 是否直接影响用户体验或关键数据流？如果不是，默认不要 P1。
2. 是否有最小流量门槛？低流量下的 1 个错误不应触发全员告警。
3. 是否有持续时间或双窗口？瞬时抖动不应直接叫醒人。
4. 是否能指向明确动作？处理人看到后至少知道先看哪个大盘、查哪个日志、回滚哪个发布或找哪个依赖 owner。
5. 是否有恢复通知？恢复卡片能帮助值班人关闭事件，不需要人工猜测。

## ACK 部署

| 项目 | 值 |
| --- | --- |
| 集群连接 | `metis-prod` |
| Namespace | `prod` |
| Deployment | `matrix-alert-forwarder` |
| Service | `matrix-alert-forwarder-svc` |
| Service 端口 | `8080` |
| Ingress 域名 | `metis.hnivj.com` |
| Ingress 路径 | `/alert-forwarder(/|$)(.*)` |
| 容器端口 | `8080` |

Ingress 会把：

```text
/alert-forwarder/grafana/feishu
```

重写到 forwarder 容器内：

```text
/grafana/feishu
```

## 云效与镜像

| 项目 | 值 |
| --- | --- |
| Codeup 仓库 | `matrix-alert-forwarder` |
| Pipeline | `matrix-alert-forwarder-build` |
| Pipeline ID | `4908679` |
| ACR 仓库 | `registry.cn-hangzhou.aliyuncs.com/awj_prod/matrix-alert-forwarder` |
| 镜像 Tag | 使用云效 `${DATETIME}` |

流水线主要步骤：

1. 拉取 `master` 分支代码。
2. 执行 Go 测试。
3. 使用云效原生 `DockerBuildPush` 构建并推送镜像。
4. 使用 `KubectlSetImage` 更新 ACK `prod/matrix-alert-forwarder`。

## 告警卡片交互

Grafana 触发告警后：

1. Grafana Contact Point 调用 forwarder。
2. Forwarder 解析 Grafana webhook payload。
3. Forwarder 使用飞书应用机器人 API 发送交互卡片。
4. 卡片包含：
   - `我来处理`
   - `AI 归因`
   - `打开大盘`
5. 卡片会展示 Grafana payload 中的关键上下文：
   - `当前值`
   - `category`
   - `route`
   - `target`
   - `mode/action/channel`
   - `pod`
6. 用户点击 `我来处理` 后，飞书发送 `card.action.trigger` 回调到 forwarder。
7. Forwarder 解析点击人和 `open_message_id`。
8. Forwarder 调用飞书消息回复 API，在原告警话题下回复认领信息。
9. 用户点击 `AI 归因` 后，forwarder 会把 `category`、`route`、`target`、`operation`、`pod`、`current_value` 等上下文传给归因器；优先调用 `COPILOT_RUNNER_URL` 指向的 `matrix-agent-runner`（HTTP 调 `POST /agents/attribution-agent/invoke`），其次回退到 `COPILOT_RUNNER_COMMAND` 本地命令，最后回退到内置规则型归因，并回复原告警话题。

## 常用验证命令

健康检查：

```bash
curl -i https://metis.hnivj.com/alert-forwarder/healthz
```

查看 ACK 镜像版本：

```bash
kubectl -n prod get deploy matrix-alert-forwarder \
  -o jsonpath='{.spec.template.spec.containers[*].image}{"\n"}{.status.readyReplicas}{"/"}{.status.replicas}{" ready\n"}'
```

查看 Grafana 规则状态：

```bash
curl -s \
  -H "Authorization: Bearer ${GRAFANA_TOKEN}" \
  https://grafana-cn-0564r6g2h02.grafana.aliyuncs.com/api/prometheus/grafana/api/v1/rules
```

## 安全注意

- 不要把 `FEISHU_APP_SECRET`、`FEISHU_ENCRYPT_KEY`、`FEISHU_VERIFICATION_TOKEN`、`GRAFANA_FORWARDER_TOKEN`、`GRAFANA_TOKEN` 写入 Git。
- 如果这些值曾经出现在聊天、截图或日志中，建议在对应平台轮换。
- `FEISHU_CHAT_ID`、`FEISHU_APP_ID`、Grafana UID、Pipeline ID、K8s 资源名可以记录在文档中。
