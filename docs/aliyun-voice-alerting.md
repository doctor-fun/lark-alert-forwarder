# 阿里云电话告警接入 runbook

本文记录 `matrix-alert-forwarder` 接入阿里云语音通知的完整步骤。当前实现只对 **P0 / critical** 等白名单严重度拨电话：告警先正常进飞书群，超过 L1 阈值仍未处理时给当前告警负责人拨 TTS 电话；超过 L2 阈值仍未处理时在群里 @ leader，并给 leader 拨兜底电话。

> 安全原则：AccessKey / Secret / 真实手机号只放在阿里云 RAM、K8s Secret 或后台数据库里，不写入 Git、截图、文档和工单。

## 1. 前置条件

- `matrix-alert-forwarder` 已接入 `ALERT_BACKEND_URL`，并启动 escalator。
- `matrix-backend` oncall 成员里维护了告警负责人和 L2 leader 的 `phone` 字段。
- 阿里云账号已开通 **语音服务**，并能申请 **语音通知** 的文本转语音模板。
- 已准备一个专用 RAM 用户，用于 forwarder 运行期调用 `dyvmsapi`。

相关入口：

- 语音服务控制台：https://dyvms.console.aliyun.com/
- RAM 控制台：https://ram.console.aliyun.com/
- 语音服务 API 文档：https://help.aliyun.com/zh/vms/developer-reference/api-dyvmsapi-2017-05-25-singlecallbytts

## 2. 阿里云控制台配置

### 2.1 开通语音服务

进入阿里云 **语音服务** 控制台，按页面提示开通服务。如果子账号点击"立即开通"是灰色，或提示"没有签署权限"，需要主账号或管理员给开通人临时授权：

```json
{
  "Version": "1",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["bss:ModifyAgreementRecord"],
      "Resource": "*"
    }
  ]
}
```

这是服务协议签署权限，只用于开通阶段。服务开通后，forwarder 运行期不需要这个权限。

### 2.2 创建 TTS 模板

在控制台进入：

```text
语音通知 -> 文本转语音模板 / TTS 模板 -> 添加模板
```

推荐模板内容：

```text
matrix 告警通知，告警名称 ${alertname}，严重级别 ${severity}，请尽快处理
```

填写建议：

- 场景选择：运维告警、故障通知、系统通知等最贴近的类型。
- 变量名必须包含 `alertname` 和 `severity`，代码拨号时只会传这两个参数。
- 模板内容不要包含用户手机号、订单号、token、SQL、URL query 等敏感信息。
- 审核通过后记录模板 ID，形如 `TTS_xxxxxxx`，后续作为 `ALIYUN_VOICE_TTS_CODE`。

### 2.3 准备运行期 RAM 权限

建议新建专用 RAM 用户，例如 `matrix-alert-forwarder-voice`，只给运行期最小权限：

```json
{
  "Version": "1",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "dyvms:SingleCallByTts",
        "dyvms:QueryCallDetailByCallId"
      ],
      "Resource": "*"
    }
  ]
}
```

说明：

- `dyvms:SingleCallByTts`：发起电话告警，必需。
- `dyvms:QueryCallDetailByCallId`：排查通话状态时使用，建议保留。
- 不建议直接给 `AliyunDyvmsFullAccess` 作为生产运行权限。
- AK/SK 创建后只写入 K8s Secret，不要提交到仓库。

## 3. K8s Secret 与 forwarder ENV

### 3.1 创建 Secret

用本地安全终端创建 Secret，避免明文落盘：

```bash
kubectl -n prod create secret generic matrix-alert-forwarder-aliyun-voice \
  --from-literal=ALIYUN_VOICE_ACCESS_KEY_ID='<RAM_ACCESS_KEY_ID>' \
  --from-literal=ALIYUN_VOICE_ACCESS_KEY_SECRET='<RAM_ACCESS_KEY_SECRET>' \
  --from-literal=ALIYUN_VOICE_TTS_CODE='TTS_xxxxxxx' \
  --dry-run=client -o yaml | kubectl apply -f -
```

如果已经有同名 Secret，重复执行上面的命令会更新。

### 3.2 给 Deployment 注入环境变量

`matrix-alert-forwarder` 需要以下环境变量：

```yaml
env:
  - name: ALIYUN_VOICE_ACCESS_KEY_ID
    valueFrom:
      secretKeyRef:
        name: matrix-alert-forwarder-aliyun-voice
        key: ALIYUN_VOICE_ACCESS_KEY_ID
  - name: ALIYUN_VOICE_ACCESS_KEY_SECRET
    valueFrom:
      secretKeyRef:
        name: matrix-alert-forwarder-aliyun-voice
        key: ALIYUN_VOICE_ACCESS_KEY_SECRET
  - name: ALIYUN_VOICE_TTS_CODE
    valueFrom:
      secretKeyRef:
        name: matrix-alert-forwarder-aliyun-voice
        key: ALIYUN_VOICE_TTS_CODE

  # 可选：默认 cn-hangzhou
  - name: ALIYUN_VOICE_REGION
    value: cn-hangzhou
  # 可选：主叫显示号，未配置则用阿里云默认主显号
  - name: ALIYUN_VOICE_CALLED_SHOW_NUMBER
    value: ""
  # 可选：默认 https://dyvmsapi.aliyuncs.com
  - name: ALIYUN_VOICE_ENDPOINT
    value: https://dyvmsapi.aliyuncs.com
```

也可以直接用 `envFrom` 引入 Secret，但建议显式 `secretKeyRef`，便于 review 哪些敏感配置被 forwarder 使用。

