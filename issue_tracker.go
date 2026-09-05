package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// 非缺陷问题跟踪表真实状态（飞书单选选项）。
const (
	issueTrackerStatusOwnerPending  = "负责人-待处理"
	issueTrackerStatusHelperPending = "协助人-待补充"
	issueTrackerStatusPendingVerify = "已解决-待验证"
	issueTrackerStatusRejected      = "已拒绝-注明原因"
	issueTrackerStatusClosed        = "已关闭"

	defaultIssueTrackerRecordURL = "https://wishhudong.feishu.cn/base/O01Tb1mXvaRQTMs6lR0cWb9en1b?table=tbl6QKvzuEHw2K3R&view=vewI2zXFLO"
	defaultIssueTrackerTitle     = "非缺陷问题定时提醒"
)

type issueTrackerConfig struct {
	Enabled  bool
	AppToken string
	TableID  string
	ChatID   string
	Interval time.Duration
	Cooldown time.Duration
	Title    string
	Statuses []string
	URL      string

	NumberField   string
	TitleField    string
	DescField     string
	StatusField   string
	AssigneeField string
	HelperField   string
	CreatorField  string
	CreatedField  string
}

type issueTrackerRecord struct {
	RecordID       string
	Number         string
	Title          string
	Description    string
	Status         string
	AssigneeName   string
	AssigneeOpenID string
	HelperName     string
	HelperOpenID   string
	CreatorName    string
	CreatorOpenID  string
	CreatedAt      time.Time
}

type issueTrackerClient struct {
	server *server
	cfg    issueTrackerConfig
}

func issueTrackerFromEnv(s *server) *issueTrackerClient {
	cfg := issueTrackerConfig{
		AppToken: strings.TrimSpace(os.Getenv("ISSUE_TRACKER_APP_TOKEN")),
		TableID:  strings.TrimSpace(os.Getenv("ISSUE_TRACKER_TABLE_ID")),
		ChatID:   strings.TrimSpace(os.Getenv("ISSUE_TRACKER_CHAT_ID")),
		Interval: durationFromEnv("ISSUE_TRACKER_INTERVAL", time.Hour),
		Cooldown: durationFromEnv("ISSUE_TRACKER_COOLDOWN", time.Hour),
		Title:    envOrDefault("ISSUE_TRACKER_TITLE", defaultIssueTrackerTitle),
		URL:      envOrDefault("ISSUE_TRACKER_RECORD_URL", defaultIssueTrackerRecordURL),

		NumberField:   envOrDefault("ISSUE_TRACKER_NUMBER_FIELD", "问题编号"),
		TitleField:    envOrDefault("ISSUE_TRACKER_TITLE_FIELD", "问题标题"),
		DescField:     envOrDefault("ISSUE_TRACKER_DESC_FIELD", "问题描述"),
		StatusField:   envOrDefault("ISSUE_TRACKER_STATUS_FIELD", "问题状态"),
		AssigneeField: envOrDefault("ISSUE_TRACKER_ASSIGNEE_FIELD", "负责人（问题源头负责人）"),
		HelperField:   envOrDefault("ISSUE_TRACKER_HELPER_FIELD", "协助人"),
		CreatorField:  envOrDefault("ISSUE_TRACKER_CREATOR_FIELD", "创建人"),
		CreatedField:  envOrDefault("ISSUE_TRACKER_CREATED_AT_FIELD", "创建时间"),
		Statuses: []string{
			issueTrackerStatusOwnerPending,
			issueTrackerStatusHelperPending,
			issueTrackerStatusPendingVerify,
		},
	}
	if value := strings.TrimSpace(os.Getenv("ISSUE_TRACKER_STATUSES")); value != "" {
		cfg.Statuses = splitTrimmedList(value)
	}
	if value := strings.TrimSpace(os.Getenv("ISSUE_TRACKER_ENABLED")); value != "" {
		cfg.Enabled = parseBoolValue("ISSUE_TRACKER_ENABLED", value, false)
	} else {
		cfg.Enabled = cfg.AppToken != "" && cfg.TableID != "" && cfg.ChatID != ""
	}
	if !cfg.Enabled || cfg.AppToken == "" || cfg.TableID == "" || cfg.ChatID == "" {
		return nil
	}
	return &issueTrackerClient{server: s, cfg: cfg}
}

