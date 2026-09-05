package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// descriptionTableDisplayNames 把 BI / CronJob 里常见英文字段映射成中文表头。
var descriptionTableDisplayNames = map[string]string{
	"cohort_date":   "日期",
	"cohort_hour":   "小时",
	"account_id":    "账号 ID",
	"account_name":  "账号",
	"campaign_id":   "Campaign ID",
	"campaign_name": "Campaign",
	"link_clicks":   "点击",
	"lpv":           "LPV",
	"arrival_pct":   "到达率%",
	"ctr_pct":       "CTR%",
	"impressions":   "曝光",
	"spend":         "消耗",
}

var descriptionTableNumberCols = map[string]bool{
	"cohort_hour": true,
	"link_clicks": true,
	"lpv":         true,
	"arrival_pct": true,
	"ctr_pct":     true,
	"impressions": true,
	"spend":       true,
}

var tableColumnKeySanitizer = regexp.MustCompile(`[^a-zA-Z0-9_]+`)

// splitDescriptionTable 从告警详情里抽出 TSV 表（首行表头 + 后续数据行），
// 返回去掉表格后的正文，以及可直接塞进飞书卡片的 table 元素。
// 识别不到合法表格时 ok=false，调用方保持原文展示。
func splitDescriptionTable(description string) (prose string, table map[string]any, ok bool) {
	lines := strings.Split(strings.ReplaceAll(description, "\r\n", "\n"), "\n")
	headerIdx := -1
	var headers []string
	for i, line := range lines {
		cols := splitTSVLine(line)
		if len(cols) < 3 {
			continue
		}
		if !looksLikeTSVHeader(cols) {
			continue
		}
		headerIdx = i
		headers = cols
		break
	}
	if headerIdx < 0 {
		return description, nil, false
	}

	keys := make([]string, len(headers))
	used := map[string]int{}
	for i, h := range headers {
		key := sanitizeTableColumnKey(h)
		if key == "" {
			key = fmt.Sprintf("col_%d", i)
		}
		if n, exists := used[key]; exists {
			used[key] = n + 1
			key = fmt.Sprintf("%s_%d", key, n+1)
		} else {
			used[key] = 1
		}
		keys[i] = key
	}

	rows := make([]map[string]any, 0, 10)
	lastDataIdx := headerIdx
	for i := headerIdx + 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			// 空行结束表格，后面若还有正文并回 prose 尾部。
			if len(rows) > 0 {
				break
			}
			continue
		}
		cols := splitTSVLine(line)
		if len(cols) < 2 || !strings.Contains(line, "\t") {
			break
		}
		row := map[string]any{}
		for j, key := range keys {
			val := ""
			if j < len(cols) {
				val = strings.TrimSpace(cols[j])
			}
			if descriptionTableNumberCols[headers[j]] {
				if n, err := strconv.ParseFloat(val, 64); err == nil {
					row[key] = n
					continue
				}
			}
			// 过长文案截断，避免把卡片撑爆。
			row[key] = truncateRunes(val, 48)
		}
		rows = append(rows, row)
		lastDataIdx = i
		if len(rows) >= 20 {
			break
		}
	}
	if len(rows) == 0 {
		return description, nil, false
	}

	columns := make([]map[string]any, 0, len(headers))
	for i, h := range headers {
		col := map[string]any{
			"name":             keys[i],
			"display_name":     firstNonEmpty(descriptionTableDisplayNames[h], h),
			"width":            "auto",
			"horizontal_align": "left",
		}
		if descriptionTableNumberCols[h] {
			col["data_type"] = "number"
			col["horizontal_align"] = "right"
			if strings.HasSuffix(h, "_pct") {
				col["format"] = map[string]any{"precision": 1}
			}
		} else {
			col["data_type"] = "text"
		}
		// 长 ID / 名称列给固定宽度，表格更稳。
		switch h {
		case "account_id", "campaign_id":
			col["width"] = "140px"
		case "account_name", "campaign_name":
			col["width"] = "160px"
		case "cohort_date":
			col["width"] = "100px"
		case "cohort_hour", "link_clicks", "lpv", "arrival_pct", "ctr_pct":
			col["width"] = "80px"
		}
		columns = append(columns, col)
	}

	proseParts := make([]string, 0, headerIdx+2)
	for i := 0; i < headerIdx; i++ {
		proseParts = append(proseParts, lines[i])
	}
	for i := lastDataIdx + 1; i < len(lines); i++ {
		proseParts = append(proseParts, lines[i])
	}
	prose = strings.TrimSpace(strings.Join(proseParts, "\n"))
	if prose == "" {
		prose = "详见下表"
	}

	pageSize := len(rows)
	if pageSize > 10 {
		pageSize = 10
	}
	table = map[string]any{
		"tag":                 "table",
		"page_size":           pageSize,
		"row_height":          "low",
		"freeze_first_column": false,
		"header_style": map[string]any{
			"text_align":       "left",
			"text_size":        "normal",
			"background_style": "grey",
			"text_color":       "default",
			"bold":             true,
			"lines":            1,
		},
		"columns": columns,
		"rows":    rows,
	}
	return prose, table, true
}

func splitTSVLine(line string) []string {
	if !strings.Contains(line, "\t") {
		return nil
	}
	parts := strings.Split(line, "\t")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

func looksLikeTSVHeader(cols []string) bool {
	hits := 0
	for _, c := range cols {
		if _, ok := descriptionTableDisplayNames[c]; ok {
			hits++
		}
	}
	// 至少命中 2 个已知字段，避免把普通正文误判成表头。
	return hits >= 2
}

func sanitizeTableColumnKey(raw string) string {
	key := tableColumnKeySanitizer.ReplaceAllString(strings.TrimSpace(raw), "_")
	key = strings.Trim(key, "_")
	if key == "" {
		return ""
	}
	if key[0] >= '0' && key[0] <= '9' {
		key = "c_" + key
	}
	return key
}
