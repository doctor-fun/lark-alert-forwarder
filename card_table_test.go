package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSplitDescriptionTable_DirtyAccountTSV(t *testing.T) {
	desc := strings.Join([]string{
		"[观察] TikTok 异常账号×小时：近 3 小时 clicks≥300 且到达率<20%",
		"规则: ads_ad_delivery_hour_hi，platform=tiktok",
		"动作: 对照健康账户 CTR/到达率，必要时降预算",
		"",
		"cohort_date\tcohort_hour\taccount_id\taccount_name\tlink_clicks\tlpv\tarrival_pct\tctr_pct",
		"2026-07-16\t13\t7650036439154851847\t朱存银-tt-2-价值\t4200\t480\t11.4\t45.2",
		"2026-07-16\t14\t7650078346950049813\tJY-avj-boom-666\t1800\t210\t11.7\t38.0",
		"",
		"这是路由验收测试，可忽略。",
	}, "\n")

	prose, table, ok := splitDescriptionTable(desc)
	if !ok {
		t.Fatal("expected table")
	}
	if !strings.Contains(prose, "对照健康账户") {
		t.Fatalf("prose missing lead text: %q", prose)
	}
	if !strings.Contains(prose, "可忽略") {
		t.Fatalf("prose missing trailing text: %q", prose)
	}
	if strings.Contains(prose, "cohort_date") || strings.Contains(prose, "7650036439154851847") {
		t.Fatalf("prose should not keep TSV: %q", prose)
	}
	if table["tag"] != "table" {
		t.Fatalf("tag=%v", table["tag"])
	}
	rows, _ := table["rows"].([]map[string]any)
	if len(rows) != 2 {
		t.Fatalf("rows=%d", len(rows))
	}
	if rows[0]["link_clicks"] != float64(4200) {
		t.Fatalf("link_clicks=%v want number 4200", rows[0]["link_clicks"])
	}
	raw, _ := json.Marshal(table)
	body := string(raw)
	for _, want := range []string{`"display_name":"账号"`, `"display_name":"到达率%"`, `"data_type":"number"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("table missing %s\n%s", want, body)
		}
	}
}

func TestSplitDescriptionTable_NoFalsePositive(t *testing.T) {
	desc := "matrix-api P99 > 1s 持续 5 分钟\n请检查下游依赖"
	if _, _, ok := splitDescriptionTable(desc); ok {
		t.Fatal("plain description should not become table")
	}
}

func TestBuildFeishuCard_EmbedsDescriptionTable(t *testing.T) {
	desc := "规则: ads_ad_delivery_di\n\n" +
		"cohort_date\taccount_id\taccount_name\tcampaign_id\tlink_clicks\tlpv\tarrival_pct\n" +
		"2026-07-15\t7650078297107464212\t示例账号\t1870759185508706\t15983\t122\t0.76\n"
	msg := buildFeishuCard(grafanaWebhook{
		Status: "firing",
		CommonLabels: map[string]string{
			"alertname": "DirtyTikTokCampaign",
			"service":   "ad-delivery",
			"env":       "prod",
			"severity":  "P1",
		},
		CommonAnnotations: map[string]string{
			"summary":     "DirtyTikTokCampaign",
			"description": desc,
		},
	}, false, cardContext{})
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, `"tag":"table"`) {
		t.Fatalf("expected embedded table:\n%s", body)
	}
	if strings.Contains(body, "15983\t122") {
		t.Fatalf("raw TSV should not remain in markdown:\n%s", body)
	}
	if !strings.Contains(body, "规则: ads_ad_delivery_di") {
		t.Fatalf("prose should remain:\n%s", body)
	}
}