func (c *issueTrackerClient) FetchRecords(ctx context.Context) ([]issueTrackerRecord, error) {
	if c == nil || c.cfg.AppToken == "" || c.cfg.TableID == "" {
		return nil, nil
	}
	token, err := c.server.tenantAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	fieldNames := []string{
		c.cfg.NumberField,
		c.cfg.TitleField,
		c.cfg.DescField,
		c.cfg.StatusField,
		c.cfg.AssigneeField,
		c.cfg.HelperField,
		c.cfg.CreatorField,
		c.cfg.CreatedField,
	}
	records := []issueTrackerRecord{}
	pageToken := ""
	for {
		q := url.Values{}
		q.Set("page_size", "500")
		q.Set("user_id_type", "open_id")
		if fields, err := json.Marshal(nonEmptyStrings(fieldNames)); err == nil {
			q.Set("field_names", string(fields))
		}
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}
		path := fmt.Sprintf("/open-apis/bitable/v1/apps/%s/tables/%s/records?%s",
			url.PathEscape(c.cfg.AppToken), url.PathEscape(c.cfg.TableID), q.Encode())
		var resp struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
			Data struct {
				Items []struct {
					RecordID string         `json:"record_id"`
					Fields   map[string]any `json:"fields"`
				} `json:"items"`
				HasMore   bool   `json:"has_more"`
				PageToken string `json:"page_token"`
			} `json:"data"`
		}
		if err := c.server.callFeishuAPI(ctx, http.MethodGet, path, token, nil, &resp); err != nil {
			return nil, err
		}
		if resp.Code != 0 {
			return nil, fmt.Errorf("list issue tracker records failed: code=%d msg=%s", resp.Code, resp.Msg)
		}
		for _, item := range resp.Data.Items {
			fields := item.Fields
			if fields == nil {
				fields = map[string]any{}
			}
			rec := issueTrackerRecord{
				RecordID:       item.RecordID,
				Number:         stringFromAny(fields[c.cfg.NumberField]),
				Title:          stringFromAny(fields[c.cfg.TitleField]),
				Description:    stringFromAny(fields[c.cfg.DescField]),
				Status:         stringFromAny(fields[c.cfg.StatusField]),
				AssigneeOpenID: bitableUserOpenID(fields[c.cfg.AssigneeField]),
				AssigneeName: firstNonEmpty(
					bitableUserName(fields[c.cfg.AssigneeField]),
					bitableUserOpenID(fields[c.cfg.AssigneeField]),
				),
				HelperOpenID: bitableUserOpenID(fields[c.cfg.HelperField]),
				HelperName: firstNonEmpty(
					bitableUserName(fields[c.cfg.HelperField]),
					bitableUserOpenID(fields[c.cfg.HelperField]),
				),
				CreatorOpenID: bitableUserOpenID(fields[c.cfg.CreatorField]),
				CreatorName: firstNonEmpty(
					bitableUserName(fields[c.cfg.CreatorField]),
					bitableUserOpenID(fields[c.cfg.CreatorField]),
				),
				CreatedAt: bitableTime(fields[c.cfg.CreatedField]),
			}
			records = append(records, rec)
		}
		if !resp.Data.HasMore || strings.TrimSpace(resp.Data.PageToken) == "" {
			break
		}
		pageToken = resp.Data.PageToken
	}
	return records, nil
}