改完后重启：

```bash
kubectl -n prod rollout restart deploy/matrix-alert-forwarder
kubectl -n prod rollout status deploy/matrix-alert-forwarder --timeout=120s
```

## 4. Oncall 手机号配置

电话会拨给 backend 返回的升级目标手机号：L1 是当前告警负责人，L2 是 leader。

需要确认：

- Oncall 成员表里当前告警负责人和 leader 有 `phone`。
- 手机号建议使用国际格式，例如 `+8613800138000`。forwarder 会自动归一为阿里云需要的纯数字 `8613800138000`。
- forwarder 仍会按 `VOICE_ALERT_SEVERITIES` / `VOICE_ALERT_SEVERITIES_BY_SERVICE` 控制是否拨号，默认只拨 P0 / critical。

可在 CMS 的 Oncall 管理页面维护成员手机号，或通过 backend oncall admin 接口更新成员信息。

## 5. 工作机制

电话告警路径：

```text
Grafana FIRING
  -> matrix-alert-forwarder 发飞书告警卡片
  -> matrix-backend 创建 / 去重 incident 并选择负责人
  -> 超过 l1_after 未认领：飞书 thread @ L1
  -> forwarder 调 Aliyun SingleCallByTts 给当前负责人拨电话
  -> 超过 l2_after 未认领：飞书 thread @ L2 leader
  -> forwarder 调 Aliyun SingleCallByTts 给 L2 leader 拨电话
```

关键行为：

- 缺少 `ALIYUN_VOICE_ACCESS_KEY_ID` / `ALIYUN_VOICE_ACCESS_KEY_SECRET` / `ALIYUN_VOICE_TTS_CODE` 任意一个时，电话功能关闭，但飞书告警和升级 @ 仍正常。
- 只有命中电话严重度白名单的告警会拨电话，默认 P0 / critical；L1 拨当前负责人，L2 拨 leader 兜底。
- 每通电话 15s 超时；拨号失败只记录日志，不阻塞 `mark_escalated`。
- 日志里手机号会被掩码，成功日志包含 `call_id`，失败日志包含阿里云返回的 `requestId` / 错误码。

## 6. 验证

### 6.1 启动日志

重启后查看 forwarder 日志：

```bash
kubectl -n prod logs deploy/matrix-alert-forwarder --since=10m | grep 'aliyun voice'
```

期望启用成功：

```text
aliyun voice: enabled tts_code=TTS_xxxxxxx region=cn-hangzhou
```

如果看到：

```text
aliyun voice: disabled (set ALIYUN_VOICE_ACCESS_KEY_ID/_SECRET/_TTS_CODE to enable)
```

说明 Secret 或 Deployment env 还没接上。

### 6.2 触发 L1 / L2 验证

建议先在灰度或测试告警上验证：

1. 选一条低风险测试告警，确认它能进入飞书群。
2. 将对应服务的当前负责人和 L2 leader 配成自己的 open_id 和手机号。
3. 临时把 `l1_after` / `l2_after` 调短，例如 `1m` / `2m`。
4. 不点击"我来处理"，等待 L1 / L2 升级。
5. 观察飞书 thread 出现 L1 / L2 升级消息，并确认手机收到 TTS 电话。
6. 查看日志：

```bash
kubectl -n prod logs deploy/matrix-alert-forwarder --since=15m | grep 'voice call'
```

成功示例：

```text
alert escalator: L1 voice call ok incident=123 phone=861****8000 call_id=...
```

失败示例：

```text
alert escalator: L2 voice call failed incident=123 phone=861****8000: aliyun voice call failed http=200 code=... message=... requestId=...
```

## 7. 常见问题

| 现象 | 常见原因 | 处理 |
| --- | --- | --- |
| 控制台无法点击"立即开通" | 子账号缺少协议签署权限 | 让管理员临时授权 `bss:ModifyAgreementRecord`，或主账号开通 |
| 找不到模板入口 | 误进了"语音文件管理"或"语音验证码" | 进入"语音通知 -> 文本转语音模板 / TTS 模板" |
| 日志显示 voice disabled | 三个必填 env 没配齐，或 Deployment 未引用 Secret | 检查 `ALIYUN_VOICE_ACCESS_KEY_ID` / `_SECRET` / `_TTS_CODE` |
| `isv.INVALID_PARAMETERS` | 手机号格式、模板变量、TtsCode 不对 | 确认手机号带国家码，模板变量是 `alertname` / `severity` |
| `isv.TEMPLATE_MISSING_PARAMETERS` | TTS 模板变量和传参不一致 | 模板只使用 `${alertname}`、`${severity}` |
| 电话没响但飞书升级有 @ | 当前负责人 / leader 没配手机号，或手机号为空 | 在 Oncall 成员信息里补 `phone` |
| 拨号权限失败 | RAM 用户权限不足 | 确认有 `dyvms:SingleCallByTts` |

## 8. 回滚

电话告警是独立增强，不影响飞书主链路。需要关闭时任选其一：

```bash
# 快速关闭：删掉 TTS_CODE env 或把 Secret key 置空后重启
kubectl -n prod rollout restart deploy/matrix-alert-forwarder
```

或从 Deployment 中移除所有 `ALIYUN_VOICE_*` env。重启后日志应回到：

```text
aliyun voice: disabled (set ALIYUN_VOICE_ACCESS_KEY_ID/_SECRET/_TTS_CODE to enable)
```

关闭后 L1 / L2 仍会在飞书 thread @ 升级目标，只是不再拨电话。
