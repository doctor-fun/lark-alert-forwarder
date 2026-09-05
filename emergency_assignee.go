package main

import (
	"hash/fnv"
	"strings"
)

// emergency_assignee.go：当 backend / picker 没给 assignee 时的最后一道兜底。
//
// 设计要求：
//   1. 同一条告警（按 fingerprint）反复触发命中同一个人，不要每次随机换人；
//   2. forwarder 进程重启后选人结果保持一致（不用内存计数器）；
//   3. 列表为空时不 panic、不返回空字符串触发其它分支异常 → 调用方应该先判长度。
//
// 用 fnv32 哈希 + 取模实现确定性轮询。fnv 速度快、分布均匀、无外部依赖。

// splitOpenIDList 把 "ou_a, ou_b ,ou_c" 切成 ["ou_a","ou_b","ou_c"]；
// 容忍空格 / 空段。返回 nil 表示用户没配；调用方据此走"无兜底"分支。
func splitOpenIDList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		v := strings.TrimSpace(p)
		if v == "" {
			continue
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// pickEmergencyAssignee：从 list 中按 fingerprint 哈希挑一个 open_id。
// list 长度为 0 时返回空串；调用方应该在调用前判长度并做 log。
func pickEmergencyAssignee(list []string, fingerprint string) string {
	if len(list) == 0 {
		return ""
	}
	if len(list) == 1 {
		return list[0]
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(fingerprint))
	idx := int(h.Sum32() % uint32(len(list)))
	return list[idx]
}

// fingerprintForEmergency 给 grafanaWebhook 算一个稳定的 fingerprint 供 emergency
// 兜底用。优先用 commonLabels 拼，落到第一条 alert 的 labels；都没有就拿 title 顶。
//
// 不与 backend 算的 sha1 fingerprint 强一致 —— backend 那边算的 fingerprint
// 用于 incident 去重，要求语义稳定（带 service/env）；这里只是 emergency 的
// 选人种子，能保证"同条告警 → 同一人"就够。
func (g grafanaWebhook) fingerprintForEmergency() string {
	if v := commonLabelsKey(g); v != "" {
		return v
	}
	if len(g.Alerts) > 0 {
		if v := alertLabelsKey(g.Alerts[0]); v != "" {
			return v
		}
	}
	return strings.TrimSpace(g.Title)
}

func commonLabelsKey(g grafanaWebhook) string {
	keys := []string{"alertname", "service", "env", "namespace", "instance", "severity"}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if v := strings.TrimSpace(g.CommonLabels[k]); v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, "|")
}

func alertLabelsKey(a grafanaAlert) string {
	keys := []string{"alertname", "service", "env", "namespace", "instance", "severity"}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if v := strings.TrimSpace(a.Labels[k]); v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, "|")
}