func issueTrackerOpenDue(records []issueTrackerRecord, statuses []string) []issueTrackerRecord {
	allowed := map[string]struct{}{}
	for _, status := range statuses {
		status = strings.TrimSpace(status)
		if status != "" {
			allowed[status] = struct{}{}
		}
	}
	due := make([]issueTrackerRecord, 0, len(records))
	seen := map[string]struct{}{}
	for _, rec := range records {
		key := strings.TrimSpace(rec.RecordID)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		status := strings.TrimSpace(rec.Status)
		if len(allowed) > 0 {
			if _, ok := allowed[status]; !ok {
				continue
			}
		} else if status == "" || status == issueTrackerStatusClosed || status == issueTrackerStatusRejected {
			continue
		}
		if strings.TrimSpace(rec.Title) == "" && strings.TrimSpace(rec.Number) == "" {
			continue
		}
		seen[key] = struct{}{}
		due = append(due, rec)
	}
	return due
}

// issueTrackerMentionForStatus 按状态选 @ 目标：待处理→负责人；协助中→协助人；待验证→创建人。
// 协助人/创建人空时回退负责人。
func issueTrackerMentionForStatus(rec issueTrackerRecord) (openID, name, roleLabel string) {
	switch strings.TrimSpace(rec.Status) {
	case issueTrackerStatusHelperPending:
		if strings.TrimSpace(rec.HelperOpenID) != "" {
			return rec.HelperOpenID, firstNonEmpty(rec.HelperName, rec.HelperOpenID), "协助人"
		}
		return rec.AssigneeOpenID, firstNonEmpty(rec.AssigneeName, rec.AssigneeOpenID), "负责人"
	case issueTrackerStatusPendingVerify:
		if strings.TrimSpace(rec.CreatorOpenID) != "" {
			return rec.CreatorOpenID, firstNonEmpty(rec.CreatorName, rec.CreatorOpenID), "创建人"
		}
		return rec.AssigneeOpenID, firstNonEmpty(rec.AssigneeName, rec.AssigneeOpenID), "负责人"
	default:
		return rec.AssigneeOpenID, firstNonEmpty(rec.AssigneeName, rec.AssigneeOpenID), "负责人"
	}
}

func issueTrackerTaskPreview(rec issueTrackerRecord) string {
	title := strings.TrimSpace(rec.Title)
	number := strings.TrimSpace(rec.Number)
	switch {
	case number != "" && title != "":
		return number + " " + title
	case title != "":
		return title
	case number != "":
		return number
	default:
		return "未命名问题"
	}
}

func buildIssueTrackerReminderCard(records []issueTrackerRecord, now time.Time, cfg issueTrackerConfig) feishuCard {
	title := firstNonEmpty(cfg.Title, defaultIssueTrackerTitle)
	elements := []map[string]any{
		cardMarkdown("以下 **非缺陷问题** 仍待推进，请相关同学跟进并回写表格。"),
	}
	limit := 12
	for i, rec := range records {
		if i >= limit {
			elements = append(elements, cardMarkdown(fmt.Sprintf("还有 %d 条开放中，请打开表格查看。", len(records)-limit)))
			break
		}
		openID, mentionName, roleLabel := issueTrackerMentionForStatus(rec)
		mention := escapeLarkMarkdown(firstNonEmpty(mentionName, "未记录"))
		if strings.TrimSpace(openID) != "" {
			mention = fmt.Sprintf("%s <at id=\"%s\"></at>", mention, openID)
		}
		elapsed := "未记录"
		if !rec.CreatedAt.IsZero() {
			elapsed = formatDurationCN(now.Sub(rec.CreatedAt))
		}
		line := fmt.Sprintf("**%d. %s**\n%s：%s\n状态：%s｜已停留：%s",
			i+1,
			escapeLarkMarkdown(issueTrackerTaskPreview(rec)),
			roleLabel,
			mention,
			escapeLarkMarkdown(firstNonEmpty(rec.Status, "未记录")),
			elapsed,
		)
		elements = append(elements, cardMarkdown(line))
	}
	if strings.TrimSpace(cfg.URL) != "" {
		elements = append(elements, map[string]any{
			"tag": "action",
			"actions": []map[string]any{
				{
					"tag": "button",
					"text": map[string]string{
						"tag":     "plain_text",
						"content": "打开非缺陷问题表",
					},
					"type": "default",
					"url":  cfg.URL,
				},
			},
		})
	}
	return feishuCard{
		Config: map[string]any{"wide_screen_mode": true},
		Header: feishuHeader{
			Template: "turquoise",
			Title: feishuCardText{
				Tag:     "plain_text",
				Content: title,
			},
		},
		Elements: elements,
	}
}

func (s *server) startIssueTrackerReminder() func() {
	if s == nil || s.issueTracker == nil || !s.feishuAppConfigured() {
		return func() {}
	}
	cfg := s.issueTracker.cfg
	if !cfg.Enabled || strings.TrimSpace(cfg.ChatID) == "" || cfg.Interval <= 0 {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// 启动后稍等再跑，避免与其它 ticker 同时打飞书。
		timer := time.NewTimer(15 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			runCtx, runCancel := context.WithTimeout(ctx, 20*time.Second)
			if err := s.runIssueTrackerReminderOnce(runCtx, time.Now()); err != nil {
				log.Printf("issue-tracker reminder failed: %v", err)
			}
			runCancel()
		}
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				runCtx, runCancel := context.WithTimeout(ctx, 20*time.Second)
				if err := s.runIssueTrackerReminderOnce(runCtx, now); err != nil {
					log.Printf("issue-tracker reminder failed: %v", err)
				}
				runCancel()
			}
		}
	}()
	return cancel
}

func (s *server) runIssueTrackerReminderOnce(ctx context.Context, now time.Time) error {
	if s == nil || s.issueTracker == nil || !s.feishuAppConfigured() {
		return nil
	}
	cfg := s.issueTracker.cfg
	if !cfg.Enabled || strings.TrimSpace(cfg.ChatID) == "" {
		return nil
	}
	records, err := s.issueTracker.FetchRecords(ctx)
	if err != nil {
		return err
	}
	due := issueTrackerOpenDue(records, cfg.Statuses)
	if len(due) == 0 {
		return nil
	}
	key := "issue-tracker"
	if !s.shouldSendIssueTrackerReminder(key, now) {
		return nil
	}
	card := buildIssueTrackerReminderCard(due, now, cfg)
	if _, err := s.sendFeishuAppCardTo(ctx, cfg.ChatID, "chat_id", feishuCardMessage{MsgType: "interactive", Card: card}); err != nil {
		return err
	}
	s.markIssueTrackerReminded(key, now)
	log.Printf("issue-tracker reminder sent chat=%s count=%d", cfg.ChatID, len(due))
	return nil
}

func (s *server) shouldSendIssueTrackerReminder(key string, now time.Time) bool {
	if s == nil {
		return false
	}
	cooldown := time.Hour
	if s.issueTracker != nil && s.issueTracker.cfg.Cooldown > 0 {
		cooldown = s.issueTracker.cfg.Cooldown
	}
	s.issueTrackerReminderMu.Lock()
	defer s.issueTrackerReminderMu.Unlock()
	if s.issueTrackerReminderAt == nil {
		s.issueTrackerReminderAt = map[string]time.Time{}
	}
	last, ok := s.issueTrackerReminderAt[key]
	return !ok || now.Sub(last) >= cooldown
}

func (s *server) markIssueTrackerReminded(key string, now time.Time) {
	if s == nil || strings.TrimSpace(key) == "" {
		return
	}
	s.issueTrackerReminderMu.Lock()
	defer s.issueTrackerReminderMu.Unlock()
	if s.issueTrackerReminderAt == nil {
		s.issueTrackerReminderAt = map[string]time.Time{}
	}
	s.issueTrackerReminderAt[key] = now
}
