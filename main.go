package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultListenAddr = ":8080"
const defaultGrafanaURL = "https://grafana.example.com/"
const matrixAPIDashboardURL = "https://grafana.example.com/d/matrix-api/matrix-api-overview"
const userSrvDashboardURL = "https://grafana.example.com/d/user-srv/user-srv-overview"
const defaultDedupNotifyInterval = 15 * time.Minute
const defaultFeishuEventDedupTTL = 30 * time.Minute
const defaultDirtyWorkTimeoutReminderChatID = "oc_example_dirty_work"
const defaultUserFeedbackOncallChatID = "oc_example_feedback"
const defaultUserFeedbackOncallReplyPrefix = "收到新的用户反馈，请跟进"
const defaultUserFeedbackOncallMentionTTL = 24 * time.Hour

type grafanaWebhook struct {
	Receiver          string            `json:"receiver"`
	Status            string            `json:"status"`
	Alerts            []grafanaAlert    `json:"alerts"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Title             string            `json:"title"`
	Message           string            `json:"message"`
}

type grafanaAlert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	DashboardURL string            `json:"dashboardURL"`
	PanelURL     string            `json:"panelURL"`
	Values       map[string]any    `json:"values"`
}

type feishuCardMessage struct {
	MsgType string     `json:"msg_type"`
	Card    feishuCard `json:"card"`
}

type feishuCard struct {
	Schema   string           `json:"schema,omitempty"`
	Config   map[string]any   `json:"config,omitempty"`
	Header   feishuHeader     `json:"header"`
	Elements []map[string]any `json:"elements,omitempty"`
	Body     map[string]any   `json:"body,omitempty"`
}

type feishuHeader struct {
	Template string         `json:"template"`
	Title    feishuCardText `json:"title"`
}

type feishuCardText struct {
	Tag     string `json:"tag"`
	Content string `json:"content"`
}

type server struct {
	feishuWebhook   string
	feishuAppID     string
	feishuAppSecret string
	feishuChatID    string
	feishuP1ChatID  string
	// attributionChatID routes every attribution-* service alert to the
	// attribution on-call group, regardless of severity.
	attributionChatID string
	// dataAlertChatID and dataAlertMentionOpenID keep alerts sent to the data
	// group owned by a single designated responder instead of the incident pool.
	dataAlertChatID        string
	dataAlertMentionOpenID string
	// serviceChatRoutes 把 service 标签映射到独立飞书群 chat_id（来自 SERVICE_CHAT_ROUTES）。
	// 命中时优先于 severity 路由——让数据告警等不同团队各进各群。空=不启用，沿用旧逻辑。
	serviceChatRoutes       map[string]string
	feishuAPIBase           string
	token                   string
	feishuVerificationToken string
	feishuEncryptKey        string
	client                  *http.Client
	analyzer                Analyzer
	analysisTimeout         time.Duration
	// backend 是 matrix-backend 的 alert_incident HTTP 客户端；nil 表示
	// 本次部署还没接入 backend，所有 callback 走 legacy 路径（仅 thread reply、
	// 不做台账、不做去重、不做积分）。降级路径必须可用，因为 backend 可能
	// 在维护或未灰度。
	backend *IncidentBackend
	// mailer is optional: when nil, attribution reports stay in the
	// Feishu thread (legacy mode). When set, the full report is sent by
	// email and the thread reply is reduced to a one-liner pointing at
	// the mailbox. Tests inject a fakeMailer to capture rendered HTML.
	mailer Mailer
	// emailLookup resolves operator open_id -> email. In production this
	// is `s` itself (server implements EmailLookup); tests inject a stub
	// to avoid hitting the contact API.
	emailLookup EmailLookup
	// chatMembers lists everyone in an alert chat so we can broadcast
	// the attribution email to the whole on-call room, not just the one
	// person who happened to click "AI 归因". Production uses `s`
	// itself; tests inject a stub.
	chatMembers ChatMemberLister
	// emailMap translates open_ids listed by chatMembers into real
	// mailboxes. Loaded at startup from a ConfigMap-mounted file, with
	// optional background refresh. Nil is treated as "every member maps
	// to nothing" — broadcast will yield zero recipients and
	// deliverReportByEmail falls back to fallbackTo.
	emailMap EmailMap
	// fallbackTo is the comma-separated list of emails used when the
	// operator's mailbox cannot be looked up (scope missing, user has
	// no enterprise email, etc.). Empty by default.
	fallbackTo []string
	// emergencyAssignees 是 backend 完全不可用 / picker 返回空 assignee 时的
	// 应急 @ 人列表。来源：env `EMERGENCY_ASSIGNEE_LIST=ou_a,ou_b,ou_c`。
	//
	// 选人策略（pickEmergencyAssignee）：按 fingerprint 做 fnv32 哈希后取模，
	// 这样同一条告警的反复触发会落到同一个人头上，避免：
	//   * 重发告警每次随机换人，所有人都被打扰；
	//   * forwarder 重启后选人结果飘动，运维难追踪。
	// 只在 backend 没给出 assignee 时启用；正常路径不影响。
	emergencyAssignees []string
	// dedupNotifyAt records the last fallback notification per incident/fingerprint.
	// It lets us keep incident dedup while still surfacing long-running or
	// unbound alerts instead of silently swallowing every repeated firing.
	dedupNotifyInterval           time.Duration
	dedupNotifyMu                 sync.Mutex
	dedupNotifyAt                 map[string]time.Time
	feishuEventDedupTTL           time.Duration
	feishuEventDedupMu            sync.Mutex
	feishuEventDedupAt            map[string]time.Time
	alertSilences                 *alertSilenceStore
	dirtyWorkBitable              *dirtyWorkBitableClient
	dirtyWorkRecordURL            string
	dirtyWorkReminder             dirtyWorkTimeoutReminderConfig
	dirtyWorkReminderMu           sync.Mutex
	dirtyWorkReminderAt           map[string]time.Time
	dirtyWorkTopicReminder        dirtyWorkTopicReminderConfig
	dirtyWorkTopicReminderMu      sync.Mutex
	dirtyWorkTopicReminderAt      map[string]time.Time
	issueTracker                  *issueTrackerClient
	issueTrackerReminderMu        sync.Mutex
	issueTrackerReminderAt        map[string]time.Time
	testCaseReminder              *testCaseReminder
	productionAcceptanceReminder  *productionAcceptanceReminder
	dirtyWorkRotationMu           sync.Mutex
	dirtyWorkRotationLastOpenID   string
	userFeedbackOncallChatID      string
	userFeedbackOncallCandidates  []string
	userFeedbackOncallReplyPrefix string
	userFeedbackOncallMentionTTL  time.Duration
	userFeedbackOncallMentionMu   sync.Mutex
	userFeedbackOncallMentionAt   map[string]time.Time
	userFeedbackOncallSnapshotMu  sync.RWMutex
	userFeedbackOncallSnapshot    *userFeedbackOncallRuntimeConfig
	userFeedbackOncallSnapshotAt  time.Time
	refactorOrchestrator          *RefactorOrchestratorClient
	refactorDefaultRepo           string
	refactorAutoMetric            bool
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "voice-probe" {
		os.Exit(runVoiceProbeCLI(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}

	webhook := strings.TrimSpace(os.Getenv("FEISHU_WEBHOOK"))
	appID := strings.TrimSpace(os.Getenv("FEISHU_APP_ID"))
	appSecret := strings.TrimSpace(os.Getenv("FEISHU_APP_SECRET"))
	chatID := strings.TrimSpace(os.Getenv("FEISHU_CHAT_ID"))
	p1ChatID := strings.TrimSpace(os.Getenv("FEISHU_P1_CHAT_ID"))
	if webhook == "" && (appID == "" || appSecret == "" || chatID == "") {
		log.Fatal("FEISHU_WEBHOOK or FEISHU_APP_ID/FEISHU_APP_SECRET/FEISHU_CHAT_ID is required")
	}

	addr := strings.TrimSpace(os.Getenv("LISTEN_ADDR"))
	if addr == "" {
		addr = defaultListenAddr
	}
	apiBase := strings.TrimRight(strings.TrimSpace(os.Getenv("FEISHU_API_BASE")), "/")
	if apiBase == "" {
		apiBase = "https://open.feishu.cn"
	}
	analysisTimeout := durationFromEnv("COPILOT_ANALYSIS_TIMEOUT", 30*time.Second)

	s := &server{
		feishuWebhook:           webhook,
		feishuAppID:             appID,
		feishuAppSecret:         appSecret,
		feishuChatID:            chatID,
		feishuP1ChatID:          p1ChatID,
		attributionChatID:       strings.TrimSpace(os.Getenv("ATTRIBUTION_CHAT_ID")),
		dataAlertChatID:         strings.TrimSpace(os.Getenv("DATA_ALERT_CHAT_ID")),
		dataAlertMentionOpenID:  strings.TrimSpace(os.Getenv("DATA_ALERT_MENTION_OPEN_ID")),
		serviceChatRoutes:       parseServiceChatRoutes(os.Getenv("SERVICE_CHAT_ROUTES")),
		feishuAPIBase:           apiBase,
		token:                   strings.TrimSpace(os.Getenv("GRAFANA_FORWARDER_TOKEN")),
		feishuVerificationToken: strings.TrimSpace(os.Getenv("FEISHU_VERIFICATION_TOKEN")),
		feishuEncryptKey:        strings.TrimSpace(os.Getenv("FEISHU_ENCRYPT_KEY")),
		client:                  &http.Client{Timeout: 10 * time.Second},
		analyzer:                analyzerFromEnv(analysisTimeout),
		analysisTimeout:         analysisTimeout,
		fallbackTo:              splitEmailList(os.Getenv("EMAIL_FALLBACK_TO")),
		emergencyAssignees:      splitOpenIDList(os.Getenv("EMERGENCY_ASSIGNEE_LIST")),
		dedupNotifyInterval:     durationFromEnv("ALERT_DEDUP_NOTIFY_INTERVAL", defaultDedupNotifyInterval),
		feishuEventDedupTTL:     durationFromEnv("FEISHU_EVENT_DEDUP_TTL", defaultFeishuEventDedupTTL),
		feishuEventDedupAt:      map[string]time.Time{},
		alertSilences:           newAlertSilenceStore(),
		dirtyWorkRecordURL:      strings.TrimSpace(os.Getenv("DIRTY_WORK_BITABLE_RECORD_URL")),
		dirtyWorkReminder: dirtyWorkTimeoutReminderConfig{
			ChatID:   envOrDefault("DIRTY_WORK_TIMEOUT_REMINDER_CHAT_ID", defaultDirtyWorkTimeoutReminderChatID),
			After:    durationFromEnv("DIRTY_WORK_TIMEOUT_REMINDER_AFTER", 48*time.Hour),
			Interval: durationFromEnv("DIRTY_WORK_TIMEOUT_REMINDER_INTERVAL", 10*time.Minute),
			Cooldown: durationFromEnv("DIRTY_WORK_TIMEOUT_REMINDER_COOLDOWN", 6*time.Hour),
		},
		dirtyWorkTopicReminder:        dirtyWorkTopicReminderFromEnv(),
		dirtyWorkReminderAt:           map[string]time.Time{},
		dirtyWorkTopicReminderAt:      map[string]time.Time{},
		issueTrackerReminderAt:        map[string]time.Time{},
		userFeedbackOncallChatID:      envOrDefault("USER_FEEDBACK_ONCALL_CHAT_ID", defaultUserFeedbackOncallChatID),
		userFeedbackOncallCandidates:  splitOpenIDList(os.Getenv("USER_FEEDBACK_ONCALL_CANDIDATES")),
		userFeedbackOncallReplyPrefix: envOrDefault("USER_FEEDBACK_ONCALL_REPLY_PREFIX", defaultUserFeedbackOncallReplyPrefix),
		userFeedbackOncallMentionTTL:  durationFromEnv("USER_FEEDBACK_ONCALL_MENTION_TTL", defaultUserFeedbackOncallMentionTTL),
		userFeedbackOncallMentionAt:   map[string]time.Time{},
		refactorDefaultRepo:           envOrDefault("REFACTOR_DEFAULT_REPO", "matrix-api"),
		refactorAutoMetric:            os.Getenv("REFACTOR_AUTO_ENQUEUE_METRIC") == "1",
	}
	if refactorURL := strings.TrimSpace(os.Getenv("REFACTOR_ORCHESTRATOR_URL")); refactorURL != "" {
		s.refactorOrchestrator = &RefactorOrchestratorClient{
			BaseURL: refactorURL,
			Timeout: durationFromEnv("REFACTOR_ENQUEUE_TIMEOUT", 5*time.Second),
		}
		log.Printf("refactor orchestrator enabled url=%s auto_metric=%v", refactorURL, s.refactorAutoMetric)
	}
	s.dirtyWorkBitable = dirtyWorkBitableFromEnv(s)
	s.issueTracker = issueTrackerFromEnv(s)
	s.testCaseReminder = testCaseReminderFromEnv()
	s.productionAcceptanceReminder = productionAcceptanceReminderFromEnv()
	if len(s.emergencyAssignees) > 0 {
		log.Printf("emergency-assignee: enabled list_size=%d", len(s.emergencyAssignees))
	} else {
		log.Printf("emergency-assignee: disabled (set EMERGENCY_ASSIGNEE_LIST=ou_a,ou_b to enable)")
	}
	if s.dirtyWorkBitable != nil {
		log.Printf("dirty-work bitable: enabled app_token=%s candidate_table=%s record_table=%s",
			maskToken(s.dirtyWorkBitable.cfg.AppToken),
			firstNonEmpty(s.dirtyWorkBitable.cfg.CandidateTableID, "-"),
			firstNonEmpty(s.dirtyWorkBitable.cfg.RecordTableID, "-"))
		if s.dirtyWorkReminder.ChatID != "" && s.dirtyWorkReminder.After > 0 && s.dirtyWorkReminder.Interval > 0 {
			log.Printf("dirty-work timeout reminder: enabled chat=%s after=%s interval=%s cooldown=%s",
				s.dirtyWorkReminder.ChatID, s.dirtyWorkReminder.After, s.dirtyWorkReminder.Interval, s.dirtyWorkReminder.Cooldown)
		}
		if s.dirtyWorkTopicReminder.Enabled {
			log.Printf("dirty-work topic reminder: enabled chat=%s topic_field=%s topic_value=%q interval=%s cooldown=%s statuses=%v mention_all=%v",
				s.dirtyWorkTopicReminder.ChatID,
				s.dirtyWorkTopicReminder.TopicField,
				s.dirtyWorkTopicReminder.TopicValue,
				s.dirtyWorkTopicReminder.Interval,
				s.dirtyWorkTopicReminder.Cooldown,
				s.dirtyWorkTopicReminder.Statuses,
				s.dirtyWorkTopicReminder.MentionAll)
		}
	} else {
		log.Printf("dirty-work bitable: disabled (set DIRTY_WORK_BITABLE_APP_TOKEN and table ids to enable)")
	}
	if s.issueTracker != nil {
		log.Printf("issue-tracker reminder: enabled chat=%s table=%s interval=%s cooldown=%s statuses=%v",
			s.issueTracker.cfg.ChatID,
			s.issueTracker.cfg.TableID,
			s.issueTracker.cfg.Interval,
			s.issueTracker.cfg.Cooldown,
			s.issueTracker.cfg.Statuses)
	} else {
		log.Printf("issue-tracker reminder: disabled (set ISSUE_TRACKER_APP_TOKEN/TABLE_ID/CHAT_ID to enable)")
	}
	if s.testCaseReminder != nil {
		log.Printf("test-case reminder: enabled chat=%s weekday=Friday at=14:00 repeat=%s owners=%d",
			s.testCaseReminder.cfg.ChatID, s.testCaseReminder.cfg.RepeatInterval, len(s.testCaseReminder.cfg.Owners))
	} else {
		log.Printf("test-case reminder: disabled (set TEST_CASE_REMINDER_CHAT_ID to enable)")
	}
	if s.productionAcceptanceReminder != nil {
		log.Printf("production acceptance reminder: enabled chat=%s weekday=Thursday at=09:00 duration=7d owners=%d",
			s.productionAcceptanceReminder.cfg.ChatID, len(s.productionAcceptanceReminder.cfg.Owners))
	} else {
		log.Printf("production acceptance reminder: disabled (set PRODUCTION_ACCEPTANCE_REMINDER_CHAT_ID to enable)")
	}
	if s.userFeedbackOncallChatID != "" && len(s.userFeedbackOncallCandidates) > 0 {
		log.Printf("user-feedback oncall: env fallback enabled chat=%s candidates=%d", s.userFeedbackOncallChatID, len(s.userFeedbackOncallCandidates))
	} else {
		log.Printf("user-feedback oncall: env fallback disabled (set USER_FEEDBACK_ONCALL_CANDIDATES=ou_a,ou_b or configure in admin)")
	}
	// Backend 接入是可选的：缺省时 forwarder 退回旧行为（无台账、无 @ 当班人、
	// 无去重）。这是为了灰度阶段不阻塞告警链路。
	if backendURL := strings.TrimSpace(os.Getenv("ALERT_BACKEND_URL")); backendURL != "" {
		s.backend = &IncidentBackend{
			BaseURL:    backendURL,
			HTTPClient: &http.Client{Timeout: 5 * time.Second},
			Timeout:    durationFromEnv("ALERT_BACKEND_TIMEOUT", 3*time.Second),
		}
		log.Printf("alert backend: enabled url=%s", backendURL)
		s.reloadUserFeedbackOncallFromBackend(context.Background())
		stopFeedbackOncall := s.startUserFeedbackOncallReloader()
		defer stopFeedbackOncall()
	} else {
		log.Printf("alert backend: disabled (ALERT_BACKEND_URL unset); legacy mode")
	}
	// Wire SMTP if configured. We never fatal here: a misconfigured or
	// not-yet-provisioned SMTP must NOT break the alert path; the agent
	// just falls back to in-thread Feishu replies until ops fix it.
	if mailer, err := smtpMailerFromEnv(); err != nil {
		log.Printf("smtp: disabled (%v); attribution will stay in feishu thread", err)
	} else if mailer != nil {
		s.mailer = mailer
		log.Printf("smtp: enabled host=%s port=%d from=%s",
			mailer.cfg.Host, mailer.cfg.Port, mailer.cfg.FromAddress)
	}
	// EmailLookup is the server itself (it talks to the same Feishu
	// app). Wired even when mailer is nil so unit tests can override it
	// independently.
	s.emailLookup = s
	s.chatMembers = s

	// Email map is optional: without it the broadcast path simply
	// yields zero recipients, and deliverReportByEmail drops to the
	// fallback list. We do NOT fatal on a parse error — the alert path
	// must survive a bad ConfigMap.
	if mapPath := strings.TrimSpace(os.Getenv("EMAIL_MAP_FILE")); mapPath != "" {
		refresh := durationFromEnv("EMAIL_MAP_REFRESH", 5*time.Minute)
		if fm, err := NewFileEmailMap(mapPath, refresh); err != nil {
			log.Printf("email-map: disabled (%v)", err)
		} else {
			s.emailMap = fm
			log.Printf("email-map: loaded %d entries from %s (refresh=%s)",
				fm.Size(), mapPath, refresh)
		}
	} else {
		log.Printf("email-map: disabled (EMAIL_MAP_FILE unset); broadcast will fall back")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/grafana/feishu", s.handleGrafanaWebhook)
	mux.HandleFunc("/dataworks/alert", s.handleDataWorksAlert)
	mux.HandleFunc("/feishu/events", s.handleFeishuEvents)
	mux.HandleFunc("/internal/incident_reassigned", s.handleReassignNotify)
	mux.HandleFunc("/internal/dirty_work/launcher", s.handleDirtyWorkLaunch)
	mux.HandleFunc("/internal/test_case_reminder/send", s.handleTestCaseReminderSend)
	mux.HandleFunc("/internal/production_acceptance_reminder/send", s.handleProductionAcceptanceReminderSend)
	mux.HandleFunc("/internal/feedback_weekly/report", s.handleFeedbackWeeklyReport)

	// 升级调度器：仅在 backend 接入时有意义。L2 电话告警通过阿里云语音通知，
	// 缺 env 时 voice=nil，escalator 自动退回到"只 @ 不打电话"。
	if s.backend != nil {
		voice := NewAliyunVoiceClientFromEnv()
		if voice != nil && voice.IsEnabled() {
			log.Printf("aliyun voice: enabled tts_code=%s region=%s", voice.TtsCode, voice.Region)
		} else {
			log.Printf("aliyun voice: disabled (set ALIYUN_VOICE_ACCESS_KEY_ID/_SECRET/_TTS_CODE to enable)")
		}
		voiceSeverities := parseVoiceSeverities(firstNonEmpty(os.Getenv("ALERT_VOICE_SEVERITIES"), "p0,critical"))
		voiceSeveritiesByService := parseServiceVoiceSeverities(os.Getenv("SERVICE_VOICE_SEVERITIES"))
		// 投放 Oncall 等业务群只收卡片，不走 L1/L2 升级 @ / 电话。
		skipEscalation := parseEscalationSkipServices(firstNonEmpty(
			os.Getenv("ESCALATION_SKIP_SERVICES"), "ad-delivery"))
		if voice != nil && voice.IsEnabled() {
			log.Printf("aliyun voice: severity allowlist=%v per_service_overrides=%d (set ALERT_VOICE_SEVERITIES / SERVICE_VOICE_SEVERITIES to override)",
				sortedSeverityKeys(voiceSeverities), len(voiceSeveritiesByService))
		}
		if len(skipEscalation) > 0 {
			log.Printf("alert escalator: skip services=%v (ESCALATION_SKIP_SERVICES)", sortedSeverityKeys(skipEscalation))
		}
		escalator := NewEscalator(s.backend, s, voice, durationFromEnv("ALERT_ESCALATOR_INTERVAL", 30*time.Second), voiceSeverities, voiceSeveritiesByService, skipEscalation)
		if s.dataAlertChatID != "" {
			escalator.dataServiceMatcher = func(service string) bool {
				return s.feishuChatIDForPayload(grafanaWebhook{
					CommonLabels: map[string]string{"service": service},
				}) == s.dataAlertChatID
			}
			escalator.dataEscalationMentionOpenID = strings.TrimSpace(os.Getenv("DATA_ALERT_ESCALATION_MENTION_OPEN_ID"))
		}
		escalator.Start()
		defer escalator.Close()
	}
	stopDirtyWorkReminder := s.startDirtyWorkTimeoutReminder()
	defer stopDirtyWorkReminder()
	stopDirtyWorkTopicReminder := s.startDirtyWorkTopicReminder()
	defer stopDirtyWorkTopicReminder()
	stopIssueTrackerReminder := s.startIssueTrackerReminder()
	defer stopIssueTrackerReminder()
	stopTestCaseReminder := s.startTestCaseReminder()
	defer stopTestCaseReminder()
	stopProductionAcceptanceReminder := s.startProductionAcceptanceReminder()
	defer stopProductionAcceptanceReminder()
	stopFeedbackWeekly := s.startFeedbackWeeklyReportCron()
	defer stopFeedbackWeekly()

	log.Printf("alert forwarder listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

type dirtyWorkCandidate struct {
	Name   string `json:"name"`
	OpenID string `json:"open_id"`
	Weight int    `json:"weight,omitempty"`
}

type dirtyWorkLaunchRequest struct {
	ChatID     string               `json:"chat_id"`
	Title      string               `json:"title"`
	Candidates []dirtyWorkCandidate `json:"candidates"`
}

type dirtyWorkBitableConfig struct {
	AppToken         string
	CandidateTableID string
	CandidateViewID  string
	RecordTableID    string

	CandidateNameField    string
	CandidateOpenIDField  string
	CandidateEnabledField string
	CandidateWeightField  string

	RecordTaskField           string
	RecordOperatorField       string
	RecordOperatorOpenIDField string
	RecordAssigneeField       string
	RecordAssigneeOpenIDField string
	RecordPreviousField       string
	RecordPreviousOpenIDField string
	RecordActionField         string
	RecordStatusField         string
	RecordChatIDField         string
	RecordMessageIDField      string
	RecordCreatedAtField      string
	RecordTopicField          string
}

type dirtyWorkBitableClient struct {
	server *server
	cfg    dirtyWorkBitableConfig
}

type dirtyWorkRecord struct {
	Task           string
	Operator       string
	OperatorOpenID string
	AssigneeName   string
	AssigneeOpenID string
	PreviousName   string
	PreviousOpenID string
	Action         string
	Status         string
	ChatID         string
	MessageID      string
	CreatedAt      time.Time
}

type dirtyWorkTimeoutReminderConfig struct {
	ChatID   string
	After    time.Duration
	Interval time.Duration
	Cooldown time.Duration
}

type dirtyWorkTopicReminderConfig struct {
	Enabled    bool
	ChatID     string
	Interval   time.Duration
	Cooldown   time.Duration
	TopicField string
	TopicValue string
	Statuses   []string
	Title      string
	MentionAll bool
}

type dirtyWorkBitableRecord struct {
	RecordID       string
	Topic          string
	Task           string
	Status         string
	AssigneeName   string
	AssigneeOpenID string
	Operator       string
	OperatorOpenID string
	ChatID         string
	MessageID      string
	CreatedAt      time.Time
}

func dirtyWorkBitableFromEnv(s *server) *dirtyWorkBitableClient {
	topicField := "议题"
	if s != nil && strings.TrimSpace(s.dirtyWorkTopicReminder.TopicField) != "" {
		topicField = strings.TrimSpace(s.dirtyWorkTopicReminder.TopicField)
	}
	cfg := dirtyWorkBitableConfig{
		AppToken:         strings.TrimSpace(os.Getenv("DIRTY_WORK_BITABLE_APP_TOKEN")),
		CandidateTableID: strings.TrimSpace(os.Getenv("DIRTY_WORK_BITABLE_CANDIDATE_TABLE_ID")),
		CandidateViewID:  strings.TrimSpace(os.Getenv("DIRTY_WORK_BITABLE_CANDIDATE_VIEW_ID")),
		RecordTableID:    strings.TrimSpace(os.Getenv("DIRTY_WORK_BITABLE_RECORD_TABLE_ID")),

		CandidateNameField:    envOrDefault("DIRTY_WORK_BITABLE_CANDIDATE_NAME_FIELD", "姓名"),
		CandidateOpenIDField:  envOrDefault("DIRTY_WORK_BITABLE_CANDIDATE_OPEN_ID_FIELD", "open_id"),
		CandidateEnabledField: envOrDefault("DIRTY_WORK_BITABLE_CANDIDATE_ENABLED_FIELD", "启用"),
		CandidateWeightField:  envOrDefault("DIRTY_WORK_BITABLE_CANDIDATE_WEIGHT_FIELD", "权重"),

		RecordTaskField:           envOrDefault("DIRTY_WORK_BITABLE_RECORD_TASK_FIELD", "任务内容"),
		RecordOperatorField:       envOrDefault("DIRTY_WORK_BITABLE_RECORD_OPERATOR_FIELD", "发起人"),
		RecordOperatorOpenIDField: strings.TrimSpace(os.Getenv("DIRTY_WORK_BITABLE_RECORD_OPERATOR_OPEN_ID_FIELD")),
		RecordAssigneeField:       envOrDefault("DIRTY_WORK_BITABLE_RECORD_ASSIGNEE_FIELD", "负责人"),
		RecordAssigneeOpenIDField: strings.TrimSpace(os.Getenv("DIRTY_WORK_BITABLE_RECORD_ASSIGNEE_OPEN_ID_FIELD")),
		RecordPreviousField:       envOrDefault("DIRTY_WORK_BITABLE_RECORD_PREVIOUS_FIELD", "原负责人"),
		RecordPreviousOpenIDField: strings.TrimSpace(os.Getenv("DIRTY_WORK_BITABLE_RECORD_PREVIOUS_OPEN_ID_FIELD")),
		RecordActionField:         strings.TrimSpace(os.Getenv("DIRTY_WORK_BITABLE_RECORD_ACTION_FIELD")),
		RecordStatusField:         envOrDefault("DIRTY_WORK_BITABLE_RECORD_STATUS_FIELD", "状态"),
		RecordChatIDField:         strings.TrimSpace(os.Getenv("DIRTY_WORK_BITABLE_RECORD_CHAT_ID_FIELD")),
		RecordMessageIDField:      strings.TrimSpace(os.Getenv("DIRTY_WORK_BITABLE_RECORD_MESSAGE_ID_FIELD")),
		RecordCreatedAtField:      envOrDefault("DIRTY_WORK_BITABLE_RECORD_CREATED_AT_FIELD", "创建时间"),
		RecordTopicField:          envOrDefault("DIRTY_WORK_BITABLE_RECORD_TOPIC_FIELD", topicField),
	}
	if cfg.AppToken == "" || (cfg.CandidateTableID == "" && cfg.RecordTableID == "") {
		return nil
	}
	return &dirtyWorkBitableClient{server: s, cfg: cfg}
}

func (c *dirtyWorkBitableClient) FetchCandidates(ctx context.Context) ([]dirtyWorkCandidate, error) {
	if c == nil || c.cfg.AppToken == "" || c.cfg.CandidateTableID == "" {
		return nil, nil
	}
	token, err := c.server.tenantAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("page_size", "500")
	q.Set("user_id_type", "open_id")
	if c.cfg.CandidateViewID != "" {
		q.Set("view_id", c.cfg.CandidateViewID)
	}
	if fields, err := json.Marshal([]string{
		c.cfg.CandidateNameField,
		c.cfg.CandidateOpenIDField,
		c.cfg.CandidateEnabledField,
		c.cfg.CandidateWeightField,
	}); err == nil {
		q.Set("field_names", string(fields))
	}
	path := fmt.Sprintf("/open-apis/bitable/v1/apps/%s/tables/%s/records?%s",
		url.PathEscape(c.cfg.AppToken), url.PathEscape(c.cfg.CandidateTableID), q.Encode())
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Items []struct {
				RecordID string         `json:"record_id"`
				Fields   map[string]any `json:"fields"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := c.server.callFeishuAPI(ctx, http.MethodGet, path, token, nil, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("list dirty work candidates failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	candidates := make([]dirtyWorkCandidate, 0, len(resp.Data.Items))
	for _, item := range resp.Data.Items {
		fields := item.Fields
		if fields == nil {
			continue
		}
		if !dirtyWorkBitableBool(fields[c.cfg.CandidateEnabledField], true) {
			continue
		}
		name := firstNonEmpty(
			stringFromAny(fields[c.cfg.CandidateNameField]),
			stringFromAny(fields["Name"]),
			stringFromAny(fields["name"]),
			stringFromAny(fields["姓名"]),
		)
		openID := firstNonEmpty(
			stringFromAny(fields[c.cfg.CandidateOpenIDField]),
			stringFromAny(fields["OpenID"]),
			stringFromAny(fields["openID"]),
			stringFromAny(fields["open_id"]),
		)
		if strings.TrimSpace(openID) == "" {
			log.Printf("dirty-work bitable candidate skipped record=%s: open_id empty", item.RecordID)
			continue
		}
		candidates = append(candidates, dirtyWorkCandidate{
			Name:   name,
			OpenID: openID,
			Weight: dirtyWorkBitableInt(fields[c.cfg.CandidateWeightField], 0),
		})
	}
	return normalizeDirtyWorkCandidates(candidates), nil
}

func (c *dirtyWorkBitableClient) CreateRecord(ctx context.Context, rec dirtyWorkRecord) error {
	if c == nil || c.cfg.AppToken == "" || c.cfg.RecordTableID == "" {
		return nil
	}
	token, err := c.server.tenantAccessToken(ctx)
	if err != nil {
		return err
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	fields := map[string]any{}
	setField(fields, c.cfg.RecordTaskField, rec.Task)
	setBitableUserField(fields, c.cfg.RecordOperatorField, rec.OperatorOpenID)
	setField(fields, c.cfg.RecordOperatorOpenIDField, rec.OperatorOpenID)
	setBitableUserField(fields, c.cfg.RecordAssigneeField, rec.AssigneeOpenID)
	setField(fields, c.cfg.RecordAssigneeOpenIDField, rec.AssigneeOpenID)
	setBitableUserField(fields, c.cfg.RecordPreviousField, rec.PreviousOpenID)
	setField(fields, c.cfg.RecordPreviousOpenIDField, rec.PreviousOpenID)
	setField(fields, c.cfg.RecordActionField, firstNonEmpty(rec.Action, "分配"))
	setField(fields, c.cfg.RecordStatusField, firstNonEmpty(rec.Status, "已分配"))
	setField(fields, c.cfg.RecordChatIDField, rec.ChatID)
	setField(fields, c.cfg.RecordMessageIDField, rec.MessageID)
	if strings.TrimSpace(c.cfg.RecordCreatedAtField) != "" {
		fields[c.cfg.RecordCreatedAtField] = rec.CreatedAt.UnixMilli()
	}
	if len(fields) == 0 {
		return nil
	}
	body := map[string]any{"fields": fields}
	path := fmt.Sprintf("/open-apis/bitable/v1/apps/%s/tables/%s/records?user_id_type=open_id",
		url.PathEscape(c.cfg.AppToken), url.PathEscape(c.cfg.RecordTableID))
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := c.server.callFeishuAPI(ctx, http.MethodPost, path, token, body, &resp); err != nil {
		return err
	}
	if resp.Code != 0 {
		return fmt.Errorf("create dirty work record failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (c *dirtyWorkBitableClient) UpdateLatestRecordStatus(ctx context.Context, rec dirtyWorkRecord) error {
	if c == nil || c.cfg.AppToken == "" || c.cfg.RecordTableID == "" || strings.TrimSpace(c.cfg.RecordStatusField) == "" {
		return nil
	}
	records, err := c.FetchRecords(ctx)
	if err != nil {
		return err
	}
	target, ok := findDirtyWorkRecordForStatusUpdate(records, rec)
	if !ok || strings.TrimSpace(target.RecordID) == "" {
		return fmt.Errorf("dirty work record not found for status update task=%q assignee=%s message=%s", truncateMsg(rec.Task, 80), rec.AssigneeOpenID, rec.MessageID)
	}
	token, err := c.server.tenantAccessToken(ctx)
	if err != nil {
		return err
	}
	fields := map[string]any{}
	setField(fields, c.cfg.RecordStatusField, rec.Status)
	setField(fields, c.cfg.RecordActionField, firstNonEmpty(rec.Action, "状态更新"))
	if len(fields) == 0 {
		return nil
	}
	body := map[string]any{"fields": fields}
	path := fmt.Sprintf("/open-apis/bitable/v1/apps/%s/tables/%s/records/%s?user_id_type=open_id",
		url.PathEscape(c.cfg.AppToken), url.PathEscape(c.cfg.RecordTableID), url.PathEscape(target.RecordID))
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := c.server.callFeishuAPI(ctx, http.MethodPut, path, token, body, &resp); err != nil {
		return err
	}
	if resp.Code != 0 {
		return fmt.Errorf("update dirty work record failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (c *dirtyWorkBitableClient) UpdateRecordStatusByID(ctx context.Context, recordID string, rec dirtyWorkRecord) error {
	recordID = strings.TrimSpace(recordID)
	if c == nil || c.cfg.AppToken == "" || c.cfg.RecordTableID == "" || recordID == "" || strings.TrimSpace(c.cfg.RecordStatusField) == "" {
		return nil
	}
	token, err := c.server.tenantAccessToken(ctx)
	if err != nil {
		return err
	}
	fields := map[string]any{}
	setField(fields, c.cfg.RecordStatusField, rec.Status)
	setField(fields, c.cfg.RecordActionField, firstNonEmpty(rec.Action, "状态更新"))
	setBitableUserField(fields, c.cfg.RecordOperatorField, rec.OperatorOpenID)
	setField(fields, c.cfg.RecordOperatorOpenIDField, rec.OperatorOpenID)
	if len(fields) == 0 {
		return nil
	}
	body := map[string]any{"fields": fields}
	path := fmt.Sprintf("/open-apis/bitable/v1/apps/%s/tables/%s/records/%s?user_id_type=open_id",
		url.PathEscape(c.cfg.AppToken), url.PathEscape(c.cfg.RecordTableID), url.PathEscape(recordID))
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := c.server.callFeishuAPI(ctx, http.MethodPut, path, token, body, &resp); err != nil {
		return err
	}
	if resp.Code != 0 {
		return fmt.Errorf("update dirty work record by id failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (c *dirtyWorkBitableClient) FetchRecords(ctx context.Context) ([]dirtyWorkBitableRecord, error) {
	if c == nil || c.cfg.AppToken == "" || c.cfg.RecordTableID == "" {
		return nil, nil
	}
	token, err := c.server.tenantAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	fieldNames := []string{
		c.cfg.RecordTaskField,
		c.cfg.RecordStatusField,
		c.cfg.RecordAssigneeField,
		c.cfg.RecordAssigneeOpenIDField,
		c.cfg.RecordOperatorField,
		c.cfg.RecordOperatorOpenIDField,
		c.cfg.RecordChatIDField,
		c.cfg.RecordMessageIDField,
		c.cfg.RecordCreatedAtField,
	}
	if c.server != nil && c.server.dirtyWorkTopicReminder.Enabled {
		fieldNames = append(fieldNames, c.cfg.RecordTopicField)
	}
	records := []dirtyWorkBitableRecord{}
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
			url.PathEscape(c.cfg.AppToken), url.PathEscape(c.cfg.RecordTableID), q.Encode())
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
			return nil, fmt.Errorf("list dirty work records failed: code=%d msg=%s", resp.Code, resp.Msg)
		}
		for _, item := range resp.Data.Items {
			rec := dirtyWorkBitableRecord{RecordID: item.RecordID}
			fields := item.Fields
			rec.Topic = stringFromAny(fields[c.cfg.RecordTopicField])
			rec.Task = stringFromAny(fields[c.cfg.RecordTaskField])
			rec.Status = stringFromAny(fields[c.cfg.RecordStatusField])
			rec.AssigneeOpenID = firstNonEmpty(
				stringFromAny(fields[c.cfg.RecordAssigneeOpenIDField]),
				bitableUserOpenID(fields[c.cfg.RecordAssigneeField]),
			)
			rec.AssigneeName = firstNonEmpty(
				bitableUserName(fields[c.cfg.RecordAssigneeField]),
				stringFromAny(fields[c.cfg.RecordAssigneeField]),
				rec.AssigneeOpenID,
			)
			rec.OperatorOpenID = firstNonEmpty(
				stringFromAny(fields[c.cfg.RecordOperatorOpenIDField]),
				bitableUserOpenID(fields[c.cfg.RecordOperatorField]),
			)
			rec.Operator = firstNonEmpty(
				bitableUserName(fields[c.cfg.RecordOperatorField]),
				stringFromAny(fields[c.cfg.RecordOperatorField]),
				rec.OperatorOpenID,
			)
			rec.ChatID = stringFromAny(fields[c.cfg.RecordChatIDField])
			rec.MessageID = stringFromAny(fields[c.cfg.RecordMessageIDField])
			rec.CreatedAt = bitableTime(fields[c.cfg.RecordCreatedAtField])
			records = append(records, rec)
		}
		if !resp.Data.HasMore || strings.TrimSpace(resp.Data.PageToken) == "" {
			break
		}
		pageToken = resp.Data.PageToken
	}
	return records, nil
}

func findDirtyWorkRecordForStatusUpdate(records []dirtyWorkBitableRecord, rec dirtyWorkRecord) (dirtyWorkBitableRecord, bool) {
	rec.MessageID = strings.TrimSpace(rec.MessageID)
	rec.ChatID = strings.TrimSpace(rec.ChatID)
	rec.Task = strings.TrimSpace(rec.Task)
	rec.AssigneeOpenID = strings.TrimSpace(rec.AssigneeOpenID)
	matchers := []func(dirtyWorkBitableRecord) bool{
		func(row dirtyWorkBitableRecord) bool {
			return rec.MessageID != "" && strings.TrimSpace(row.MessageID) == rec.MessageID
		},
		func(row dirtyWorkBitableRecord) bool {
			return rec.ChatID != "" &&
				rec.Task != "" &&
				rec.AssigneeOpenID != "" &&
				strings.TrimSpace(row.ChatID) == rec.ChatID &&
				strings.TrimSpace(row.Task) == rec.Task &&
				strings.TrimSpace(row.AssigneeOpenID) == rec.AssigneeOpenID
		},
		func(row dirtyWorkBitableRecord) bool {
			return rec.Task != "" &&
				rec.AssigneeOpenID != "" &&
				strings.TrimSpace(row.Task) == rec.Task &&
				strings.TrimSpace(row.AssigneeOpenID) == rec.AssigneeOpenID
		},
	}
	for _, matcher := range matchers {
		if row, ok := latestMatchingDirtyWorkRecord(records, matcher); ok {
			return row, true
		}
	}
	return dirtyWorkBitableRecord{}, false
}

func latestMatchingDirtyWorkRecord(records []dirtyWorkBitableRecord, matcher func(dirtyWorkBitableRecord) bool) (dirtyWorkBitableRecord, bool) {
	var latest dirtyWorkBitableRecord
	ok := false
	for _, row := range records {
		if !matcher(row) {
			continue
		}
		if !ok || row.CreatedAt.After(latest.CreatedAt) {
			latest = row
			ok = true
		}
	}
	return latest, ok
}

func setField(fields map[string]any, name string, value string) {
	name = strings.TrimSpace(name)
	value = strings.TrimSpace(value)
	if name == "" || value == "" {
		return
	}
	fields[name] = value
}

func setBitableUserField(fields map[string]any, name string, openID string) {
	name = strings.TrimSpace(name)
	openID = strings.TrimSpace(openID)
	if name == "" || openID == "" {
		return
	}
	fields[name] = []map[string]string{{"id": openID}}
}

func nonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func bitableUserOpenID(value any) string {
	item := firstBitableUserItem(value)
	return firstNonEmpty(
		stringFromAny(item["id"]),
		stringFromAny(item["open_id"]),
		stringFromAny(item["openID"]),
		stringFromAny(item["user_id"]),
	)
}

func bitableUserName(value any) string {
	item := firstBitableUserItem(value)
	return firstNonEmpty(
		stringFromAny(item["name"]),
		stringFromAny(item["en_name"]),
		stringFromAny(item["text"]),
		stringFromAny(item["value"]),
	)
}

func firstBitableUserItem(value any) map[string]any {
	switch v := value.(type) {
	case map[string]any:
		return v
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				return m
			}
		}
	case []map[string]any:
		if len(v) > 0 {
			return v[0]
		}
	}
	return map[string]any{}
}

func bitableTime(value any) time.Time {
	switch v := value.(type) {
	case time.Time:
		return v
	case int:
		return bitableUnixTime(int64(v))
	case int64:
		return bitableUnixTime(v)
	case float64:
		return bitableUnixTime(int64(v))
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return bitableUnixTime(i)
		}
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return time.Time{}
		}
		if i, err := strconv.ParseInt(text, 10, 64); err == nil {
			return bitableUnixTime(i)
		}
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
			if t, err := time.Parse(layout, text); err == nil {
				return t
			}
		}
	case map[string]any:
		return bitableTime(firstNonEmpty(stringFromAny(v["value"]), stringFromAny(v["text"])))
	case []any:
		if len(v) > 0 {
			return bitableTime(v[0])
		}
	}
	return time.Time{}
}

func bitableUnixTime(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	if value > 1_000_000_000_000 {
		return time.UnixMilli(value)
	}
	return time.Unix(value, 0)
}

func dirtyWorkBitableBool(value any, defaultValue bool) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "", "true", "1", "yes", "y", "on", "启用", "是":
			return true
		case "false", "0", "no", "n", "off", "停用", "否":
			return false
		default:
			return defaultValue
		}
	case float64:
		return v != 0
	case int:
		return v != 0
	case nil:
		return defaultValue
	default:
		text := strings.TrimSpace(stringFromAny(v))
		if text == "" {
			return defaultValue
		}
		return dirtyWorkBitableBool(text, defaultValue)
	}
}

func dirtyWorkBitableInt(value any, defaultValue int) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return int(i)
		}
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return i
		}
	default:
		if i, err := strconv.Atoi(strings.TrimSpace(stringFromAny(v))); err == nil {
			return i
		}
	}
	return defaultValue
}

func dirtyWorkTopicReminderFromEnv() dirtyWorkTopicReminderConfig {
	cfg := dirtyWorkTopicReminderConfig{
		Enabled:    false,
		Interval:   time.Hour,
		Cooldown:   time.Hour,
		TopicField: "议题",
		Statuses:   []string{"已分配", "处理中"},
		Title:      "后端待处理问题定时提醒",
	}

	if raw := strings.TrimSpace(os.Getenv("DIRTY_WORK_TOPIC_REMINDER")); raw != "" {
		parts := splitPipeSpec(raw)
		if len(parts) >= 1 {
			cfg.TopicValue = parts[0]
		}
		if len(parts) >= 2 {
			cfg.ChatID = parts[1]
		}
		if len(parts) >= 3 {
			cfg.Interval = parseDurationValue("DIRTY_WORK_TOPIC_REMINDER.interval", parts[2], cfg.Interval)
		}
		if len(parts) >= 4 {
			cfg.Cooldown = parseDurationValue("DIRTY_WORK_TOPIC_REMINDER.cooldown", parts[3], cfg.Cooldown)
		}
		if len(parts) >= 5 {
			cfg.MentionAll = parseBoolValue("DIRTY_WORK_TOPIC_REMINDER.mention_all", parts[4], cfg.MentionAll)
		}
		cfg.Enabled = cfg.TopicValue != "" && cfg.ChatID != ""
	}

	// Advanced overrides. Most cases should only need:
	// DIRTY_WORK_TOPIC_REMINDER="议题名|oc_xxx|1h"
	if value := strings.TrimSpace(os.Getenv("DIRTY_WORK_TOPIC_REMINDER_ENABLED")); value != "" {
		cfg.Enabled = parseBoolValue("DIRTY_WORK_TOPIC_REMINDER_ENABLED", value, cfg.Enabled)
	}
	if value := strings.TrimSpace(os.Getenv("DIRTY_WORK_TOPIC_REMINDER_CHAT_ID")); value != "" {
		cfg.ChatID = value
	}
	if value := strings.TrimSpace(os.Getenv("DIRTY_WORK_TOPIC_REMINDER_FIELD")); value != "" {
		cfg.TopicField = value
	}
	if value := strings.TrimSpace(os.Getenv("DIRTY_WORK_TOPIC_REMINDER_VALUE")); value != "" {
		cfg.TopicValue = value
	}
	if value := strings.TrimSpace(os.Getenv("DIRTY_WORK_TOPIC_REMINDER_INTERVAL")); value != "" {
		cfg.Interval = parseDurationValue("DIRTY_WORK_TOPIC_REMINDER_INTERVAL", value, cfg.Interval)
	}
	if value := strings.TrimSpace(os.Getenv("DIRTY_WORK_TOPIC_REMINDER_COOLDOWN")); value != "" {
		cfg.Cooldown = parseDurationValue("DIRTY_WORK_TOPIC_REMINDER_COOLDOWN", value, cfg.Cooldown)
	}
	if value := strings.TrimSpace(os.Getenv("DIRTY_WORK_TOPIC_REMINDER_STATUSES")); value != "" {
		cfg.Statuses = splitTrimmedList(value)
	}
	if value := strings.TrimSpace(os.Getenv("DIRTY_WORK_TOPIC_REMINDER_TITLE")); value != "" {
		cfg.Title = value
	} else if cfg.TopicValue != "" && cfg.Title == "后端待处理问题定时提醒" {
		cfg.Title = cfg.TopicValue + "定时提醒"
	}
	if value := strings.TrimSpace(os.Getenv("DIRTY_WORK_TOPIC_REMINDER_MENTION_ALL")); value != "" {
		cfg.MentionAll = parseBoolValue("DIRTY_WORK_TOPIC_REMINDER_MENTION_ALL", value, cfg.MentionAll)
	}

	return cfg
}

func envOrDefault(key string, defaultValue string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return defaultValue
}

func boolFromEnv(key string, defaultValue bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	return parseBoolValue(key, value, defaultValue)
}

func parseBoolValue(name string, value string, defaultValue bool) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		log.Printf("invalid %s=%q, using %v", name, value, defaultValue)
		return defaultValue
	}
}

func parseDurationValue(name string, value string, fallback time.Duration) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		log.Printf("invalid %s=%q, using %s", name, value, fallback)
		return fallback
	}
	return duration
}

func splitPipeSpec(raw string) []string {
	raw = strings.ReplaceAll(raw, "｜", "|")
	parts := strings.Split(raw, "|")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		out = append(out, strings.TrimSpace(part))
	}
	return out
}

func splitTrimmedList(raw string) []string {
	raw = strings.ReplaceAll(raw, "\n", ",")
	raw = strings.ReplaceAll(raw, "，", ",")
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]struct{}{}
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func maskToken(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 8 {
		return "***"
	}
	return value[:4] + "..." + value[len(value)-4:]
}

func (s *server) handleDirtyWorkLaunch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.token != "" && !s.validForwarderToken(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.feishuAppConfigured() {
		http.Error(w, "feishu app bot is not configured", http.StatusPreconditionFailed)
		return
	}

	var req dirtyWorkLaunchRequest
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req)
	}
	chatID := strings.TrimSpace(req.ChatID)
	if chatID == "" {
		chatID = s.feishuChatID
	}
	candidates := normalizeDirtyWorkCandidates(req.Candidates)
	if len(candidates) == 0 {
		candidates = s.dirtyWorkCandidates(r.Context())
	}
	if len(candidates) == 0 {
		http.Error(w, "dirty work candidates are empty", http.StatusBadRequest)
		return
	}

	msg := feishuCardMessage{
		MsgType: "interactive",
		Card: buildDirtyWorkLauncherCard(firstNonEmpty(req.Title, "后端待处理问题"), candidates, dirtyWorkLauncherOptions{
			RecordURL: s.dirtyWorkRecordURL,
		}),
	}
	messageID, err := s.sendFeishuAppCardTo(r.Context(), chatID, "chat_id", msg)
	if err != nil {
		http.Error(w, "send dirty work launcher failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]string{
		"status":     "ok",
		"message_id": messageID,
		"chat_id":    chatID,
	})
}

func (s *server) handleGrafanaWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.token != "" && !s.validForwarderToken(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}

	var payload grafanaWebhook
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid grafana webhook payload", http.StatusBadRequest)
		return
	}
	s.processAlert(w, r.Context(), payload)
}

// processAlert 是告警处理的公共主链路：silence → backend upsert/resolve →
// 应急兜底 → 飞书卡片 → bind/DM。Grafana 和 DataWorks 两个入口都复用它，
// 区别只在各自如何把请求解析成 grafanaWebhook。
func (s *server) processAlert(w http.ResponseWriter, ctx context.Context, payload grafanaWebhook) {
	active := isActiveAlertStatus(payload.Status)
	silenceMatch := alertSilenceMatchFromPayload(payload)
	if active {
		if silence, ok := s.matchAlertSilence(ctx, silenceMatch); ok {
			s.writeSilencedResponse(w, "alert silence hit", silence)
			return
		}
	}

	// Backend 接入：尝试 upsert，把 incident_id + assignee 注入卡片。失败时
	// 完全降级到 legacy 卡片，不影响告警送达。dedup=true 时仍然合并 incident，
	// 但按节流策略补发"重复触发"提醒，避免新的 5xx 被静默吞掉。
	var ctxInfo cardContext
	if s.backend != nil {
		if !active {
			incident, resolved, err := s.backend.ResolveByFingerprint(ctx, payload)
			switch {
			case err != nil:
				log.Printf("alert backend resolve-by-fingerprint failed (degrading): %v", err)
			case resolved && incident != nil:
				log.Printf("alert backend resolved incident=%d fp=%s by grafana webhook",
					incident.idAsInt64(), incident.Fingerprint)
			case incident != nil:
				log.Printf("alert backend resolve-by-fingerprint no-op incident=%d fp=%s",
					incident.idAsInt64(), incident.Fingerprint)
			default:
				log.Printf("alert backend resolve-by-fingerprint no active incident")
			}
		} else {
			incident, dedup, fireMentions, err := s.backend.UpsertOnFire(ctx, payload)
			switch {
			case err != nil:
				log.Printf("alert backend upsert failed (degrading): %v", err)
			case dedup:
				ctxInfo = cardContextFromIncident(incident, fireMentions)
				ctxInfo.Repeat = true
				notify, reason := s.shouldNotifyDedup(incident)
				if !notify {
					log.Printf("alert backend dedup hit incident=%d fp=%s fire_count=%d fire_mentions=%d; throttled repeat card",
						ctxInfo.IncidentID, incident.Fingerprint, incident.FireCount, len(fireMentions))
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"status":"deduped"}`))
					return
				}
				ctxInfo.DedupReason = reason
				log.Printf("alert backend dedup hit incident=%d fp=%s fire_count=%d reason=%s; send repeat card",
					ctxInfo.IncidentID, incident.Fingerprint, incident.FireCount, reason)
			default:
				ctxInfo = cardContextFromIncident(incident, fireMentions)
				if len(fireMentions) > 0 {
					log.Printf("alert backend incident=%d assignee=%s fire_mention=%d (full pool @)",
						ctxInfo.IncidentID, ctxInfo.AssigneeOpenID, len(fireMentions))
				}
			}
		}
	}
	if !active {
		if silence, ok := s.matchAlertSilence(ctx, silenceMatch); ok {
			s.writeSilencedResponse(w, "alert silence hit resolved", silence)
			return
		}
	}
	// 应急兜底：backend 失败 / picker 没给 assignee 时，从 EMERGENCY_ASSIGNEE_LIST
	// 按 fingerprint 哈希挑一个人垫上去——这样卡片仍然能 @ 到具体的人，避免"全员
	// 沉默 → 谁也不响应"。同 fingerprint 的反复触发命中同一个人，运维链路稳定。
	if active && ctxInfo.AssigneeOpenID == "" && len(s.emergencyAssignees) > 0 {
		fp := payload.fingerprintForEmergency()
		ctxInfo.AssigneeOpenID = pickEmergencyAssignee(s.emergencyAssignees, fp)
		log.Printf("emergency-assignee: backend gave no assignee, fallback open_id=%s fp=%s",
			ctxInfo.AssigneeOpenID, fp)
	}
	s.applyDataAlertMentionOverride(payload, &ctxInfo)

	msg := buildFeishuCard(payload, s.feishuAppConfigured(), ctxInfo)
	feishuMsgID, err := s.sendFeishu(ctx, msg, payload)
	if err != nil {
		log.Printf("send feishu failed: %v", err)
		http.Error(w, "send feishu failed", http.StatusBadGateway)
		return
	}
	if active && ctxInfo.IncidentID > 0 && feishuMsgID != "" && s.backend != nil {
		bindCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := s.backend.BindFeishuMessage(bindCtx, ctxInfo.IncidentID, feishuMsgID); err != nil {
			log.Printf("alert backend bind feishu message failed incident=%d msg=%s: %v",
				ctxInfo.IncidentID, feishuMsgID, err)
		} else {
			log.Printf("alert backend bind feishu message ok incident=%d msg=%s", ctxInfo.IncidentID, feishuMsgID)
		}
		cancel()
	}
	s.enqueueRefactorMetricFromGrafana(payload, ctxInfo)

	// 普通艾特到人：群卡片里的 <at id> 在飞书移动端经常不弹小红点，运维全员潜
	// 水时就漏了。所以新 incident 创建后，再给责任人发一条私聊 DM 卡片——开 App
	// 的人本机会响、桌面端会弹角标。重复触发只发群提醒，不重复 DM 轰炸责任人。
	//
	// 异步发送：DM 不在告警送达关键路径上，失败/超时只 log，不影响主流程。
	if active && !ctxInfo.Repeat && ctxInfo.AssigneeOpenID != "" && s.feishuAppConfigured() {
		dmCard := buildAssigneeDMCard(payload, ctxInfo)
		go func() {
			dmCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := s.sendFeishuAppCardTo(dmCtx, ctxInfo.AssigneeOpenID, "open_id",
				feishuCardMessage{MsgType: "interactive", Card: dmCard}); err != nil {
				log.Printf("send assignee DM failed incident=%d open_id=%s: %v",
					ctxInfo.IncidentID, ctxInfo.AssigneeOpenID, err)
				return
			}
			log.Printf("send assignee DM ok incident=%d open_id=%s",
				ctxInfo.IncidentID, ctxInfo.AssigneeOpenID)
		}()
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *server) matchAlertSilence(ctx context.Context, match alertSilenceMatch) (alertSilence, bool) {
	if strings.TrimSpace(match.Fingerprint) == "" {
		return alertSilence{}, false
	}
	if s.backend != nil {
		if silence, ok, err := s.backend.MatchSilence(ctx, match.Fingerprint); err != nil {
			log.Printf("alert backend match silence failed fp=%s (degrading to local store): %v", match.Fingerprint, err)
		} else if ok {
			if s.alertSilences != nil {
				if _, err := s.alertSilences.Put(silence); err != nil {
					log.Printf("alert local silence cache put failed fp=%s: %v", silence.Fingerprint, err)
				}
			}
			return silence, true
		}
	}
	if s.alertSilences != nil {
		return s.alertSilences.Match(match.Fingerprint)
	}
	return alertSilence{}, false
}

func (s *server) writeSilencedResponse(w http.ResponseWriter, prefix string, silence alertSilence) {
	log.Printf("%s fp=%s alert=%s service=%s env=%s severity=%s operator=%s expires_at=%s",
		prefix, silence.Fingerprint, silence.AlertName, silence.Service, silence.Env, silence.Severity,
		silence.OperatorOpenID, silence.ExpiresAt.Format(time.RFC3339))
	writeJSON(w, map[string]string{
		"status":      "silenced",
		"fingerprint": silence.Fingerprint,
		"expires_at":  silence.ExpiresAt.Format(time.RFC3339),
	})
}

func isActiveAlertStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "firing", "alerting":
		return true
	case "resolved", "ok":
		return false
	default:
		// Unknown Grafana-like states are safer to surface as active alerts than
		// to silently drop on the floor.
		return true
	}
}

// cardContext 把 backend 决定的"上下文信息"注入卡片：incident_id 必须埋到
// 每个 callback button 的 value，让点击事件能反查到 incident。
//
// FireMentionOpenIDs 是 fire-time 全员 @ 列表（不含 AssigneeOpenID，backend 已 dedup）。
// buildFeishuCard 会在卡片末尾追加一段 "cc: <at>×N" 行，给整队人一次性可见性，
// 但 button 仍然只发给 assignee（避免 N 个人都点 ack 之后状态机抖动）。
type cardContext struct {
	IncidentID         int64
	AssigneeOpenID     string
	FireMentionOpenIDs []string
	Repeat             bool
	FireCount          int32
	LastFiredAt        string
	DedupReason        string
}

func cardContextFromIncident(incident *incidentInfoDTO, fireMentions []string) cardContext {
	if incident == nil {
		return cardContext{FireMentionOpenIDs: fireMentions}
	}
	return cardContext{
		IncidentID:         incident.idAsInt64(),
		AssigneeOpenID:     incident.AssigneeOpenID,
		FireMentionOpenIDs: fireMentions,
		FireCount:          incident.FireCount,
		LastFiredAt:        incident.LastFiredAt,
	}
}

func (s *server) shouldNotifyDedup(incident *incidentInfoDTO) (bool, string) {
	if incident == nil {
		return false, "empty_incident"
	}
	key := strings.TrimSpace(incident.ID.String())
	if key == "" || key == "0" {
		key = strings.TrimSpace(incident.Fingerprint)
	}
	if key == "" {
		return true, "missing_dedup_key"
	}
	interval := s.dedupNotifyInterval
	if interval <= 0 {
		interval = defaultDedupNotifyInterval
	}
	now := time.Now()
	s.dedupNotifyMu.Lock()
	defer s.dedupNotifyMu.Unlock()
	if s.dedupNotifyAt == nil {
		s.dedupNotifyAt = make(map[string]time.Time)
	}
	last, ok := s.dedupNotifyAt[key]
	if ok && now.Sub(last) < interval {
		return false, "throttled"
	}
	s.dedupNotifyAt[key] = now
	if strings.TrimSpace(incident.FeishuMsgID) == "" {
		return true, "missing_feishu_msg_id"
	}
	return true, "repeat_interval"
}

type feishuEventEnvelope struct {
	Challenge string            `json:"challenge"`
	Token     string            `json:"token"`
	Type      string            `json:"type"`
	Encrypt   string            `json:"encrypt"`
	Header    feishuEventHeader `json:"header"`
	Event     json.RawMessage   `json:"event"`
}

type feishuEventHeader struct {
	EventID   string `json:"event_id"`
	Token     string `json:"token"`
	EventType string `json:"event_type"`
	Type      string `json:"type"`
}

func (s *server) handleFeishuEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}

	if s.feishuEncryptKey != "" && r.Header.Get("X-Lark-Signature") != "" {
		if !verifyFeishuSignature(r, s.feishuEncryptKey, body) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
	}

	payload, err := s.decodeFeishuEventBody(body)
	if err != nil {
		http.Error(w, "invalid event payload", http.StatusBadRequest)
		return
	}

	var envelope feishuEventEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		http.Error(w, "invalid event json", http.StatusBadRequest)
		return
	}

	if s.feishuVerificationToken != "" {
		token := firstNonEmpty(envelope.Token, envelope.Header.Token, tokenFromRawEvent(envelope.Event))
		if token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.feishuVerificationToken)) != 1 {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
	}

	eventType := firstNonEmpty(envelope.Type, envelope.Header.EventType, envelope.Header.Type, typeFromRawEvent(envelope.Event))
	if eventType == "url_verification" {
		challenge := firstNonEmpty(envelope.Challenge, challengeFromRawEvent(envelope.Event))
		if challenge == "" {
			http.Error(w, "missing challenge", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]string{"challenge": challenge})
		return
	}

	if eventType == "card.action.trigger" {
		result, err := s.handleCardAction(r.Context(), envelope.Event)
		if err != nil {
			log.Printf("handle card action failed: %v", err)
			writeJSON(w, feishuCardCallbackResponse{
				Toast: &feishuCardToast{Type: "warning", Content: "认领记录失败，请稍后重试"},
			})
			return
		}
		writeJSON(w, result)
		return
	}

	if eventType == "im.message.receive_v1" {
		if key := s.feishuMessageEventDedupKey(envelope.Header.EventID, envelope.Event); key != "" && s.seenFeishuMessageEvent(key, time.Now()) {
			log.Printf("feishu message event duplicate ignored key=%s", key)
			writeJSON(w, map[string]int{"code": 0})
			return
		}
		event := append(json.RawMessage(nil), envelope.Event...)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := s.handleDirtyWorkMessageEvent(ctx, event); err != nil {
				log.Printf("handle dirty work message event failed: %v", err)
			}
			if err := s.handleTestCaseReminderMessageEvent(ctx, event); err != nil {
				log.Printf("handle test-case reminder message event failed: %v", err)
			}
			if err := s.handleProductionAcceptanceReminderMessageEvent(ctx, event); err != nil {
				log.Printf("handle production acceptance reminder message event failed: %v", err)
			}
			if err := s.handleUserFeedbackOncallMessageEvent(ctx, event, time.Now()); err != nil {
				log.Printf("handle user feedback oncall message event failed: %v", err)
			}
		}()
		writeJSON(w, map[string]int{"code": 0})
		return
	}

	writeJSON(w, map[string]int{"code": 0})
}

func (s *server) feishuMessageEventDedupKey(eventID string, raw json.RawMessage) string {
	if eventID = strings.TrimSpace(eventID); eventID != "" {
		return "event:" + eventID
	}
	var event feishuMessageReceiveEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return ""
	}
	if messageID := strings.TrimSpace(event.Message.MessageID); messageID != "" {
		return "message:" + messageID
	}
	return ""
}

func (s *server) seenFeishuMessageEvent(key string, now time.Time) bool {
	key = strings.TrimSpace(key)
	if s == nil || key == "" {
		return false
	}
	ttl := s.feishuEventDedupTTL
	if ttl <= 0 {
		ttl = defaultFeishuEventDedupTTL
	}
	s.feishuEventDedupMu.Lock()
	defer s.feishuEventDedupMu.Unlock()
	if s.feishuEventDedupAt == nil {
		s.feishuEventDedupAt = map[string]time.Time{}
	}
	for cachedKey, seenAt := range s.feishuEventDedupAt {
		if now.Sub(seenAt) > ttl {
			delete(s.feishuEventDedupAt, cachedKey)
		}
	}
	if seenAt, ok := s.feishuEventDedupAt[key]; ok && now.Sub(seenAt) <= ttl {
		return true
	}
	s.feishuEventDedupAt[key] = now
	return false
}

func (s *server) validForwarderToken(r *http.Request) bool {
	if r.Header.Get("X-Forwarder-Token") == s.token {
		return true
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	const bearerPrefix = "Bearer "
	if strings.HasPrefix(auth, bearerPrefix) && strings.TrimSpace(strings.TrimPrefix(auth, bearerPrefix)) == s.token {
		return true
	}
	// DataWorks 等只能配 URL、不能加请求头的来源，允许用 ?token= 查询参数鉴权。
	// 用常量时间比较，避免 token 被时序侧信道猜出来。
	if qt := strings.TrimSpace(r.URL.Query().Get("token")); qt != "" {
		return subtle.ConstantTimeCompare([]byte(qt), []byte(s.token)) == 1
	}
	return false
}

func buildFeishuCard(payload grafanaWebhook, enableCallback bool, ctxInfo cardContext) feishuCardMessage {
	status := firstNonEmpty(payload.Status, "unknown")
	alertName := firstNonEmpty(payload.CommonLabels["alertname"], payload.Title, "Grafana Alert")
	service := firstNonEmpty(payload.CommonLabels["service"], "-")
	env := firstNonEmpty(payload.CommonLabels["env"], payload.CommonLabels["namespace"], "-")
	severity := firstNonEmpty(payload.CommonLabels["severity"], "-")
	summary := firstNonEmpty(payload.CommonAnnotations["summary"], payload.Title, "-")
	description := firstNonEmpty(payload.CommonAnnotations["description"], payload.Message, "-")
	// 详情里若带 TSV（如脏 campaign CronJob），拆成正文 + 飞书原生表格，避免等宽错位。
	detailText := description
	var detailTable map[string]any
	if prose, table, ok := splitDescriptionTable(description); ok {
		detailText = prose
		detailTable = table
	}

	statusUpper := strings.ToUpper(status)
	headerContent := fmt.Sprintf("[Grafana告警] %s %s", statusUpper, alertName)
	card := feishuCard{
		Config: map[string]any{"wide_screen_mode": true},
		Header: feishuHeader{
			Template: cardTemplate(status),
			Title: feishuCardText{
				Tag:     "plain_text",
				Content: headerContent,
			},
		},
		Elements: []map[string]any{
			{
				"tag": "div",
				"fields": []map[string]any{
					cardField("服务", service),
					cardField("环境", env),
					cardField("级别", severity),
					cardField("状态", statusUpper),
				},
			},
			{"tag": "hr"},
			cardMarkdown(fmt.Sprintf("**摘要**：%s\n**详情**：%s", escapeLarkMarkdown(summary), escapeLarkMarkdown(detailText))),
		},
	}
	if detailTable != nil {
		card.Elements = append(card.Elements, detailTable)
	}

	if ctxInfo.Repeat {
		repeatText := fmt.Sprintf("**重复触发**：已合并到 incident #%d", ctxInfo.IncidentID)
		if ctxInfo.FireCount > 0 {
			repeatText += fmt.Sprintf("，累计触发 %d 次", ctxInfo.FireCount)
		}
		if ctxInfo.LastFiredAt != "" {
			repeatText += fmt.Sprintf("，最近触发 %s", escapeLarkMarkdown(ctxInfo.LastFiredAt))
		}
		if ctxInfo.DedupReason == "missing_feishu_msg_id" {
			repeatText += "\n原飞书消息未绑定，已补发本提醒。"
		}
		card.Elements = append(card.Elements, cardMarkdown(repeatText))
	}

	// @ assignee：互动卡片 lark_md 里 @ 人必须用 <at id="ou_..."></at>。
	// 注意是 id= 不是 user_id=（user_id= 是文本消息的语法，飞书卡片会静默吞掉）。
	// 见 https://open.larksuite.com/document/ugTN1YjL4UTN24CO1UjN/uUzN1YjL1cTN24SN3UjN
	if ctxInfo.AssigneeOpenID != "" {
		card.Elements = append(card.Elements,
			cardMarkdown(fmt.Sprintf("**责任人**：<at id=\"%s\"></at>",
				ctxInfo.AssigneeOpenID)))
	}
	// Fire-time 全员 @：admin 在 oncall 配置里把当前 severity 标记为"首次告警就 @ 全员"。
	// backend 已经过滤掉 OOO / 全局停用 / assignee 自己；这里仅做一行 markdown 渲染。
	// 故意单独成行 + "抄送" 前缀，避免与"责任人"那行视觉混淆——让接单人一眼看出"我是主责，
	// 其它人是 cc"。同时所有 button 仍只走 assignee（见上方注释）。
	if len(ctxInfo.FireMentionOpenIDs) > 0 {
		var b strings.Builder
		b.WriteString("**抄送**：")
		for i, oid := range ctxInfo.FireMentionOpenIDs {
			if i > 0 {
				b.WriteString(" ")
			}
			b.WriteString(fmt.Sprintf("<at id=\"%s\"></at>", oid))
		}
		card.Elements = append(card.Elements, cardMarkdown(b.String()))
	}

	if len(payload.Alerts) > 0 {
		first := payload.Alerts[0]
		if !first.StartsAt.IsZero() {
			card.Elements = append(card.Elements, cardMarkdown(fmt.Sprintf("**触发时间**：%s", formatAlertTime(first.StartsAt))))
		}
		if value := formatValues(first.Values); value != "" {
			card.Elements = append(card.Elements, map[string]any{
				"tag": "div",
				"fields": []map[string]any{
					cardField("当前值", value),
				},
			})
		}
		if fields := alertContextFields(payload); len(fields) > 0 {
			card.Elements = append(card.Elements, map[string]any{
				"tag":    "div",
				"fields": fields,
			})
		}

		var details strings.Builder
		details.WriteString("**告警明细**")
		for i, alert := range payload.Alerts {
			// 按 target 分组的规则一次红 6-8 个下游很常见，上限太小会把关键实例截掉。
			if i >= 8 {
				fmt.Fprintf(&details, "\n- 还有 %d 条告警未展示", len(payload.Alerts)-i)
				break
			}
			name := firstNonEmpty(instanceLabelSummary(alert, payload.CommonLabels), alert.Labels["alertname"], alertName)
			alertStatus := firstNonEmpty(alert.Status, status)
			fmt.Fprintf(&details, "\n- %s %s", strings.ToUpper(alertStatus), escapeLarkMarkdown(name))
			if value := formatValues(alert.Values); value != "" {
				fmt.Fprintf(&details, " | %s", escapeLarkMarkdown(value))
			}
		}
		card.Elements = append(card.Elements, cardMarkdown(details.String()))
	}

	links := cardActions(payload, enableCallback, ctxInfo)
	if len(links) > 0 {
		card.Elements = append(card.Elements, map[string]any{
			"tag":     "action",
			"actions": links,
		})
	}

	if ctxInfo.IncidentID > 0 {
		card.Elements = append(card.Elements, map[string]any{
			"tag": "note",
			"elements": []map[string]any{
				{
					"tag":     "lark_md",
					"content": fmt.Sprintf("incident #%d", ctxInfo.IncidentID),
				},
			},
		})
	}

	return feishuCardMessage{
		MsgType: "interactive",
		Card:    card,
	}
}

func (s *server) sendFeishu(ctx context.Context, msg feishuCardMessage, payload grafanaWebhook) (string, error) {
	if s.feishuAppConfigured() {
		return s.sendFeishuAppCardTo(ctx, s.feishuChatIDForPayload(payload), "chat_id", msg)
	}
	return "", s.sendFeishuWebhook(ctx, msg)
}

func (s *server) sendFeishuWebhook(ctx context.Context, msg feishuCardMessage) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.feishuWebhook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("feishu http status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var feishuResp struct {
		Code       int    `json:"code"`
		Msg        string `json:"msg"`
		StatusCode int    `json:"StatusCode"`
	}
	if err := json.Unmarshal(respBody, &feishuResp); err != nil {
		return fmt.Errorf("decode feishu response: %w", err)
	}
	if feishuResp.Code != 0 || feishuResp.StatusCode != 0 {
		return errors.New(strings.TrimSpace(string(respBody)))
	}
	return nil
}

func (s *server) sendFeishuAppCard(ctx context.Context, msg feishuCardMessage) (string, error) {
	return s.sendFeishuAppCardTo(ctx, s.feishuChatID, "chat_id", msg)
}

func (s *server) feishuChatIDForPayload(payload grafanaWebhook) string {
	// 1) service 路由优先：让数据告警等团队各进各群（SERVICE_CHAT_ROUTES）。
	service := strings.ToLower(strings.TrimSpace(payload.CommonLabels["service"]))
	if service != "" {
		if chatID, ok := s.serviceChatRoutes[service]; ok && chatID != "" {
			return chatID
		}
		if strings.HasPrefix(service, "attribution-") && s.attributionChatID != "" {
			return s.attributionChatID
		}
	}
	// 2) 否则按 severity：P1/P2 进 P1 群，其余进默认群。
	severity := strings.ToUpper(strings.TrimSpace(firstNonEmpty(payload.CommonLabels["severity"], payload.CommonLabels["level"])))
	if s.feishuP1ChatID != "" && (severity == "P1" || severity == "P2") {
		return s.feishuP1ChatID
	}
	return s.feishuChatID
}

func (s *server) applyDataAlertMentionOverride(payload grafanaWebhook, ctxInfo *cardContext) {
	if s == nil || ctxInfo == nil || !isActiveAlertStatus(payload.Status) {
		return
	}
	if s.dataAlertChatID == "" || s.dataAlertMentionOpenID == "" {
		return
	}
	if s.feishuChatIDForPayload(payload) != s.dataAlertChatID {
		return
	}
	ctxInfo.AssigneeOpenID = s.dataAlertMentionOpenID
	ctxInfo.FireMentionOpenIDs = nil
}

// parseServiceChatRoutes 解析 "svcA=oc_xxx;svcB=oc_yyy" 成 map（service 小写归一）。
// 分隔符支持 ; 或换行；空串返回 nil。
func parseServiceChatRoutes(raw string) map[string]string {
	out := map[string]string{}
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ';' || r == '\n' || r == ',' })
	for _, f := range fields {
		kv := strings.SplitN(f, "=", 2)
		if len(kv) != 2 {
			continue
		}
		svc := strings.ToLower(strings.TrimSpace(kv[0]))
		chat := strings.TrimSpace(kv[1])
		if svc != "" && chat != "" {
			out[svc] = chat
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sendFeishuAppCardTo 把卡片投给指定 receive_id。
// receiveIDType 支持 "chat_id"（群） / "open_id"（个人 DM）/ "email" 等。
// 抽出来是为了让"给责任人发 DM"复用同一条 access token 链路。
func (s *server) sendFeishuAppCardTo(ctx context.Context, receiveID, receiveIDType string, msg feishuCardMessage) (string, error) {
	if strings.TrimSpace(receiveID) == "" {
		return "", errors.New("receive_id is empty")
	}
	if strings.TrimSpace(receiveIDType) == "" {
		receiveIDType = "chat_id"
	}
	token, err := s.tenantAccessToken(ctx)
	if err != nil {
		return "", err
	}
	content, err := json.Marshal(msg.Card)
	if err != nil {
		return "", err
	}
	body := map[string]string{
		"receive_id": receiveID,
		"msg_type":   "interactive",
		"content":    string(content),
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	if err := s.callFeishuAPI(ctx, http.MethodPost, "/open-apis/im/v1/messages?receive_id_type="+receiveIDType, token, body, &resp); err != nil {
		return "", err
	}
	if resp.Code != 0 {
		return "", fmt.Errorf("send app card failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return resp.Data.MessageID, nil
}

func (s *server) replyFeishuMessage(ctx context.Context, messageID, text string) error {
	token, err := s.tenantAccessToken(ctx)
	if err != nil {
		return err
	}
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	body := map[string]any{
		"msg_type":        "text",
		"content":         string(content),
		"reply_in_thread": true,
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := s.callFeishuAPI(ctx, http.MethodPost, "/open-apis/im/v1/messages/"+messageID+"/reply", token, body, &resp); err != nil {
		return err
	}
	if resp.Code != 0 {
		return fmt.Errorf("reply message failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

// replyFeishuCard posts an interactive card as a thread reply on `messageID`.
// We use it to render structured copilot reports so the operator gets a
// real card (sections, badges, links) instead of a markdown json blob.
func (s *server) replyFeishuCard(ctx context.Context, messageID string, card feishuCard) error {
	token, err := s.tenantAccessToken(ctx)
	if err != nil {
		return err
	}
	content, err := json.Marshal(card)
	if err != nil {
		return err
	}
	body := map[string]any{
		"msg_type":        "interactive",
		"content":         string(content),
		"reply_in_thread": true,
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := s.callFeishuAPI(ctx, http.MethodPost, "/open-apis/im/v1/messages/"+messageID+"/reply", token, body, &resp); err != nil {
		return err
	}
	if resp.Code != 0 {
		return fmt.Errorf("reply card failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (s *server) tenantAccessToken(ctx context.Context) (string, error) {
	body := map[string]string{
		"app_id":     s.feishuAppID,
		"app_secret": s.feishuAppSecret,
	}
	var resp struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
	}
	if err := s.callFeishuAPI(ctx, http.MethodPost, "/open-apis/auth/v3/tenant_access_token/internal", "", body, &resp); err != nil {
		return "", err
	}
	if resp.Code != 0 || resp.TenantAccessToken == "" {
		return "", fmt.Errorf("get tenant token failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return resp.TenantAccessToken, nil
}

func (s *server) callFeishuAPI(ctx context.Context, method, path, token string, payload any, out any) error {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, s.feishuAPIBase+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("feishu api http status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode feishu api response: %w", err)
	}
	return nil
}

func (s *server) feishuAppConfigured() bool {
	return s.feishuAppID != "" && s.feishuAppSecret != "" && s.feishuChatID != ""
}

func cardTemplate(status string) string {
	switch strings.ToLower(status) {
	case "resolved", "ok":
		return "green"
	case "firing", "alerting":
		return "red"
	default:
		return "orange"
	}
}

func cardField(label, value string) map[string]any {
	return map[string]any{
		"is_short": true,
		"text": map[string]string{
			"tag":     "lark_md",
			"content": fmt.Sprintf("**%s**\n%s", label, escapeLarkMarkdown(value)),
		},
	}
}

func cardMarkdown(content string) map[string]any {
	return map[string]any{
		"tag": "div",
		"text": map[string]string{
			"tag":     "lark_md",
			"content": content,
		},
	}
}

// 按 target / error_type 之类维度分组的规则，一次会带多个实例，但各实例 alertname 完全相同，
// 告警明细里只印 alertname 就是 N 行重复噪音。这里取实例独有的标签（commonLabels 之外的）
// 作为区分标识，让 oncall 直接看出是哪个下游红了。没有独有标签时返回空串，由调用方回退到 alertname。
func instanceLabelSummary(alert grafanaAlert, commonLabels map[string]string) string {
	keys := make([]string, 0, len(alert.Labels))
	for key := range alert.Labels {
		// commonLabels 已经渲染在卡片顶部的服务/环境/级别里，再列一遍是噪音。
		if _, common := commonLabels[key]; common {
			continue
		}
		if key == "alertname" || key == "grafana_folder" {
			continue
		}
		if strings.TrimSpace(alert.Labels[key]) == "" {
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", key, alert.Labels[key]))
	}
	return strings.Join(parts, ", ")
}

func cardV2Markdown(content string) map[string]any {
	return map[string]any{
		"tag":     "markdown",
		"content": content,
	}
}

func cardV2URLButton(text, url string) map[string]any {
	return map[string]any{
		"tag": "button",
		"text": map[string]string{
			"tag":     "plain_text",
			"content": text,
		},
		"type": "default",
		"behaviors": []map[string]any{
			{
				"type":        "open_url",
				"default_url": url,
			},
		},
	}
}

func cardV2CallbackButton(text, buttonType string, value map[string]string) map[string]any {
	if strings.TrimSpace(buttonType) == "" {
		buttonType = "default"
	}
	return map[string]any{
		"tag": "button",
		"text": map[string]string{
			"tag":     "plain_text",
			"content": text,
		},
		"type": buttonType,
		"behaviors": []map[string]any{
			{
				"type":  "callback",
				"value": value,
			},
		},
	}
}

type dirtyWorkLauncherOptions struct {
	RecordURL string
}

func buildDirtyWorkLauncherCard(title string, candidates []dirtyWorkCandidate, opts dirtyWorkLauncherOptions) feishuCard {
	candidates = normalizeDirtyWorkCandidates(candidates)
	encodedCandidates := encodeDirtyWorkCandidates(candidates)
	buttonColumns := make([]map[string]any, 0, len(candidates)+2)
	for _, candidate := range candidates {
		name := strings.TrimSpace(candidate.Name)
		if name == "" {
			name = candidate.OpenID
		}
		buttonColumns = append(buttonColumns, map[string]any{
			"tag":   "column",
			"width": "auto",
			"elements": []map[string]any{
				{
					"tag":  "button",
					"name": "dirty_work_assign_" + candidate.OpenID,
					"text": map[string]string{
						"tag":     "plain_text",
						"content": name,
					},
					"type":             "default",
					"form_action_type": "submit",
					"behaviors": []map[string]any{
						{
							"type": "callback",
							"value": map[string]string{
								"action":           "dirty_work_pick",
								"candidates":       encodedCandidates,
								"assignee_open_id": candidate.OpenID,
								"assignee_name":    name,
							},
						},
					},
				},
			},
		})
	}
	buttonColumns = append(buttonColumns, map[string]any{
		"tag":   "column",
		"width": "auto",
		"elements": []map[string]any{
			{
				"tag":  "button",
				"name": "dirty_work_submit",
				"text": map[string]string{
					"tag":     "plain_text",
					"content": "轮询分配",
				},
				"type":             "primary_filled",
				"form_action_type": "submit",
				"behaviors": []map[string]any{
					{
						"type": "callback",
						"value": map[string]string{
							"action":     "dirty_work_pick",
							"candidates": encodedCandidates,
						},
					},
				},
			},
		},
	})
	if strings.TrimSpace(opts.RecordURL) != "" {
		buttonColumns = append(buttonColumns, map[string]any{
			"tag":   "column",
			"width": "auto",
			"elements": []map[string]any{
				cardV2URLButton("查看分配记录", opts.RecordURL),
			},
		})
	}
	return feishuCard{
		Schema: "2.0",
		Config: map[string]any{"wide_screen_mode": true},
		Header: feishuHeader{
			Template: "blue",
			Title: feishuCardText{
				Tag:     "plain_text",
				Content: title,
			},
		},
		Body: map[string]any{
			"elements": []map[string]any{
				cardV2Markdown("填一下任务内容，点候选人直接指定，或点 **轮询分配** 按顺序分给下一位。"),
				{
					"tag":              "form",
					"name":             "dirty_work_form",
					"direction":        "vertical",
					"vertical_spacing": "8px",
					"elements": []map[string]any{
						cardV2Markdown("**任务内容***"),
						{
							"tag":           "input",
							"name":          "dirty_work_task",
							"required":      true,
							"width":         "fill",
							"default_value": "",
							"placeholder": map[string]string{
								"tag":     "plain_text",
								"content": "例如：整理本周发布遗留事项",
							},
							"fallback": map[string]any{
								"tag": "fallback_text",
								"text": map[string]string{
									"tag":     "plain_text",
									"content": "当前飞书版本不支持输入框，请升级客户端后重试",
								},
							},
						},
						{
							"tag":              "column_set",
							"horizontal_align": "left",
							"columns":          buttonColumns,
						},
					},
				},
			},
		},
	}
}

type dirtyWorkResultOptions struct {
	Candidates       []dirtyWorkCandidate
	PreviousAssignee dirtyWorkCandidate
	RecordURL        string
}

type dirtyWorkAssigneeDMOptions struct {
	Candidates       []dirtyWorkCandidate
	PreviousAssignee dirtyWorkCandidate
	RecordURL        string
	Status           string
	SourceChatID     string
	SourceMessageID  string
}

const dirtyWorkTaskPreviewLimit = 80

func dirtyWorkTaskPreview(task string) string {
	task = strings.TrimSpace(task)
	if task == "" {
		return "未填写任务内容"
	}
	return truncateRunes(strings.Join(strings.Fields(task), " "), dirtyWorkTaskPreviewLimit)
}

func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}

func buildDirtyWorkResultCard(task string, candidate dirtyWorkCandidate, operator string, opts dirtyWorkResultOptions) feishuCard {
	task = firstNonEmpty(task, "未填写任务内容")
	assignee := escapeLarkMarkdown(candidate.Name)
	if candidate.OpenID != "" {
		assignee = fmt.Sprintf("%s <at id=\"%s\"></at>", assignee, candidate.OpenID)
	}
	elements := []map[string]any{
		cardV2Markdown("**任务预览**\n" + escapeLarkMarkdown(dirtyWorkTaskPreview(task))),
		cardV2Markdown("**负责人**：" + assignee),
	}
	if opts.PreviousAssignee.OpenID != "" {
		elements = append(elements, cardV2Markdown(fmt.Sprintf("**转派说明**：%s 没时间，已自动重新分配。", escapeLarkMarkdown(opts.PreviousAssignee.Name))))
	}
	if strings.TrimSpace(operator) != "" {
		elements = append(elements, cardV2Markdown("**发起人**："+operator))
	}
	buttonColumns := make([]map[string]any, 0, 2)
	if len(normalizeDirtyWorkCandidates(opts.Candidates)) > 1 && candidate.OpenID != "" {
		buttonColumns = append(buttonColumns, map[string]any{
			"tag":   "column",
			"width": "auto",
			"elements": []map[string]any{
				cardV2CallbackButton("没时间，重新分配", "default", map[string]string{
					"action":           "dirty_work_repick",
					"task":             task,
					"candidates":       encodeDirtyWorkCandidates(opts.Candidates),
					"assignee_open_id": candidate.OpenID,
					"assignee_name":    candidate.Name,
				}),
			},
		})
	}
	if strings.TrimSpace(opts.RecordURL) != "" {
		buttonColumns = append(buttonColumns, map[string]any{
			"tag":   "column",
			"width": "auto",
			"elements": []map[string]any{
				cardV2URLButton("查看分配记录", opts.RecordURL),
			},
		})
	}
	if len(buttonColumns) > 0 {
		elements = append(elements, map[string]any{
			"tag":              "column_set",
			"horizontal_align": "left",
			"columns":          buttonColumns,
		})
	}
	return feishuCard{
		Schema: "2.0",
		Config: map[string]any{"wide_screen_mode": true},
		Header: feishuHeader{
			Template: "green",
			Title: feishuCardText{
				Tag:     "plain_text",
				Content: "后端待处理问题处理结果",
			},
		},
		Body: map[string]any{
			"elements": elements,
		},
	}
}

func buildDirtyWorkAssigneeDMCard(task string, candidate dirtyWorkCandidate, operator string, opts dirtyWorkAssigneeDMOptions) feishuCard {
	task = firstNonEmpty(task, "未填写任务内容")
	status := firstNonEmpty(opts.Status, "已分配")
	elements := []map[string]any{
		cardV2Markdown("你被分配为 **后端待处理问题** 的负责人。"),
		cardV2Markdown("**任务预览**\n" + escapeLarkMarkdown(dirtyWorkTaskPreview(task))),
		cardV2Markdown("**当前状态**：" + escapeLarkMarkdown(status)),
	}
	if opts.PreviousAssignee.OpenID != "" {
		elements = append(elements, cardV2Markdown(fmt.Sprintf("**转派说明**：%s 没时间，已转派给你。", escapeLarkMarkdown(opts.PreviousAssignee.Name))))
	}
	if strings.TrimSpace(operator) != "" {
		elements = append(elements, cardV2Markdown("**发起人**："+operator))
	}
	buttonColumns := make([]map[string]any, 0, 4)
	if status != "已处理" && candidate.OpenID != "" {
		if status != "处理中" {
			buttonColumns = append(buttonColumns, map[string]any{
				"tag":   "column",
				"width": "auto",
				"elements": []map[string]any{
					cardV2CallbackButton("我来处理", "primary_filled", dirtyWorkAssigneeActionValue("dirty_work_status", task, candidate, operator, opts, "处理中")),
				},
			})
		}
		buttonColumns = append(buttonColumns, map[string]any{
			"tag":   "column",
			"width": "auto",
			"elements": []map[string]any{
				cardV2CallbackButton("已处理", "default", dirtyWorkAssigneeActionValue("dirty_work_status", task, candidate, operator, opts, "已处理")),
			},
		})
		if len(normalizeDirtyWorkCandidates(opts.Candidates)) > 1 {
			buttonColumns = append(buttonColumns, map[string]any{
				"tag":   "column",
				"width": "auto",
				"elements": []map[string]any{
					cardV2CallbackButton("没时间", "default", dirtyWorkAssigneeActionValue("dirty_work_repick", task, candidate, operator, opts, "")),
				},
			})
		}
	}
	if strings.TrimSpace(opts.RecordURL) != "" {
		buttonColumns = append(buttonColumns, map[string]any{
			"tag":   "column",
			"width": "auto",
			"elements": []map[string]any{
				cardV2URLButton("查看分配记录", opts.RecordURL),
			},
		})
	}
	if len(buttonColumns) > 0 {
		elements = append(elements, map[string]any{
			"tag":              "column_set",
			"horizontal_align": "left",
			"columns":          buttonColumns,
		})
	}
	return feishuCard{
		Schema: "2.0",
		Config: map[string]any{"wide_screen_mode": true},
		Header: feishuHeader{
			Template: "blue",
			Title: feishuCardText{
				Tag:     "plain_text",
				Content: "后端待处理问题提醒",
			},
		},
		Body: map[string]any{
			"elements": elements,
		},
	}
}

func buildDirtyWorkTimeoutReminderCard(records []dirtyWorkBitableRecord, now time.Time, after time.Duration, recordURL string) feishuCard {
	elements := []map[string]any{
		cardV2Markdown(fmt.Sprintf("以下 **后端待处理问题** 已超过 %s 未完成，请在群里同步一下进展。", formatDurationCN(after))),
	}
	limit := 8
	for i, rec := range records {
		if i >= limit {
			elements = append(elements, cardV2Markdown(fmt.Sprintf("还有 %d 条也已超时，请打开表格查看。", len(records)-limit)))
			break
		}
		assignee := escapeLarkMarkdown(firstNonEmpty(rec.AssigneeName, rec.AssigneeOpenID, "未记录"))
		if rec.AssigneeOpenID != "" {
			assignee = fmt.Sprintf("%s <at id=\"%s\"></at>", assignee, rec.AssigneeOpenID)
		}
		elapsed := now.Sub(rec.CreatedAt)
		line := fmt.Sprintf("**%d. %s**\n负责人：%s\n状态：%s｜已停留：%s",
			i+1,
			escapeLarkMarkdown(dirtyWorkTaskPreview(rec.Task)),
			assignee,
			escapeLarkMarkdown(firstNonEmpty(rec.Status, "未记录")),
			formatDurationCN(elapsed),
		)
		elements = append(elements, cardV2Markdown(line))
	}
	if strings.TrimSpace(recordURL) != "" {
		elements = append(elements, map[string]any{
			"tag":              "column_set",
			"horizontal_align": "left",
			"columns": []map[string]any{
				{
					"tag":   "column",
					"width": "auto",
					"elements": []map[string]any{
						cardV2URLButton("查看分配记录", recordURL),
					},
				},
			},
		})
	}
	return feishuCard{
		Schema: "2.0",
		Config: map[string]any{"wide_screen_mode": true},
		Header: feishuHeader{
			Template: "orange",
			Title: feishuCardText{
				Tag:     "plain_text",
				Content: "后端待处理问题超时提醒",
			},
		},
		Body: map[string]any{
			"elements": elements,
		},
	}
}

func buildDirtyWorkTopicReminderCard(records []dirtyWorkBitableRecord, now time.Time, cfg dirtyWorkTopicReminderConfig, _ string) feishuCard {
	title := firstNonEmpty(cfg.Title, "后端待处理问题定时提醒")
	topic := strings.TrimSpace(cfg.TopicValue)
	intro := fmt.Sprintf("以下 **%s** 议题仍有未完成事项，请负责人同步进展。", escapeLarkMarkdown(topic))
	if cfg.MentionAll {
		intro = "<at id=\"all\"></at> " + intro
	}
	elements := []map[string]any{cardV2Markdown(intro)}
	limit := 12
	for i, rec := range records {
		if i >= limit {
			elements = append(elements, cardV2Markdown(fmt.Sprintf("还有 %d 条未完成，请打开表格查看。", len(records)-limit)))
			break
		}
		assignee := escapeLarkMarkdown(firstNonEmpty(rec.AssigneeName, rec.AssigneeOpenID, "未记录"))
		if rec.AssigneeOpenID != "" {
			assignee = fmt.Sprintf("%s <at id=\"%s\"></at>", assignee, rec.AssigneeOpenID)
		}
		elapsed := "未记录"
		if !rec.CreatedAt.IsZero() {
			elapsed = formatDurationCN(now.Sub(rec.CreatedAt))
		}
		line := fmt.Sprintf("**%d. %s**\n负责人：%s\n状态：%s｜已停留：%s",
			i+1,
			escapeLarkMarkdown(dirtyWorkTaskPreview(rec.Task)),
			assignee,
			escapeLarkMarkdown(firstNonEmpty(rec.Status, "未记录")),
			elapsed,
		)
		elements = append(elements, cardV2Markdown(line))
		if rec.RecordID != "" && rec.AssigneeOpenID != "" {
			elements = append(elements, map[string]any{
				"tag":              "column_set",
				"horizontal_align": "left",
				"columns": []map[string]any{
					{
						"tag":   "column",
						"width": "auto",
						"elements": []map[string]any{
							cardV2CallbackButton("我已处理", "primary_filled", dirtyWorkTopicDoneActionValue(rec)),
						},
					},
				},
			})
		}
	}
	return feishuCard{
		Schema: "2.0",
		Config: map[string]any{"wide_screen_mode": true},
		Header: feishuHeader{
			Template: "blue",
			Title: feishuCardText{
				Tag:     "plain_text",
				Content: title,
			},
		},
		Body: map[string]any{
			"elements": elements,
		},
	}
}

func dirtyWorkTopicDoneActionValue(rec dirtyWorkBitableRecord) map[string]string {
	return map[string]string{
		"action":           "dirty_work_topic_done",
		"record_id":        rec.RecordID,
		"task":             rec.Task,
		"assignee_open_id": rec.AssigneeOpenID,
		"assignee_name":    rec.AssigneeName,
		"status":           "已处理",
	}
}

func dirtyWorkAssigneeActionValue(action, task string, candidate dirtyWorkCandidate, operator string, opts dirtyWorkAssigneeDMOptions, status string) map[string]string {
	value := map[string]string{
		"action":            action,
		"task":              task,
		"assignee_open_id":  candidate.OpenID,
		"assignee_name":     candidate.Name,
		"requester":         operator,
		"record_url":        opts.RecordURL,
		"source_chat_id":    opts.SourceChatID,
		"source_message_id": opts.SourceMessageID,
	}
	if strings.TrimSpace(status) != "" {
		value["status"] = status
	}
	if encoded := encodeDirtyWorkCandidates(opts.Candidates); encoded != "" {
		value["candidates"] = encoded
	}
	return value
}

func cardDivider() map[string]any {
	return map[string]any{"tag": "hr"}
}

// buildCopilotReportCard renders an AnalysisReport as an interactive card we
// can post as a thread reply. Sections are skipped when empty so a tiny
// rule-based response still looks clean.
func buildCopilotReportCard(req AnalysisRequest, report AnalysisReport, fallbackText string) feishuCard {
	title := firstNonEmpty(report.Title, "告警 Copilot 归因结论")
	if len(title) > 100 {
		title = title[:97] + "..."
	}
	header := feishuHeader{
		Template: copilotCardTemplate(req),
		Title: feishuCardText{
			Tag:     "plain_text",
			Content: "🤖 " + title,
		},
	}

	var elements []map[string]any

	if req.Operator != "" {
		elements = append(elements, cardMarkdown(fmt.Sprintf("%s 触发了 AI 归因。", req.Operator)))
	}

	meta := copilotMetaLine(req)
	if meta != "" {
		elements = append(elements, cardMarkdown(meta))
	}

	elements = appendBulletSection(elements, "事实", report.Facts)
	elements = appendBulletSection(elements, "判断", report.Judgement)
	elements = appendBulletSection(elements, "建议下一步", report.NextSteps)
	elements = appendBulletSection(elements, "参考", report.References)

	// If structured sections are entirely empty, fall back to the raw
	// markdown text so we never silently lose the agent's output.
	if !hasStructuredContent(report) {
		text := strings.TrimSpace(fallbackText)
		if text == "" {
			text = strings.TrimSpace(report.RawText)
		}
		if text != "" {
			elements = append(elements, cardMarkdown(text))
		}
	}

	if !report.GeneratedAt.IsZero() {
		elements = append(elements, cardDivider())
		elements = append(elements, map[string]any{
			"tag": "note",
			"elements": []map[string]any{
				{
					"tag":     "lark_md",
					"content": "生成时间：" + formatAlertTime(report.GeneratedAt),
				},
			},
		})
	}

	return feishuCard{
		Config:   map[string]any{"wide_screen_mode": true},
		Header:   header,
		Elements: elements,
	}
}

// buildCopilotSummaryCard renders a slim card that goes back into the
// Feishu thread when the full report has been emailed out. We keep three
// pieces:
//
//  1. Header — same red/orange/blue as the original alert so the
//     thread looks consistent.
//  2. Body — operator + service/env/severity meta + the very first
//     judgement line as a one-sentence preview ("根因猜测：…").
//  3. Footer — "📧 详情已发邮件至 xxx@yyy.com" + the original
//     "查看大盘" link, all stuffed in a `note` element so the visual
//     weight stays low.
//
// We deliberately do NOT include facts/next_steps in the card; the email
// is the source of truth and we want the chat reply to read like
// "go check your inbox" rather than a duplicate report.
func buildCopilotSummaryCard(req AnalysisRequest, report AnalysisReport, recipient string, emailSent bool) feishuCard {
	title := firstNonEmpty(report.Title, "告警 Copilot 归因结论")
	if len(title) > 80 {
		title = title[:77] + "..."
	}
	header := feishuHeader{
		Template: copilotCardTemplate(req),
		Title: feishuCardText{
			Tag:     "plain_text",
			Content: "🤖 " + title,
		},
	}

	var elements []map[string]any
	if req.Operator != "" {
		elements = append(elements, cardMarkdown(fmt.Sprintf("%s 触发了 AI 归因", req.Operator)))
	}
	if meta := copilotMetaLine(req); meta != "" {
		elements = append(elements, cardMarkdown(meta))
	}

	// One-sentence judgement preview. Pick the first non-empty entry so
	// the operator always gets a "TL;DR" before deciding to open mail.
	if preview := firstNonEmptyString(report.Judgement); preview != "" {
		// Truncate at 140 chars to keep the card scannable on mobile.
		if len([]rune(preview)) > 140 {
			r := []rune(preview)
			preview = string(r[:139]) + "…"
		}
		elements = append(elements, cardMarkdown("**根因猜测**\n"+preview))
	}

	// Email status line.
	var emailLine string
	switch {
	case emailSent && recipient != "":
		rcpts := strings.Split(recipient, ",")
		if len(rcpts) > 1 {
			emailLine = fmt.Sprintf("📧 完整报告已发邮件至群内 **%d** 人，请到邮箱查看事实/建议/参考详情。", len(rcpts))
		} else {
			emailLine = fmt.Sprintf("📧 完整报告已发邮件至 **%s**，请到邮箱查看事实/建议/参考详情。", recipient)
		}
	case !emailSent:
		emailLine = "⚠️ 完整报告渲染成功，但邮件投递失败，请使用「打开大盘」自行排查。"
	}
	if emailLine != "" {
		elements = append(elements, cardMarkdown(emailLine))
	}

	// Action buttons: keep "我来处理" callback + "打开大盘" link so the
	// summary card is self-contained and the operator doesn't have to
	// scroll back to the original alert.
	var actions []map[string]any
	actions = append(actions, cardCallbackButton("我来处理", map[string]string{
		"action":    "claim",
		"alertname": req.AlertName,
		"link":      req.Link,
	}))
	if req.Link != "" {
		actions = append(actions, cardURLButton("打开大盘", req.Link))
	}
	if len(actions) > 0 {
		elements = append(elements, map[string]any{
			"tag":     "action",
			"actions": actions,
		})
	}

	if !report.GeneratedAt.IsZero() {
		elements = append(elements, cardDivider())
		elements = append(elements, map[string]any{
			"tag": "note",
			"elements": []map[string]any{
				{
					"tag":     "lark_md",
					"content": "生成时间：" + formatAlertTime(report.GeneratedAt),
				},
			},
		})
	}

	return feishuCard{
		Config:   map[string]any{"wide_screen_mode": true},
		Header:   header,
		Elements: elements,
	}
}

// firstNonEmptyString returns the first non-blank string in items, or
// "" when all are blank. Small helper kept local because main.go already
// has firstNonEmpty for variadic args; this one operates on a slice.
func firstNonEmptyString(items []string) string {
	for _, s := range items {
		if v := strings.TrimSpace(s); v != "" {
			return v
		}
	}
	return ""
}

func appendBulletSection(elements []map[string]any, title string, items []string) []map[string]any {
	cleaned := make([]string, 0, len(items))
	for _, item := range items {
		s := strings.TrimSpace(item)
		if s == "" {
			continue
		}
		cleaned = append(cleaned, s)
	}
	if len(cleaned) == 0 {
		return elements
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**%s**\n", title)
	for _, item := range cleaned {
		// Each item may itself contain "\n- " when the runner already
		// pre-flattened a nested array; we keep that as-is.
		fmt.Fprintf(&b, "- %s\n", item)
	}
	return append(elements, cardMarkdown(strings.TrimRight(b.String(), "\n")))
}

func hasStructuredContent(r AnalysisReport) bool {
	return strings.TrimSpace(r.Title) != "" ||
		len(r.Facts) > 0 || len(r.Judgement) > 0 ||
		len(r.NextSteps) > 0 || len(r.References) > 0
}

func copilotMetaLine(req AnalysisRequest) string {
	parts := []string{}
	if v := strings.TrimSpace(req.Service); v != "" {
		parts = append(parts, fmt.Sprintf("**服务** %s", v))
	}
	if v := strings.TrimSpace(req.Env); v != "" {
		parts = append(parts, fmt.Sprintf("**环境** %s", v))
	}
	if v := strings.TrimSpace(req.Severity); v != "" {
		parts = append(parts, fmt.Sprintf("**级别** %s", v))
	}
	if v := strings.TrimSpace(req.AlertName); v != "" {
		parts = append(parts, fmt.Sprintf("**告警** %s", v))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "  ·  ")
}

func copilotCardTemplate(req AnalysisRequest) string {
	switch strings.ToLower(req.Severity) {
	case "critical", "p0", "p1":
		return "red"
	case "warning", "warn", "p2":
		return "orange"
	case "info", "p3", "p4":
		return "blue"
	}
	return "purple"
}

// buildAssigneeDMCard 构造发给责任人的私聊提醒卡片。
//
// 和群卡片相比刻意做减法：DM 是个人收件箱，只显示最关键的信息让人能"瞄一眼
// 就知道是谁的告警、是不是该立刻看"。详细分析仍然在群卡片里展开。
//
//   - Header 染色复用 cardTemplate（FIRING=红、RESOLVED=蓝）
//   - 字段：服务 / 级别 / 环境 / Incident #N
//   - summary 用 lark_md 显示一行摘要
//   - 按钮："去处理" 触发和群卡片同款的 claim callback（ACK 后群卡片同步刷新），
//     另加 "打开大盘" URL 跳转
//
// 注意：这里复用 cardCallbackButton + cardActions 用过的 callback shape，
// 这样 handleCardAction 不需要任何改动，DM 上点"我来处理"也走 backend.AckIncident。
func buildAssigneeDMCard(payload grafanaWebhook, ctxInfo cardContext) feishuCard {
	status := firstNonEmpty(payload.Status, "unknown")
	alertName := firstNonEmpty(payload.CommonLabels["alertname"], payload.Title, "Grafana Alert")
	service := firstNonEmpty(payload.CommonLabels["service"], "-")
	env := firstNonEmpty(payload.CommonLabels["env"], payload.CommonLabels["namespace"], "-")
	severity := firstNonEmpty(payload.CommonLabels["severity"], "-")
	summary := firstNonEmpty(payload.CommonAnnotations["summary"], payload.Title, "-")

	card := feishuCard{
		Config: map[string]any{"wide_screen_mode": true},
		Header: feishuHeader{
			Template: cardTemplate(status),
			Title: feishuCardText{
				Tag:     "plain_text",
				Content: fmt.Sprintf("📣 你被分配了告警：%s", alertName),
			},
		},
		Elements: []map[string]any{
			cardMarkdown("你是当班，**麻烦先在群里点 \"我来处理\"** 接走它（这样队友就不会重复跟）。"),
			{
				"tag": "div",
				"fields": []map[string]any{
					cardField("服务", service),
					cardField("级别", severity),
					cardField("环境", env),
					cardField("Incident", incidentLabel(ctxInfo.IncidentID)),
				},
			},
			cardMarkdown(fmt.Sprintf("**摘要**：%s", escapeLarkMarkdown(summary))),
		},
	}

	// DM 上的按钮：直接走和群卡片同 shape 的 claim callback，让用户在 DM 里也能
	// 一键 ACK；ACK 后 backend 会更新 incident，群卡片下一次状态查询/edit 时
	// 自然反映出来。incident_id 为空时退化为只有跳转大盘按钮。
	var actions []map[string]any
	idStr := ""
	if ctxInfo.IncidentID > 0 {
		idStr = strconv.FormatInt(ctxInfo.IncidentID, 10)
	}
	if idStr != "" {
		actions = append(actions, cardCallbackButton("我来处理", map[string]string{
			"action":      "claim",
			"alertname":   alertName,
			"incident_id": idStr,
		}))
	}
	if link := grafanaLink(payload); link != "" {
		actions = append(actions, cardURLButton("打开大盘", link))
	}
	if len(actions) > 0 {
		card.Elements = append(card.Elements, map[string]any{
			"tag":     "action",
			"actions": actions,
		})
	}

	if idStr != "" {
		card.Elements = append(card.Elements, map[string]any{
			"tag": "note",
			"elements": []map[string]any{
				{
					"tag":     "lark_md",
					"content": fmt.Sprintf("incident #%s · 这条 DM 由值班调度自动发送", idStr),
				},
			},
		})
	}
	return card
}

// incidentLabel 把 0 渲染成"-"，>0 渲染成"#N"，避免 DM 上出现"#0"。
func incidentLabel(id int64) string {
	if id <= 0 {
		return "-"
	}
	return fmt.Sprintf("#%d", id)
}

func cardActions(payload grafanaWebhook, enableCallback bool, ctxInfo cardContext) []map[string]any {
	var actions []map[string]any
	link := grafanaLink(payload)
	alertName := firstNonEmpty(payload.CommonLabels["alertname"], payload.Title, "Grafana Alert")
	status := firstNonEmpty(payload.Status, "unknown")
	active := isActiveAlertStatus(status)
	labels := mergedAlertLabels(payload)
	currentValue := ""
	triggeredAt := ""
	if len(payload.Alerts) > 0 {
		alert := payload.Alerts[0]
		alertName = firstNonEmpty(alert.Labels["alertname"], alertName)
		currentValue = formatValues(alert.Values)
		if !alert.StartsAt.IsZero() {
			triggeredAt = alert.StartsAt.Format(time.RFC3339)
		}
	}
	idStr := ""
	if ctxInfo.IncidentID > 0 {
		idStr = strconv.FormatInt(ctxInfo.IncidentID, 10)
	}
	if enableCallback && active {
		silenceMatch := alertSilenceMatchFromPayload(payload)
		actions = append(actions, cardCallbackButton("我来处理", map[string]string{
			"action":      "claim",
			"alertname":   alertName,
			"link":        link,
			"incident_id": idStr,
		}))
		// 「已修复」「标为误报」仅在接入 backend 时才显示，否则点击只能 toast 失败。
		if idStr != "" {
			actions = append(actions, cardCallbackButton("已修复", map[string]string{
				"action":      "resolve",
				"alertname":   alertName,
				"incident_id": idStr,
			}))
			actions = append(actions, cardCallbackButton("标为误报", map[string]string{
				"action":      "discard",
				"alertname":   alertName,
				"incident_id": idStr,
			}))
		}
		actions = append(actions, cardCallbackButton("AI 归因", map[string]string{
			"action":        "copilot_analyze",
			"alertname":     alertName,
			"service":       payload.CommonLabels["service"],
			"env":           firstNonEmpty(payload.CommonLabels["env"], payload.CommonLabels["namespace"]),
			"severity":      payload.CommonLabels["severity"],
			"status":        status,
			"category":      labels["category"],
			"route":         labels["route"],
			"target":        labels["target"],
			"mode":          labels["mode"],
			"action_name":   labels["action"],
			"channel":       labels["channel"],
			"pod":           labels["pod"],
			"current_value": currentValue,
			"summary":       payload.CommonAnnotations["summary"],
			"description":   payload.CommonAnnotations["description"],
			"link":          link,
			"starts_at":     triggeredAt,
			"incident_id":   idStr,
		}))
		actions = append(actions, cardCallbackButton("进重构队列", map[string]string{
			"action":        "refactor_enqueue",
			"repo":          firstNonEmpty(labels["repo"], labels["repository"], labels["project"], labels["service"]),
			"alertname":     alertName,
			"service":       payload.CommonLabels["service"],
			"env":           firstNonEmpty(payload.CommonLabels["env"], payload.CommonLabels["namespace"]),
			"severity":      payload.CommonLabels["severity"],
			"status":        status,
			"category":      labels["category"],
			"route":         labels["route"],
			"target":        labels["target"],
			"mode":          labels["mode"],
			"metric_action": labels["action"],
			"channel":       labels["channel"],
			"pod":           labels["pod"],
			"current_value": currentValue,
			"summary":       payload.CommonAnnotations["summary"],
			"description":   payload.CommonAnnotations["description"],
			"link":          link,
			"starts_at":     triggeredAt,
			"incident_id":   idStr,
		}))
		actions = append(actions, cardCallbackSelect("选择屏蔽时长", map[string]string{
			"action":      "silence_alert",
			"fingerprint": silenceMatch.Fingerprint,
			"alertname":   silenceMatch.AlertName,
			"service":     silenceMatch.Service,
			"env":         silenceMatch.Env,
			"severity":    silenceMatch.Severity,
			"incident_id": idStr,
		}, alertSilenceDurationOptions(silenceMatch.Severity)))
	} else if link != "" && active {
		actions = append(actions, cardURLButton("我来处理", link))
	}
	if link != "" {
		actions = append(actions, cardURLButton("打开大盘", link))
	}
	if payload.ExternalURL != "" && payload.ExternalURL != link {
		actions = append(actions, cardURLButton("打开 Grafana", payload.ExternalURL))
	}
	return actions
}

func mergedAlertLabels(payload grafanaWebhook) map[string]string {
	labels := make(map[string]string, len(payload.CommonLabels)+8)
	for k, v := range payload.CommonLabels {
		labels[k] = v
	}
	if len(payload.Alerts) > 0 {
		for k, v := range payload.Alerts[0].Labels {
			labels[k] = v
		}
	}
	return labels
}

func alertContextFields(payload grafanaWebhook) []map[string]any {
	labels := mergedAlertLabels(payload)
	operation := firstNonEmpty(labels["operation"], labels["mode"], joinNonEmpty("/", labels["action"], labels["channel"]), labels["channel"])
	items := []struct {
		label string
		value string
	}{
		{"类别", labels["category"]},
		{"Route", labels["route"]},
		{"下游", labels["target"]},
		{"业务维度", operation},
		{"Pod", labels["pod"]},
	}
	var fields []map[string]any
	for _, item := range items {
		if strings.TrimSpace(item.value) == "" {
			continue
		}
		fields = append(fields, cardField(item.label, item.value))
	}
	return fields
}

func joinNonEmpty(sep string, values ...string) string {
	var parts []string
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			parts = append(parts, strings.TrimSpace(value))
		}
	}
	return strings.Join(parts, sep)
}

func grafanaLink(payload grafanaWebhook) string {
	if len(payload.Alerts) > 0 {
		alert := payload.Alerts[0]
		if link := firstNonEmpty(alert.PanelURL, alert.DashboardURL, alert.GeneratorURL); link != "" {
			return link
		}
	}
	if payload.ExternalURL != "" {
		return payload.ExternalURL
	}
	switch payload.CommonLabels["service"] {
	case "matrix-api":
		return matrixAPIDashboardURL
	case "user-srv":
		return userSrvDashboardURL
	}
	return defaultGrafanaURL
}

func cardURLButton(text, url string) map[string]any {
	return map[string]any{
		"tag": "button",
		"text": map[string]string{
			"tag":     "plain_text",
			"content": text,
		},
		"url":  url,
		"type": "primary",
	}
}

func cardCallbackButton(text string, value map[string]string) map[string]any {
	return cardCallbackButtonOfType(text, "primary", value)
}

func cardCallbackButtonOfType(text, buttonType string, value map[string]string) map[string]any {
	if strings.TrimSpace(buttonType) == "" {
		buttonType = "primary"
	}
	return map[string]any{
		"tag": "button",
		"text": map[string]string{
			"tag":     "plain_text",
			"content": text,
		},
		"type":  buttonType,
		"value": value,
	}
}

func cardCallbackSelect(placeholder string, value map[string]string, options []alertSilenceDurationOption) map[string]any {
	selectOptions := make([]map[string]any, 0, len(options))
	for _, option := range options {
		selectOptions = append(selectOptions, map[string]any{
			"text": map[string]string{
				"tag":     "plain_text",
				"content": option.Label,
			},
			"value": option.Duration,
		})
	}
	return map[string]any{
		"tag": "select_static",
		"placeholder": map[string]string{
			"tag":     "plain_text",
			"content": placeholder,
		},
		"type":    "default",
		"width":   "default",
		"name":    "silence_duration",
		"options": selectOptions,
		"behaviors": []map[string]any{
			{
				"type":  "callback",
				"value": value,
			},
		},
	}
}

type feishuCardActionEvent struct {
	Operator struct {
		TenantKey string `json:"tenant_key"`
		OpenID    string `json:"open_id"`
		UserID    string `json:"user_id"`
		UnionID   string `json:"union_id"`
	} `json:"operator"`
	Action struct {
		Tag       string            `json:"tag"`
		Name      string            `json:"name"`
		Option    string            `json:"option"`
		Value     map[string]string `json:"value"`
		FormValue map[string]any    `json:"form_value"`
	} `json:"action"`
	Context struct {
		OpenMessageID string `json:"open_message_id"`
		OpenChatID    string `json:"open_chat_id"`
	} `json:"context"`
	MessageID string `json:"message_id"`
}

type feishuMessageReceiveEvent struct {
	Sender struct {
		SenderID struct {
			OpenID  string `json:"open_id"`
			UserID  string `json:"user_id"`
			UnionID string `json:"union_id"`
		} `json:"sender_id"`
	} `json:"sender"`
	Message struct {
		MessageID   string `json:"message_id"`
		RootID      string `json:"root_id"`
		ParentID    string `json:"parent_id"`
		ThreadID    string `json:"thread_id"`
		ChatID      string `json:"chat_id"`
		ChatType    string `json:"chat_type"`
		MessageType string `json:"message_type"`
		Content     string `json:"content"`
	} `json:"message"`
}

type feishuCardCallbackResponse struct {
	Toast *feishuCardToast    `json:"toast,omitempty"`
	Card  *feishuCallbackCard `json:"card,omitempty"`
}

type feishuCallbackCard struct {
	Type string     `json:"type"`
	Data feishuCard `json:"data"`
}

type feishuCardToast struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

func (s *server) handleCardAction(ctx context.Context, raw json.RawMessage) (feishuCardCallbackResponse, error) {
	// DEBUG (temporary, Phase 7-D verification): dump the exact Feishu
	var event feishuCardActionEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return feishuCardCallbackResponse{}, err
	}
	action := event.Action.Value["action"]
	switch action {
	case "claim":
		return s.handleClaimAction(ctx, event)
	case "resolve":
		return s.handleResolveAction(ctx, event)
	case "discard":
		return s.handleDiscardAction(ctx, event)
	case "copilot_analyze":
		return s.handleCopilotAnalyzeAction(event), nil
	case "refactor_enqueue":
		return s.handleRefactorEnqueueAction(ctx, event), nil
	case "silence_alert":
		return s.handleSilenceAlertAction(ctx, event)
	case "dirty_work_pick":
		return s.handleDirtyWorkPickAction(ctx, event)
	case "dirty_work_status":
		return s.handleDirtyWorkStatusAction(ctx, event)
	case "dirty_work_repick":
		return s.handleDirtyWorkRepickAction(ctx, event)
	case "dirty_work_topic_done":
		return s.handleDirtyWorkTopicDoneAction(ctx, event)
	case "pm_task_update":
		return s.handlePMTaskAction(ctx, event)
	case testCaseReminderAction:
		return s.handleTestCaseReminderAction(ctx, event)
	case testCaseReminderSuppressAction:
		return s.handleTestCaseReminderAction(ctx, event)
	case productionAcceptanceCompleteAction:
		return s.handleProductionAcceptanceReminderAction(ctx, event)
	case productionAcceptanceSuppressAction:
		return s.handleProductionAcceptanceReminderAction(ctx, event)
	default:
		return feishuCardCallbackResponse{}, nil
	}
}

func (s *server) handleDirtyWorkMessageEvent(ctx context.Context, raw json.RawMessage) error {
	var event feishuMessageReceiveEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return err
	}
	if event.Message.MessageType != "" && event.Message.MessageType != "text" {
		return nil
	}
	text := feishuMessageText(event.Message.Content)
	normalizedText := normalizeDirtyWorkLaunchCommand(text)
	matched := isDirtyWorkLaunchCommand(normalizedText)
	if matched || strings.Contains(text, "杂活") || strings.Contains(normalizedText, "杂活") ||
		strings.Contains(text, "后端待处理问题") || strings.Contains(normalizedText, "后端待处理问题") ||
		normalizedText == "杂" || normalizedText == "/杂" ||
		normalizedText == "待处理" || normalizedText == "/待处理" {
		log.Printf("dirty-work message event chat=%s operator=%s text=%q normalized=%q matched=%t",
			event.Message.ChatID, event.Sender.SenderID.OpenID, truncateMsg(text, 80), normalizedText, matched)
	}
	if !matched {
		return nil
	}
	chatID := strings.TrimSpace(event.Message.ChatID)
	if chatID == "" {
		return nil
	}
	msg := feishuCardMessage{
		MsgType: "interactive",
		Card: buildDirtyWorkLauncherCard("后端待处理问题", s.dirtyWorkCandidates(ctx), dirtyWorkLauncherOptions{
			RecordURL: s.dirtyWorkRecordURL,
		}),
	}
	messageID, err := s.sendFeishuAppCardTo(ctx, chatID, "chat_id", msg)
	if err != nil {
		return err
	}
	log.Printf("dirty-work launcher sent by message command chat=%s operator=%s message=%s",
		chatID, event.Sender.SenderID.OpenID, messageID)
	return nil
}

func (s *server) handleUserFeedbackOncallMessageEvent(ctx context.Context, raw json.RawMessage, now time.Time) error {
	if s == nil || s.feishuAppID == "" || s.feishuAppSecret == "" {
		return nil
	}
	var event feishuMessageReceiveEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return err
	}
	if event.Message.MessageType != "" && event.Message.MessageType != "text" {
		return nil
	}
	chatID := strings.TrimSpace(event.Message.ChatID)
	messageID := strings.TrimSpace(event.Message.MessageID)
	text := feishuMessageText(event.Message.Content)
	if chatID == "" || messageID == "" || strings.TrimSpace(text) == "" {
		return nil
	}
	s.refreshUserFeedbackOncallFromBackendIfStale(ctx, now)
	cfg, ok := s.effectiveUserFeedbackOncallConfig(now)
	if !ok || !strings.EqualFold(chatID, strings.TrimSpace(cfg.ChatID)) {
		return nil
	}
	if userFeedbackOncallLooksLikeReminder(text, cfg.ReplyPrefix) {
		return nil
	}
	threadKeys := userFeedbackOncallThreadKeys(event)
	if s.seenUserFeedbackOncallThreadWithTTL(threadKeys, now, cfg.MentionTTL) {
		log.Printf("user-feedback oncall skipped duplicate thread chat=%s message=%s thread=%s", chatID, messageID, strings.Join(threadKeys, ","))
		return nil
	}
	reply := buildUserFeedbackOncallReply(cfg.ReplyPrefix, cfg.Assignees)
	if strings.TrimSpace(reply) == "" {
		s.forgetUserFeedbackOncallThread(threadKeys)
		return nil
	}
	if err := s.replyFeishuMessage(ctx, messageID, reply); err != nil {
		s.forgetUserFeedbackOncallThread(threadKeys)
		return err
	}
	assigneeIDs := make([]string, 0, len(cfg.Assignees))
	for _, a := range cfg.Assignees {
		assigneeIDs = append(assigneeIDs, a.OpenID)
	}
	log.Printf("user-feedback oncall notified chat=%s message=%s thread=%s source=%s assignees=%s",
		chatID, messageID, strings.Join(threadKeys, ","), cfg.Source, strings.Join(assigneeIDs, ","))
	return nil
}

func userFeedbackOncallThreadKeys(event feishuMessageReceiveEvent) []string {
	return nonEmptyStrings([]string{
		strings.TrimSpace(event.Message.RootID),
		strings.TrimSpace(event.Message.ParentID),
		strings.TrimSpace(event.Message.ThreadID),
		strings.TrimSpace(event.Message.MessageID),
	})
}

func (s *server) seenUserFeedbackOncallThread(threadKeys []string, now time.Time) bool {
	return s.seenUserFeedbackOncallThreadWithTTL(threadKeys, now, s.userFeedbackOncallMentionTTL)
}

func (s *server) seenUserFeedbackOncallThreadWithTTL(threadKeys []string, now time.Time, ttl time.Duration) bool {
	threadKeys = nonEmptyStrings(threadKeys)
	if s == nil || len(threadKeys) == 0 {
		return false
	}
	if ttl <= 0 {
		ttl = defaultUserFeedbackOncallMentionTTL
	}
	s.userFeedbackOncallMentionMu.Lock()
	defer s.userFeedbackOncallMentionMu.Unlock()
	if s.userFeedbackOncallMentionAt == nil {
		s.userFeedbackOncallMentionAt = map[string]time.Time{}
	}
	for key, at := range s.userFeedbackOncallMentionAt {
		if now.Sub(at) > ttl {
			delete(s.userFeedbackOncallMentionAt, key)
		}
	}
	for _, threadKey := range threadKeys {
		if at, ok := s.userFeedbackOncallMentionAt[threadKey]; ok && now.Sub(at) <= ttl {
			return true
		}
	}
	for _, threadKey := range threadKeys {
		s.userFeedbackOncallMentionAt[threadKey] = now
	}
	return false
}

func (s *server) forgetUserFeedbackOncallThread(threadKeys []string) {
	threadKeys = nonEmptyStrings(threadKeys)
	if s == nil || len(threadKeys) == 0 {
		return
	}
	s.userFeedbackOncallMentionMu.Lock()
	defer s.userFeedbackOncallMentionMu.Unlock()
	for _, threadKey := range threadKeys {
		delete(s.userFeedbackOncallMentionAt, threadKey)
	}
}

func userFeedbackOncallLooksLikeReminder(text, prefix string) bool {
	text = strings.TrimSpace(text)
	prefix = strings.TrimSpace(prefix)
	if text == "" || prefix == "" {
		return false
	}
	return strings.Contains(text, prefix)
}

func pickUserFeedbackOncallAssignee(candidates []string, now time.Time) string {
	candidates = splitOpenIDList(strings.Join(candidates, ","))
	if len(candidates) == 0 {
		return ""
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.Local
	}
	local := now.In(loc)
	year, month, day := local.Date()
	dayStart := time.Date(year, month, day, 0, 0, 0, 0, loc)
	idx := int(dayStart.Unix()/86400) % len(candidates)
	if idx < 0 {
		idx = -idx
	}
	return candidates[idx]
}

func formatFeishuTextMention(openID, label string) string {
	openID = strings.TrimSpace(openID)
	if openID == "" {
		return ""
	}
	label = strings.TrimSpace(label)
	if label == "" {
		label = "值班人"
	}
	return fmt.Sprintf(`<at user_id="%s">%s</at>`, openID, label)
}

func feishuMessageText(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &body); err == nil {
		return strings.TrimSpace(body.Text)
	}
	return content
}

func isDirtyWorkLaunchCommand(text string) bool {
	text = normalizeDirtyWorkLaunchCommand(text)
	switch text {
	case "杂", "/杂", "杂活", "/杂活", "杂活分配", "/杂活分配", "发起杂活", "发起杂活分配",
		"待处理", "/待处理", "后端待处理问题", "/后端待处理问题", "发起后端待处理问题":
		return true
	default:
		return false
	}
}

func normalizeDirtyWorkLaunchCommand(text string) string {
	text = strings.TrimSpace(text)
	for strings.HasPrefix(text, "<at ") {
		end := strings.Index(text, "</at>")
		if end < 0 {
			break
		}
		text = strings.TrimSpace(text[end+len("</at>"):])
	}
	for strings.HasPrefix(text, "@") {
		withoutAt := strings.TrimPrefix(text, "@")
		if withoutAt == "" {
			return ""
		}
		if idx := strings.IndexAny(withoutAt, " \t\r\n"); idx >= 0 {
			text = strings.TrimSpace(withoutAt[idx+1:])
			continue
		}
		if idx := strings.Index(withoutAt, "/"); idx >= 0 {
			text = strings.TrimSpace(withoutAt[idx:])
			continue
		}
		break
	}
	return strings.TrimSpace(text)
}

func (s *server) handleDirtyWorkPickAction(ctx context.Context, event feishuCardActionEvent) (feishuCardCallbackResponse, error) {
	task := dirtyWorkTaskFromAction(event)
	if strings.TrimSpace(task) == "" {
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "先填一下任务内容"},
		}, nil
	}
	candidates := decodeDirtyWorkCandidates(event.Action.Value["candidates"])
	if len(candidates) == 0 {
		candidates = s.dirtyWorkCandidates(ctx)
	}
	assignee, err := s.resolveDirtyWorkPickAssignee(ctx, candidates, event.Action.Value)
	if err != nil {
		toast := "候选人为空，不能分配"
		if errors.Is(err, errDirtyWorkAssigneeNotFound) {
			toast = "指定候选人不在候选池，请重试"
		}
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: toast},
		}, nil
	}

	operator := formatFeishuOperator(event.Operator.OpenID, event.Operator.UserID, event.Operator.UnionID)
	result := buildDirtyWorkResultCard(task, assignee, operator, dirtyWorkResultOptions{
		Candidates: candidates,
		RecordURL:  s.dirtyWorkRecordURL,
	})
	chatID := strings.TrimSpace(event.Context.OpenChatID)
	resultMessageID := firstNonEmpty(event.Context.OpenMessageID, event.MessageID)
	log.Printf("dirty-work assigned task=%q assignee=%s operator=%s chat=%s mode=%s",
		truncateMsg(task, 80), assignee.OpenID, event.Operator.OpenID, chatID,
		dirtyWorkPickMode(event.Action.Value["assignee_open_id"]))
	s.notifyDirtyWorkAssigneeDM(task, assignee, operator, dirtyWorkAssigneeDMOptions{
		Candidates:      candidates,
		RecordURL:       s.dirtyWorkRecordURL,
		SourceChatID:    chatID,
		SourceMessageID: resultMessageID,
	})
	s.recordDirtyWorkAssignment(dirtyWorkRecord{
		Task:           task,
		Operator:       operator,
		OperatorOpenID: event.Operator.OpenID,
		AssigneeName:   assignee.Name,
		AssigneeOpenID: assignee.OpenID,
		Action:         "分配",
		Status:         "已分配",
		ChatID:         chatID,
		MessageID:      resultMessageID,
		CreatedAt:      time.Now(),
	})
	return feishuCardCallbackResponse{
		Toast: &feishuCardToast{Type: "success", Content: "已分配给" + assignee.Name},
		Card:  rawFeishuCallbackCard(result),
	}, nil
}

func (s *server) handleDirtyWorkRepickAction(ctx context.Context, event feishuCardActionEvent) (feishuCardCallbackResponse, error) {
	current := dirtyWorkCandidate{
		Name:   firstNonEmpty(event.Action.Value["assignee_name"], event.Action.Value["assignee_open_id"]),
		OpenID: strings.TrimSpace(event.Action.Value["assignee_open_id"]),
	}
	if current.OpenID == "" {
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "缺少当前负责人，不能重新分配"},
		}, nil
	}
	if !dirtyWorkActionByAssignee(event, current.OpenID) {
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "只有当前负责人可以点没时间"},
		}, nil
	}
	task := firstNonEmpty(event.Action.Value["task"], dirtyWorkTaskFromAction(event))
	candidates := decodeDirtyWorkCandidates(event.Action.Value["candidates"])
	if len(candidates) == 0 {
		candidates = s.dirtyWorkCandidates(ctx)
	}
	assignee, err := nextDirtyWorkCandidateAfter(candidates, current.OpenID)
	if err != nil {
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "没有其他候选人可分配"},
		}, nil
	}

	operator := formatFeishuOperator(event.Operator.OpenID, event.Operator.UserID, event.Operator.UnionID)
	result := buildDirtyWorkResultCard(task, assignee, operator, dirtyWorkResultOptions{
		Candidates:       candidates,
		PreviousAssignee: current,
		RecordURL:        s.dirtyWorkRecordURL,
	})
	chatID := strings.TrimSpace(event.Context.OpenChatID)
	resultMessageID := firstNonEmpty(event.Context.OpenMessageID, event.MessageID)
	log.Printf("dirty-work reassigned task=%q previous=%s assignee=%s operator=%s chat=%s",
		truncateMsg(task, 80), current.OpenID, assignee.OpenID, event.Operator.OpenID, chatID)
	s.rememberDirtyWorkRotation(assignee.OpenID)
	s.notifyDirtyWorkAssigneeDM(task, assignee, operator, dirtyWorkAssigneeDMOptions{
		Candidates:       candidates,
		PreviousAssignee: current,
		RecordURL:        s.dirtyWorkRecordURL,
		SourceChatID:     chatID,
		SourceMessageID:  resultMessageID,
	})
	s.recordDirtyWorkAssignment(dirtyWorkRecord{
		Task:           task,
		Operator:       operator,
		OperatorOpenID: event.Operator.OpenID,
		AssigneeName:   assignee.Name,
		AssigneeOpenID: assignee.OpenID,
		PreviousName:   current.Name,
		PreviousOpenID: current.OpenID,
		Action:         "重新分配",
		Status:         "已分配",
		ChatID:         chatID,
		MessageID:      resultMessageID,
		CreatedAt:      time.Now(),
	})
	return feishuCardCallbackResponse{
		Toast: &feishuCardToast{Type: "success", Content: "已重新分配给" + assignee.Name},
		Card:  rawFeishuCallbackCard(result),
	}, nil
}

func (s *server) handleDirtyWorkStatusAction(ctx context.Context, event feishuCardActionEvent) (feishuCardCallbackResponse, error) {
	current := dirtyWorkCandidate{
		Name:   firstNonEmpty(event.Action.Value["assignee_name"], event.Action.Value["assignee_open_id"]),
		OpenID: strings.TrimSpace(event.Action.Value["assignee_open_id"]),
	}
	if current.OpenID == "" {
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "缺少当前负责人，不能更新状态"},
		}, nil
	}
	if !dirtyWorkActionByAssignee(event, current.OpenID) {
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "只有当前负责人可以更新状态"},
		}, nil
	}
	status := strings.TrimSpace(event.Action.Value["status"])
	if status != "处理中" && status != "已处理" {
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "不支持的状态"},
		}, nil
	}
	task := firstNonEmpty(event.Action.Value["task"], dirtyWorkTaskFromAction(event))
	operator := firstNonEmpty(event.Action.Value["requester"], formatFeishuOperator(event.Operator.OpenID, event.Operator.UserID, event.Operator.UnionID))
	candidates := decodeDirtyWorkCandidates(event.Action.Value["candidates"])
	recordURL := firstNonEmpty(event.Action.Value["record_url"], s.dirtyWorkRecordURL)
	chatID := firstNonEmpty(event.Action.Value["source_chat_id"], strings.TrimSpace(event.Context.OpenChatID))
	messageID := firstNonEmpty(event.Action.Value["source_message_id"], firstNonEmpty(event.Context.OpenMessageID, event.MessageID))
	card := buildDirtyWorkAssigneeDMCard(task, current, operator, dirtyWorkAssigneeDMOptions{
		Candidates:      candidates,
		RecordURL:       recordURL,
		Status:          status,
		SourceChatID:    chatID,
		SourceMessageID: messageID,
	})
	log.Printf("dirty-work status updated task=%q assignee=%s status=%s operator=%s",
		truncateMsg(task, 80), current.OpenID, status, event.Operator.OpenID)
	s.updateDirtyWorkAssignmentStatus(dirtyWorkRecord{
		Task:           task,
		Operator:       operator,
		OperatorOpenID: event.Operator.OpenID,
		AssigneeName:   current.Name,
		AssigneeOpenID: current.OpenID,
		Action:         "状态更新",
		Status:         status,
		ChatID:         chatID,
		MessageID:      messageID,
		CreatedAt:      time.Now(),
	})
	return feishuCardCallbackResponse{
		Toast: &feishuCardToast{Type: "success", Content: "已更新为" + status},
		Card:  rawFeishuCallbackCard(card),
	}, nil
}

func (s *server) handleDirtyWorkTopicDoneAction(ctx context.Context, event feishuCardActionEvent) (feishuCardCallbackResponse, error) {
	recordID := strings.TrimSpace(event.Action.Value["record_id"])
	current := dirtyWorkCandidate{
		Name:   firstNonEmpty(event.Action.Value["assignee_name"], event.Action.Value["assignee_open_id"]),
		OpenID: strings.TrimSpace(event.Action.Value["assignee_open_id"]),
	}
	if recordID == "" || current.OpenID == "" {
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "缺少任务记录，不能确认处理"},
		}, nil
	}
	if !dirtyWorkActionByAssignee(event, current.OpenID) {
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "只有当前负责人可以确认自己这条"},
		}, nil
	}
	if s == nil || s.dirtyWorkBitable == nil {
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "多维表格未配置，不能回写状态"},
		}, nil
	}
	status := firstNonEmpty(event.Action.Value["status"], "已处理")
	operator := formatFeishuOperator(event.Operator.OpenID, event.Operator.UserID, event.Operator.UnionID)
	task := firstNonEmpty(event.Action.Value["task"], "待处理事项")
	if err := s.dirtyWorkBitable.UpdateRecordStatusByID(ctx, recordID, dirtyWorkRecord{
		Task:           task,
		Operator:       operator,
		OperatorOpenID: event.Operator.OpenID,
		AssigneeName:   current.Name,
		AssigneeOpenID: current.OpenID,
		Action:         "卡片确认处理",
		Status:         status,
		CreatedAt:      time.Now(),
	}); err != nil {
		log.Printf("dirty-work topic done failed record=%s assignee=%s operator=%s: %v", recordID, current.OpenID, event.Operator.OpenID, err)
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "error", Content: "回写多维表格失败，请稍后重试"},
		}, nil
	}
	log.Printf("dirty-work topic done record=%s task=%q assignee=%s operator=%s",
		recordID, truncateMsg(task, 80), current.OpenID, event.Operator.OpenID)
	return feishuCardCallbackResponse{
		Toast: &feishuCardToast{Type: "success", Content: "已确认处理，下轮不再提醒你"},
	}, nil
}

func dirtyWorkActionByAssignee(event feishuCardActionEvent, assigneeOpenID string) bool {
	assigneeOpenID = strings.TrimSpace(assigneeOpenID)
	return assigneeOpenID != "" && strings.TrimSpace(event.Operator.OpenID) == assigneeOpenID
}

func rawFeishuCallbackCard(card feishuCard) *feishuCallbackCard {
	return &feishuCallbackCard{
		Type: "raw",
		Data: card,
	}
}

func dirtyWorkTaskFromAction(event feishuCardActionEvent) string {
	for _, key := range []string{"dirty_work_task", "task", "task_content"} {
		if v := stringFromAny(event.Action.FormValue[key]); strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		if v := strings.TrimSpace(event.Action.Value[key]); v != "" {
			return v
		}
	}
	return ""
}

func stringFromAny(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	case map[string]any:
		return firstNonEmpty(stringFromAny(v["value"]), stringFromAny(v["text"]), stringFromAny(v["content"]))
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if s := strings.TrimSpace(stringFromAny(item)); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func defaultDirtyWorkCandidates() []dirtyWorkCandidate {
	raw := strings.TrimSpace(os.Getenv("DIRTY_WORK_CANDIDATES"))
	if raw == "" {
		return nil
	}
	return decodeDirtyWorkCandidates(raw)
}

func (s *server) dirtyWorkCandidates(ctx context.Context) []dirtyWorkCandidate {
	if s != nil && s.dirtyWorkBitable != nil {
		candidates, err := s.dirtyWorkBitable.FetchCandidates(ctx)
		if err != nil {
			log.Printf("dirty-work bitable candidates failed (falling back to env): %v", err)
		} else if len(candidates) > 0 {
			log.Printf("dirty-work bitable candidates loaded count=%d", len(candidates))
			return candidates
		} else {
			log.Printf("dirty-work bitable candidates empty (falling back to env)")
		}
	}
	return defaultDirtyWorkCandidates()
}

func (s *server) startDirtyWorkTimeoutReminder() func() {
	if s == nil || s.dirtyWorkBitable == nil || !s.feishuAppConfigured() {
		return func() {}
	}
	cfg := s.dirtyWorkReminder
	if strings.TrimSpace(cfg.ChatID) == "" || cfg.After <= 0 || cfg.Interval <= 0 {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				runCtx, runCancel := context.WithTimeout(ctx, 15*time.Second)
				if err := s.runDirtyWorkTimeoutReminderOnce(runCtx, now); err != nil {
					log.Printf("dirty-work timeout reminder failed: %v", err)
				}
				runCancel()
			}
		}
	}()
	return cancel
}

func (s *server) startDirtyWorkTopicReminder() func() {
	if s == nil || s.dirtyWorkBitable == nil || !s.feishuAppConfigured() {
		return func() {}
	}
	cfg := s.dirtyWorkTopicReminder
	if !cfg.Enabled || strings.TrimSpace(cfg.ChatID) == "" || strings.TrimSpace(cfg.TopicField) == "" || strings.TrimSpace(cfg.TopicValue) == "" || cfg.Interval <= 0 {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				runCtx, runCancel := context.WithTimeout(ctx, 15*time.Second)
				if err := s.runDirtyWorkTopicReminderOnce(runCtx, now); err != nil {
					log.Printf("dirty-work topic reminder failed: %v", err)
				}
				runCancel()
			}
		}
	}()
	return cancel
}

func (s *server) runDirtyWorkTimeoutReminderOnce(ctx context.Context, now time.Time) error {
	if s == nil || s.dirtyWorkBitable == nil || !s.feishuAppConfigured() {
		return nil
	}
	cfg := s.dirtyWorkReminder
	if strings.TrimSpace(cfg.ChatID) == "" || cfg.After <= 0 {
		return nil
	}
	records, err := s.dirtyWorkBitable.FetchRecords(ctx)
	if err != nil {
		return err
	}
	due := s.dirtyWorkTimeoutReminderDue(records, now)
	if len(due) == 0 {
		return nil
	}
	card := buildDirtyWorkTimeoutReminderCard(due, now, cfg.After, s.dirtyWorkRecordURL)
	if _, err := s.sendFeishuAppCardTo(ctx, cfg.ChatID, "chat_id", feishuCardMessage{MsgType: "interactive", Card: card}); err != nil {
		return err
	}
	s.markDirtyWorkTimeoutReminded(due, now)
	log.Printf("dirty-work timeout reminder sent chat=%s count=%d", cfg.ChatID, len(due))
	return nil
}

func (s *server) runDirtyWorkTopicReminderOnce(ctx context.Context, now time.Time) error {
	if s == nil || s.dirtyWorkBitable == nil || !s.feishuAppConfigured() {
		return nil
	}
	cfg := s.dirtyWorkTopicReminder
	if !cfg.Enabled || strings.TrimSpace(cfg.ChatID) == "" || strings.TrimSpace(cfg.TopicField) == "" || strings.TrimSpace(cfg.TopicValue) == "" {
		return nil
	}
	records, err := s.dirtyWorkBitable.FetchRecords(ctx)
	if err != nil {
		return err
	}
	due := dirtyWorkTopicReminderDue(records, cfg)
	if len(due) == 0 {
		return nil
	}
	key := dirtyWorkTopicReminderKey(cfg)
	if !s.shouldSendDirtyWorkTopicReminder(key, now) {
		return nil
	}
	card := buildDirtyWorkTopicReminderCard(due, now, cfg, s.dirtyWorkRecordURL)
	if _, err := s.sendFeishuAppCardTo(ctx, cfg.ChatID, "chat_id", feishuCardMessage{MsgType: "interactive", Card: card}); err != nil {
		return err
	}
	s.markDirtyWorkTopicReminded(key, now)
	log.Printf("dirty-work topic reminder sent chat=%s topic=%q count=%d", cfg.ChatID, cfg.TopicValue, len(due))
	return nil
}

func (s *server) dirtyWorkTimeoutReminderDue(records []dirtyWorkBitableRecord, now time.Time) []dirtyWorkBitableRecord {
	cfg := s.dirtyWorkReminder
	latest := latestDirtyWorkRecords(records)
	due := make([]dirtyWorkBitableRecord, 0, len(latest))
	for _, rec := range latest {
		if rec.CreatedAt.IsZero() || now.Sub(rec.CreatedAt) < cfg.After {
			continue
		}
		status := strings.TrimSpace(rec.Status)
		if status == "" || status == "已处理" {
			continue
		}
		key := dirtyWorkReminderKey(rec)
		if key == "" {
			continue
		}
		if !s.shouldSendDirtyWorkTimeoutReminder(key, now) {
			continue
		}
		due = append(due, rec)
	}
	return due
}

func dirtyWorkTopicReminderDue(records []dirtyWorkBitableRecord, cfg dirtyWorkTopicReminderConfig) []dirtyWorkBitableRecord {
	topic := strings.TrimSpace(cfg.TopicValue)
	if topic == "" {
		return nil
	}
	statuses := map[string]struct{}{}
	for _, status := range cfg.Statuses {
		status = strings.TrimSpace(status)
		if status != "" {
			statuses[status] = struct{}{}
		}
	}
	latest := latestDirtyWorkRecords(records)
	due := make([]dirtyWorkBitableRecord, 0, len(latest))
	for _, rec := range latest {
		if strings.TrimSpace(rec.Topic) != topic {
			continue
		}
		status := strings.TrimSpace(rec.Status)
		if len(statuses) > 0 {
			if _, ok := statuses[status]; !ok {
				continue
			}
		} else if status == "" || status == "已处理" {
			continue
		}
		due = append(due, rec)
	}
	return due
}

func latestDirtyWorkRecords(records []dirtyWorkBitableRecord) []dirtyWorkBitableRecord {
	latest := map[string]dirtyWorkBitableRecord{}
	for _, rec := range records {
		if strings.TrimSpace(rec.Task) == "" {
			continue
		}
		key := dirtyWorkReminderKey(rec)
		if key == "" {
			continue
		}
		existing, ok := latest[key]
		if !ok || rec.CreatedAt.After(existing.CreatedAt) {
			latest[key] = rec
		}
	}
	out := make([]dirtyWorkBitableRecord, 0, len(latest))
	for _, rec := range latest {
		out = append(out, rec)
	}
	return out
}

func dirtyWorkReminderKey(rec dirtyWorkBitableRecord) string {
	if strings.TrimSpace(rec.MessageID) != "" {
		return "msg:" + strings.TrimSpace(rec.MessageID)
	}
	if strings.TrimSpace(rec.ChatID) != "" {
		return "chat-task:" + strings.TrimSpace(rec.ChatID) + ":" + strings.TrimSpace(rec.Task)
	}
	if strings.TrimSpace(rec.Task) != "" {
		return "task:" + strings.TrimSpace(rec.Task)
	}
	return strings.TrimSpace(rec.RecordID)
}

func dirtyWorkTopicReminderKey(cfg dirtyWorkTopicReminderConfig) string {
	return "topic:" + strings.TrimSpace(cfg.TopicField) + "=" + strings.TrimSpace(cfg.TopicValue)
}

func (s *server) shouldSendDirtyWorkTopicReminder(key string, now time.Time) bool {
	cooldown := s.dirtyWorkTopicReminder.Cooldown
	if cooldown <= 0 {
		return true
	}
	s.dirtyWorkTopicReminderMu.Lock()
	defer s.dirtyWorkTopicReminderMu.Unlock()
	if s.dirtyWorkTopicReminderAt == nil {
		s.dirtyWorkTopicReminderAt = map[string]time.Time{}
	}
	last, ok := s.dirtyWorkTopicReminderAt[key]
	return !ok || now.Sub(last) >= cooldown
}

func (s *server) markDirtyWorkTopicReminded(key string, now time.Time) {
	if strings.TrimSpace(key) == "" {
		return
	}
	s.dirtyWorkTopicReminderMu.Lock()
	defer s.dirtyWorkTopicReminderMu.Unlock()
	if s.dirtyWorkTopicReminderAt == nil {
		s.dirtyWorkTopicReminderAt = map[string]time.Time{}
	}
	s.dirtyWorkTopicReminderAt[key] = now
}

func (s *server) shouldSendDirtyWorkTimeoutReminder(key string, now time.Time) bool {
	cooldown := s.dirtyWorkReminder.Cooldown
	if cooldown <= 0 {
		return true
	}
	s.dirtyWorkReminderMu.Lock()
	defer s.dirtyWorkReminderMu.Unlock()
	if s.dirtyWorkReminderAt == nil {
		s.dirtyWorkReminderAt = map[string]time.Time{}
	}
	last, ok := s.dirtyWorkReminderAt[key]
	return !ok || now.Sub(last) >= cooldown
}

func (s *server) markDirtyWorkTimeoutReminded(records []dirtyWorkBitableRecord, now time.Time) {
	s.dirtyWorkReminderMu.Lock()
	defer s.dirtyWorkReminderMu.Unlock()
	if s.dirtyWorkReminderAt == nil {
		s.dirtyWorkReminderAt = map[string]time.Time{}
	}
	for _, rec := range records {
		if key := dirtyWorkReminderKey(rec); key != "" {
			s.dirtyWorkReminderAt[key] = now
		}
	}
}

func (s *server) recordDirtyWorkAssignment(rec dirtyWorkRecord) {
	if s == nil || s.dirtyWorkBitable == nil || s.dirtyWorkBitable.cfg.RecordTableID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.dirtyWorkBitable.CreateRecord(ctx, rec); err != nil {
			log.Printf("dirty-work bitable record failed action=%s task=%q assignee=%s: %v",
				rec.Action, truncateMsg(rec.Task, 80), rec.AssigneeOpenID, err)
			return
		}
		log.Printf("dirty-work bitable record created action=%s task=%q assignee=%s",
			rec.Action, truncateMsg(rec.Task, 80), rec.AssigneeOpenID)
	}()
}

func (s *server) updateDirtyWorkAssignmentStatus(rec dirtyWorkRecord) {
	if s == nil || s.dirtyWorkBitable == nil || s.dirtyWorkBitable.cfg.RecordTableID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.dirtyWorkBitable.UpdateLatestRecordStatus(ctx, rec); err != nil {
			log.Printf("dirty-work bitable status update failed task=%q assignee=%s status=%s: %v",
				truncateMsg(rec.Task, 80), rec.AssigneeOpenID, rec.Status, err)
			return
		}
		log.Printf("dirty-work bitable status updated task=%q assignee=%s status=%s",
			truncateMsg(rec.Task, 80), rec.AssigneeOpenID, rec.Status)
	}()
}

func (s *server) notifyDirtyWorkAssigneeDM(task string, assignee dirtyWorkCandidate, operator string, opts dirtyWorkAssigneeDMOptions) {
	if s == nil || !s.feishuAppConfigured() || strings.TrimSpace(assignee.OpenID) == "" {
		return
	}
	card := buildDirtyWorkAssigneeDMCard(task, assignee, operator, opts)
	go func(openID string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := s.sendFeishuAppCardTo(ctx, openID, "open_id", feishuCardMessage{MsgType: "interactive", Card: card}); err != nil {
			log.Printf("dirty-work assignee DM failed task=%q assignee=%s: %v", truncateMsg(task, 80), openID, err)
			return
		}
		log.Printf("dirty-work assignee DM sent task=%q assignee=%s", truncateMsg(task, 80), openID)
	}(strings.TrimSpace(assignee.OpenID))
}

func normalizeDirtyWorkCandidates(candidates []dirtyWorkCandidate) []dirtyWorkCandidate {
	out := make([]dirtyWorkCandidate, 0, len(candidates))
	seen := make(map[string]struct{})
	for _, candidate := range candidates {
		name := strings.TrimSpace(candidate.Name)
		openID := strings.TrimSpace(candidate.OpenID)
		weight := candidate.Weight
		if weight < 0 {
			weight = 0
		}
		if openID == "" {
			continue
		}
		if name == "" {
			name = openID
		}
		key := strings.ToLower(openID)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, dirtyWorkCandidate{Name: name, OpenID: openID, Weight: weight})
	}
	return out
}

func encodeDirtyWorkCandidates(candidates []dirtyWorkCandidate) string {
	candidates = normalizeDirtyWorkCandidates(candidates)
	parts := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		part := strings.ReplaceAll(candidate.Name, ",", "，") + "|" + candidate.OpenID
		if candidate.Weight > 0 {
			part += "|" + strconv.Itoa(candidate.Weight)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ",")
}

func decodeDirtyWorkCandidates(raw string) []dirtyWorkCandidate {
	var candidates []dirtyWorkCandidate
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var name, openID string
		weight := 0
		if pieces := strings.Split(part, "|"); len(pieces) >= 2 {
			name = pieces[0]
			openID = pieces[1]
			if len(pieces) >= 3 {
				weight = dirtyWorkBitableInt(pieces[2], 0)
			}
		} else if left, right, ok := strings.Cut(part, ":"); ok {
			name = left
			openID = right
		} else {
			openID = part
			name = part
		}
		candidates = append(candidates, dirtyWorkCandidate{Name: name, OpenID: openID, Weight: weight})
	}
	return normalizeDirtyWorkCandidates(candidates)
}

var errDirtyWorkAssigneeNotFound = errors.New("dirty work assignee not found")

func dirtyWorkPickMode(assigneeOpenID string) string {
	if strings.TrimSpace(assigneeOpenID) == "" {
		return "round_robin"
	}
	return "direct"
}

func findDirtyWorkCandidate(candidates []dirtyWorkCandidate, openID string) (dirtyWorkCandidate, bool) {
	openID = strings.ToLower(strings.TrimSpace(openID))
	if openID == "" {
		return dirtyWorkCandidate{}, false
	}
	for _, candidate := range normalizeDirtyWorkCandidates(candidates) {
		if strings.ToLower(strings.TrimSpace(candidate.OpenID)) == openID {
			return candidate, true
		}
	}
	return dirtyWorkCandidate{}, false
}

func (s *server) resolveDirtyWorkPickAssignee(
	ctx context.Context,
	candidates []dirtyWorkCandidate,
	value map[string]string,
) (dirtyWorkCandidate, error) {
	if openID := strings.TrimSpace(value["assignee_open_id"]); openID != "" {
		assignee, ok := findDirtyWorkCandidate(candidates, openID)
		if !ok {
			// 按钮上带了姓名但候选池刷新后 open_id 对不上时，用按钮值兜底一次。
			name := strings.TrimSpace(value["assignee_name"])
			if name == "" {
				return dirtyWorkCandidate{}, errDirtyWorkAssigneeNotFound
			}
			assignee = dirtyWorkCandidate{Name: name, OpenID: openID}
		}
		s.rememberDirtyWorkRotation(assignee.OpenID)
		return assignee, nil
	}
	return s.pickDirtyWorkCandidate(ctx, candidates)
}

func (s *server) pickDirtyWorkCandidate(ctx context.Context, candidates []dirtyWorkCandidate) (dirtyWorkCandidate, error) {
	assignee, err := pickNextDirtyWorkCandidate(candidates, s.dirtyWorkRotationCursor(ctx, candidates))
	if err != nil {
		return dirtyWorkCandidate{}, err
	}
	s.rememberDirtyWorkRotation(assignee.OpenID)
	return assignee, nil
}

func pickNextDirtyWorkCandidate(candidates []dirtyWorkCandidate, afterOpenID string) (dirtyWorkCandidate, error) {
	candidates = normalizeDirtyWorkCandidates(candidates)
	if len(candidates) == 0 {
		return dirtyWorkCandidate{}, errors.New("empty dirty work candidates")
	}
	if strings.TrimSpace(afterOpenID) == "" {
		return candidates[0], nil
	}
	afterOpenID = strings.ToLower(strings.TrimSpace(afterOpenID))
	for idx, candidate := range candidates {
		if strings.ToLower(strings.TrimSpace(candidate.OpenID)) == afterOpenID {
			return candidates[(idx+1)%len(candidates)], nil
		}
	}
	return candidates[0], nil
}

func nextDirtyWorkCandidateAfter(candidates []dirtyWorkCandidate, currentOpenID string) (dirtyWorkCandidate, error) {
	candidates = normalizeDirtyWorkCandidates(candidates)
	if len(candidates) <= 1 {
		return dirtyWorkCandidate{}, errors.New("no other dirty work candidate")
	}
	next, err := pickNextDirtyWorkCandidate(candidates, currentOpenID)
	if err != nil {
		return dirtyWorkCandidate{}, err
	}
	if strings.EqualFold(strings.TrimSpace(next.OpenID), strings.TrimSpace(currentOpenID)) {
		return dirtyWorkCandidate{}, errors.New("no other dirty work candidate")
	}
	return next, nil
}

func (s *server) dirtyWorkRotationCursor(ctx context.Context, candidates []dirtyWorkCandidate) string {
	candidates = normalizeDirtyWorkCandidates(candidates)
	if len(candidates) <= 1 {
		return ""
	}
	if last := s.dirtyWorkRotationMemoryCursor(candidates); last != "" {
		return last
	}
	last := s.dirtyWorkRotationRecordCursor(ctx, candidates)
	if last != "" {
		s.rememberDirtyWorkRotation(last)
	}
	return last
}

func (s *server) dirtyWorkRotationMemoryCursor(candidates []dirtyWorkCandidate) string {
	if s == nil {
		return ""
	}
	s.dirtyWorkRotationMu.Lock()
	defer s.dirtyWorkRotationMu.Unlock()
	last := strings.TrimSpace(s.dirtyWorkRotationLastOpenID)
	if dirtyWorkCandidateInPool(candidates, last) {
		return last
	}
	return ""
}

func (s *server) dirtyWorkRotationRecordCursor(ctx context.Context, candidates []dirtyWorkCandidate) string {
	if s == nil || s.dirtyWorkBitable == nil || strings.TrimSpace(s.dirtyWorkBitable.cfg.RecordTableID) == "" {
		return ""
	}
	records, err := s.dirtyWorkBitable.FetchRecords(ctx)
	if err != nil {
		log.Printf("dirty-work rotation record cursor failed: %v", err)
		return ""
	}
	var latest dirtyWorkBitableRecord
	for _, rec := range records {
		openID := strings.TrimSpace(rec.AssigneeOpenID)
		if !dirtyWorkCandidateInPool(candidates, openID) {
			continue
		}
		if latest.AssigneeOpenID == "" || rec.CreatedAt.After(latest.CreatedAt) {
			latest = rec
		}
	}
	return strings.TrimSpace(latest.AssigneeOpenID)
}

func (s *server) rememberDirtyWorkRotation(openID string) {
	if s == nil {
		return
	}
	openID = strings.TrimSpace(openID)
	if openID == "" {
		return
	}
	s.dirtyWorkRotationMu.Lock()
	defer s.dirtyWorkRotationMu.Unlock()
	s.dirtyWorkRotationLastOpenID = openID
}

func dirtyWorkCandidateInPool(candidates []dirtyWorkCandidate, openID string) bool {
	openID = strings.ToLower(strings.TrimSpace(openID))
	if openID == "" {
		return false
	}
	for _, candidate := range candidates {
		if strings.ToLower(strings.TrimSpace(candidate.OpenID)) == openID {
			return true
		}
	}
	return false
}

func (s *server) handleClaimAction(ctx context.Context, event feishuCardActionEvent) (feishuCardCallbackResponse, error) {
	operator := formatFeishuOperator(event.Operator.OpenID, event.Operator.UserID, event.Operator.UnionID)
	alertName := firstNonEmpty(event.Action.Value["alertname"], "Grafana 告警")
	messageID := firstNonEmpty(event.Context.OpenMessageID, event.MessageID)
	if messageID == "" {
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "已收到认领，但没有找到原消息 ID"},
		}, nil
	}

	// 接入 backend：调 ack；失败时退化为只发 thread reply（不阻塞用户）。
	reassigned := false
	if id := parseInt64(event.Action.Value["incident_id"]); id > 0 && s.backend != nil && event.Operator.OpenID != "" {
		if _, ra, err := s.backend.Ack(ctx, id, event.Operator.OpenID); err != nil {
			log.Printf("alert backend ack failed incident=%d (degrading to thread reply only): %v", id, err)
		} else {
			reassigned = ra
		}
	}

	reply := fmt.Sprintf("%s 已认领处理：%s", operator, alertName)
	if reassigned {
		reply = fmt.Sprintf("%s 已接走处理（原责任人转出）：%s", operator, alertName)
	}
	if link := event.Action.Value["link"]; strings.TrimSpace(link) != "" {
		reply += "\n" + link
	}
	if err := s.replyFeishuMessage(ctx, messageID, reply); err != nil {
		return feishuCardCallbackResponse{}, err
	}
	return feishuCardCallbackResponse{
		Toast: &feishuCardToast{Type: "success", Content: "已记录认领"},
	}, nil
}

// handleResolveAction 把 incident 标记为已修复；未传 incident_id 或 backend
// 故障时返回 warning toast，并提示走 admin 后台兜底（避免用户以为已修复但
// 实际状态机没推进）。
func (s *server) handleResolveAction(ctx context.Context, event feishuCardActionEvent) (feishuCardCallbackResponse, error) {
	id := parseInt64(event.Action.Value["incident_id"])
	if id <= 0 || s.backend == nil {
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "未接入 incident 平台，请到 admin 后台标记修复"},
		}, nil
	}
	if event.Operator.OpenID == "" {
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "未识别操作人"},
		}, nil
	}
	messageID := firstNonEmpty(event.Context.OpenMessageID, event.MessageID)
	_, minutes, err := s.backend.Resolve(ctx, id, event.Operator.OpenID, "")
	if err != nil {
		log.Printf("alert backend resolve failed incident=%d: %v", id, err)
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "标记修复失败：" + truncateMsg(err.Error(), 120)},
		}, nil
	}
	operator := formatFeishuOperator(event.Operator.OpenID, event.Operator.UserID, event.Operator.UnionID)
	reply := fmt.Sprintf("%s 已标记修复（用时 %d 分钟）", operator, minutes)
	if messageID != "" {
		_ = s.replyFeishuMessage(ctx, messageID, reply)
	}
	return feishuCardCallbackResponse{
		Toast: &feishuCardToast{Type: "success", Content: "已记录修复"},
	}, nil
}

// handleDiscardAction 把 incident 标为误报；不计入 resolver 积分。
func (s *server) handleDiscardAction(ctx context.Context, event feishuCardActionEvent) (feishuCardCallbackResponse, error) {
	id := parseInt64(event.Action.Value["incident_id"])
	if id <= 0 || s.backend == nil {
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "未接入 incident 平台"},
		}, nil
	}
	if event.Operator.OpenID == "" {
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "未识别操作人"},
		}, nil
	}
	messageID := firstNonEmpty(event.Context.OpenMessageID, event.MessageID)
	if _, err := s.backend.Discard(ctx, id, event.Operator.OpenID, ""); err != nil {
		log.Printf("alert backend discard failed incident=%d: %v", id, err)
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "标记误报失败：" + truncateMsg(err.Error(), 120)},
		}, nil
	}
	operator := formatFeishuOperator(event.Operator.OpenID, event.Operator.UserID, event.Operator.UnionID)
	if messageID != "" {
		_ = s.replyFeishuMessage(ctx, messageID, fmt.Sprintf("%s 已将该告警标记为误报，不计入工作量", operator))
	}
	return feishuCardCallbackResponse{
		Toast: &feishuCardToast{Type: "success", Content: "已记录为误报"},
	}, nil
}

func (s *server) handleSilenceAlertAction(ctx context.Context, event feishuCardActionEvent) (feishuCardCallbackResponse, error) {
	operatorOpenID := strings.TrimSpace(event.Operator.OpenID)
	if operatorOpenID == "" {
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "未识别操作人，不能创建屏蔽"},
		}, nil
	}
	fingerprint := strings.TrimSpace(event.Action.Value["fingerprint"])
	if fingerprint == "" {
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "缺少告警 fingerprint，不能创建屏蔽"},
		}, nil
	}
	if s.alertSilences == nil {
		s.alertSilences = newAlertSilenceStore()
	}

	severity := firstNonEmpty(event.Action.Value["severity"], "-")
	duration, capped := normalizeAlertSilenceDuration(severity, firstNonEmpty(event.Action.Value["duration"], event.Action.Option))
	now := s.alertSilences.currentTime()
	item := alertSilence{
		Fingerprint:    fingerprint,
		AlertName:      firstNonEmpty(event.Action.Value["alertname"], "Grafana 告警"),
		Service:        firstNonEmpty(event.Action.Value["service"], "-"),
		Env:            firstNonEmpty(event.Action.Value["env"], "-"),
		Severity:       severity,
		OperatorOpenID: operatorOpenID,
		Reason:         firstNonEmpty(event.Action.Value["reason"], "飞书卡片手动屏蔽"),
		CreatedAt:      now,
		ExpiresAt:      now.Add(duration),
	}
	var persistErr error
	if s.backend != nil {
		if persisted, err := s.backend.CreateSilence(ctx, item); err != nil {
			persistErr = err
			log.Printf("alert backend create silence failed fp=%s (local fallback): %v", item.Fingerprint, err)
		} else {
			item = persisted
		}
	}
	item, err := s.alertSilences.Put(item)
	if err != nil {
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "创建屏蔽失败：" + truncateMsg(err.Error(), 120)},
		}, nil
	}

	log.Printf("alert silence created fp=%s alert=%s service=%s env=%s severity=%s duration=%s capped=%v operator=%s incident=%s expires_at=%s",
		item.Fingerprint, item.AlertName, item.Service, item.Env, item.Severity, duration, capped,
		item.OperatorOpenID, event.Action.Value["incident_id"], item.ExpiresAt.Format(time.RFC3339))

	operator := formatFeishuOperator(event.Operator.OpenID, event.Operator.UserID, event.Operator.UnionID)
	reply := fmt.Sprintf("%s 已屏蔽告警 %s：%s（%s/%s，%s）\n到期时间：%s",
		operator, formatAlertSilenceDuration(duration), item.AlertName, item.Service, item.Env, item.Severity,
		item.ExpiresAt.Format("2006-01-02 15:04:05"))
	if capped {
		reply += "\n已按当前告警级别限制自动截断屏蔽时长。"
	}
	if persistErr != nil {
		reply += "\n持久化失败，已临时在当前 forwarder 实例生效；服务重启后可能失效。"
	}
	if messageID := firstNonEmpty(event.Context.OpenMessageID, event.MessageID); messageID != "" {
		if err := s.replyFeishuMessage(ctx, messageID, reply); err != nil {
			log.Printf("alert silence reply failed fp=%s message=%s: %v", item.Fingerprint, messageID, err)
		}
	}
	toastType := "success"
	toastContent := "已屏蔽" + formatAlertSilenceDuration(duration)
	if persistErr != nil {
		toastType = "warning"
		toastContent = "已临时屏蔽" + formatAlertSilenceDuration(duration)
	}
	return feishuCardCallbackResponse{
		Toast: &feishuCardToast{Type: toastType, Content: toastContent},
	}, nil
}

func truncateMsg(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func parseInt64(s string) int64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func (s *server) handleCopilotAnalyzeAction(event feishuCardActionEvent) feishuCardCallbackResponse {
	messageID := firstNonEmpty(event.Context.OpenMessageID, event.MessageID)
	if messageID == "" {
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "已收到 AI 归因请求，但没有找到原消息 ID"},
		}
	}

	req := analysisRequestFromCardAction(event)
	go s.replyCopilotAnalysis(messageID, req)
	return feishuCardCallbackResponse{
		Toast: &feishuCardToast{Type: "success", Content: "AI 归因已开始，稍后会回复到当前线程"},
	}
}

func analysisRequestFromCardAction(event feishuCardActionEvent) AnalysisRequest {
	triggeredAt, _ := time.Parse(time.RFC3339, event.Action.Value["starts_at"])
	operation := firstNonEmpty(
		event.Action.Value["operation"],
		event.Action.Value["mode"],
		joinNonEmpty("/", event.Action.Value["action_name"], event.Action.Value["channel"]),
		event.Action.Value["channel"],
	)
	return AnalysisRequest{
		AlertName:      event.Action.Value["alertname"],
		Service:        event.Action.Value["service"],
		Env:            event.Action.Value["env"],
		Severity:       event.Action.Value["severity"],
		Status:         event.Action.Value["status"],
		Category:       event.Action.Value["category"],
		Route:          event.Action.Value["route"],
		Target:         event.Action.Value["target"],
		Operation:      operation,
		Pod:            event.Action.Value["pod"],
		CurrentValue:   event.Action.Value["current_value"],
		Summary:        event.Action.Value["summary"],
		Description:    event.Action.Value["description"],
		Link:           event.Action.Value["link"],
		Operator:       formatFeishuOperator(event.Operator.OpenID, event.Operator.UserID, event.Operator.UnionID),
		OperatorOpenID: event.Operator.OpenID,
		OpenChatID:     event.Context.OpenChatID,
		TriggeredAt:    triggeredAt,
	}
}

func (s *server) handleRefactorEnqueueAction(ctx context.Context, event feishuCardActionEvent) feishuCardCallbackResponse {
	if s.refactorOrchestrator == nil {
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "自动重构队列未配置"},
		}
	}
	trigger := s.refactorMetricTriggerFromCardAction(event)
	result, err := s.refactorOrchestrator.EnqueueMetric(ctx, trigger)
	if err != nil {
		log.Printf("refactor enqueue from card failed repo=%s service=%s alert=%s: %v",
			trigger.Repo, trigger.Service, trigger.Labels["alertname"], err)
		return feishuCardCallbackResponse{
			Toast: &feishuCardToast{Type: "warning", Content: "入队失败：" + truncateMsg(err.Error(), 120)},
		}
	}
	log.Printf("refactor enqueue from card ok repo=%s service=%s alert=%s item=%s duplicate=%v",
		trigger.Repo, trigger.Service, trigger.Labels["alertname"], result.ItemID, result.Duplicate)
	content := "已加入自动重构队列"
	if result.Duplicate {
		content = "已合并到已有重构任务"
	}
	return feishuCardCallbackResponse{
		Toast: &feishuCardToast{Type: "success", Content: content},
	}
}

func (s *server) refactorMetricTriggerFromCardAction(event feishuCardActionEvent) refactorMetricTrigger {
	values := event.Action.Value
	labels := map[string]string{
		"alertname":   values["alertname"],
		"service":     values["service"],
		"env":         values["env"],
		"severity":    values["severity"],
		"status":      values["status"],
		"category":    values["category"],
		"route":       values["route"],
		"target":      values["target"],
		"mode":        values["mode"],
		"action":      values["metric_action"],
		"channel":     values["channel"],
		"pod":         values["pod"],
		"incident_id": values["incident_id"],
	}
	annotations := map[string]string{
		"summary":       values["summary"],
		"description":   values["description"],
		"link":          values["link"],
		"current_value": values["current_value"],
		"starts_at":     values["starts_at"],
		"operator":      formatFeishuOperator(event.Operator.OpenID, event.Operator.UserID, event.Operator.UnionID),
	}
	return refactorMetricTrigger{
		Repo:        s.refactorRepo(firstNonEmpty(values["repo"], values["service"])),
		Service:     values["service"],
		Labels:      cleanStringMap(labels),
		Annotations: cleanStringMap(annotations),
	}
}

func (s *server) enqueueRefactorMetricFromGrafana(payload grafanaWebhook, ctxInfo cardContext) {
	if s.refactorOrchestrator == nil || !s.refactorAutoMetric || !isActiveAlertStatus(payload.Status) {
		return
	}
	// 自动重构入队只针对有代码仓库的服务告警。DataWorks 等非代码来源（source=dataworks）
	// 没有对应 repo，入队只会往 orchestrator 塞垃圾任务，直接跳过。
	if strings.EqualFold(strings.TrimSpace(payload.CommonLabels["source"]), "dataworks") {
		return
	}
	trigger := s.refactorMetricTriggerFromGrafana(payload, ctxInfo)
	trigger.AutoEnqueue = true
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, err := s.refactorOrchestrator.EnqueueMetric(ctx, trigger)
		if err != nil {
			log.Printf("refactor auto enqueue failed repo=%s service=%s alert=%s: %v",
				trigger.Repo, trigger.Service, trigger.Labels["alertname"], err)
			return
		}
		log.Printf("refactor auto enqueue ok repo=%s service=%s alert=%s item=%s duplicate=%v",
			trigger.Repo, trigger.Service, trigger.Labels["alertname"], result.ItemID, result.Duplicate)
	}()
}

func (s *server) refactorMetricTriggerFromGrafana(payload grafanaWebhook, ctxInfo cardContext) refactorMetricTrigger {
	labels := mergedAlertLabels(payload)
	annotations := make(map[string]string, len(payload.CommonAnnotations)+8)
	for k, v := range payload.CommonAnnotations {
		annotations[k] = v
	}
	if len(payload.Alerts) > 0 {
		alert := payload.Alerts[0]
		for k, v := range alert.Annotations {
			annotations[k] = v
		}
		if value := formatValues(alert.Values); value != "" {
			annotations["current_value"] = value
		}
		if !alert.StartsAt.IsZero() {
			annotations["starts_at"] = alert.StartsAt.Format(time.RFC3339)
		}
	}
	if link := grafanaLink(payload); link != "" {
		annotations["link"] = link
	}
	if ctxInfo.IncidentID > 0 {
		labels["incident_id"] = strconv.FormatInt(ctxInfo.IncidentID, 10)
	}
	return refactorMetricTrigger{
		Repo:        s.refactorRepo(firstNonEmpty(labels["repo"], labels["repository"], labels["project"], labels["service"])),
		Service:     labels["service"],
		Labels:      cleanStringMap(labels),
		Annotations: cleanStringMap(annotations),
	}
}

func (s *server) refactorRepo(candidate string) string {
	if repo := strings.TrimSpace(candidate); repo != "" {
		return repo
	}
	return strings.TrimSpace(s.refactorDefaultRepo)
}

func cleanStringMap(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k != "" && v != "" {
			out[k] = v
		}
	}
	return out
}

func (s *server) replyCopilotAnalysis(messageID string, req AnalysisRequest) {
	timeout := s.analysisTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	analyzer := s.analyzer
	if analyzer == nil {
		analyzer = RuleBasedAnalyzer{}
	}
	report, err := analyzer.Analyze(ctx, req)
	if err != nil {
		log.Printf("copilot analysis failed: %v", err)
		_ = s.replyFeishuMessage(ctx, messageID, "告警 Copilot 归因失败："+err.Error())
		return
	}

	// New path (Phase 7-D): email the full report to the operator and
	// post a slim summary card back to the thread. We only enter this
	// path when both SMTP and the operator's open_id are available.
	// On any failure we transparently degrade to the legacy in-thread
	// full card, so a misconfigured email path can never silently swallow
	// an attribution at incident time.
	if s.mailer != nil {
		if recipient, sent, err := s.deliverReportByEmail(ctx, req, report); err != nil {
			log.Printf("copilot email delivery failed (%v); falling back to in-thread card", err)
		} else {
			log.Printf("copilot email delivered to=%s sent=%v; posting summary card", recipient, sent)
			summary := buildCopilotSummaryCard(req, report, recipient, sent)
			if err := s.replyFeishuCard(ctx, messageID, summary); err != nil {
				log.Printf("reply summary card failed (%v); falling back to text", err)
			} else {
				return
			}
		}
	}

	// Legacy path: full report rendered as an interactive card.
	if s.feishuAppConfigured() {
		card := buildCopilotReportCard(req, report, report.FormatText())
		if err := s.replyFeishuCard(ctx, messageID, card); err != nil {
			log.Printf("reply copilot card failed, falling back to text: %v", err)
		} else {
			return
		}
	}

	reply := report.FormatText()
	if req.Operator != "" {
		reply = req.Operator + " 触发了 AI 归因。\n\n" + reply
	}
	if err := s.replyFeishuMessage(ctx, messageID, reply); err != nil {
		log.Printf("reply copilot analysis failed: %v", err)
	}
}

// deliverReportByEmail looks up the operator's mailbox via the Feishu
// contact API and ships the full HTML report. Returns the recipient
// address it actually used (for the summary card) and `sent=true` on
// successful delivery. Errors are wrapped so the caller can decide
// between fallback strategies.
func (s *server) deliverReportByEmail(ctx context.Context, req AnalysisRequest, report AnalysisReport) (recipient string, sent bool, err error) {
	to := s.resolveRecipients(ctx, req)
	if len(to) == 0 {
		return "", false, errors.New("no recipient available (operator open_id missing AND EMAIL_FALLBACK_TO empty)")
	}
	msg, err := renderReportEmail(req, report, to)
	if err != nil {
		return "", false, fmt.Errorf("render report email: %w", err)
	}
	if err := s.mailer.Send(ctx, msg); err != nil {
		return "", false, fmt.Errorf("smtp send: %w", err)
	}
	return strings.Join(to, ","), true, nil
}

// resolveRecipients returns the To list for the attribution email.
// Precedence, most-inclusive first:
//
//  1. Chat broadcast: when the click came from a Feishu interactive
//     card (we have the OpenChatID) and we have both a ChatMemberLister
//     and an EmailMap, enumerate the chat membership and translate
//     each open_id through the map. This is the production behaviour
//     for on-call rooms — "someone clicked → everyone on duty gets
//     mailed".
//  2. Operator-only: when (1) yields no recipients but we do have the
//     operator's open_id, ask the contact API for the operator's
//     configured email. Useful in 1:1 direct messages or when the
//     broadcast map is not yet populated for this chat.
//  3. Static fallback: EMAIL_FALLBACK_TO — so even a fully
//     misconfigured deployment still ships *somewhere*.
//
// We log (not return) lookup failures so one missing mapping doesn't
// block the rest of the room from getting the report. The operator is
// always folded into the recipient set if their open_id is in the map,
// even when they aren't listed by the chat member API (defensive
// against Feishu briefly returning a stale list).
func (s *server) resolveRecipients(ctx context.Context, req AnalysisRequest) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(addr string) {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			return
		}
		key := strings.ToLower(addr)
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		out = append(out, addr)
	}

	// (1) Broadcast the entire chat membership through the map.
	if s.chatMembers != nil && s.emailMap != nil && strings.TrimSpace(req.OpenChatID) != "" {
		members, err := s.chatMembers.ListChatMembers(ctx, req.OpenChatID)
		if err != nil {
			log.Printf("copilot chat members lookup failed chat=%s: %v", req.OpenChatID, err)
		} else {
			var unmapped []string
			for _, m := range members {
				if email := s.emailMap.Lookup(m.OpenID); email != "" {
					add(email)
				} else {
					// Collect, log once at end so on-call can see
					// "these people should be added to the map".
					unmapped = append(unmapped, m.Name+"("+m.OpenID+")")
				}
			}
			log.Printf("copilot broadcast chat=%s members=%d mapped=%d unmapped=%d",
				req.OpenChatID, len(members), len(out), len(unmapped))
			if len(unmapped) > 0 {
				log.Printf("copilot broadcast unmapped members: %s",
					strings.Join(unmapped, ", "))
			}
		}
	}

	// (2) Always try to include the operator explicitly, even in
	// broadcast mode — it's a cheap insurance policy against a stale
	// chat member list.
	if strings.TrimSpace(req.OperatorOpenID) != "" {
		if s.emailMap != nil {
			if email := s.emailMap.Lookup(req.OperatorOpenID); email != "" {
				add(email)
			}
		}
		if len(out) == 0 && s.emailLookup != nil {
			email, err := s.emailLookup.LookupUserEmail(ctx, req.OperatorOpenID)
			if err != nil {
				log.Printf("copilot email lookup failed for open_id=%s: %v", req.OperatorOpenID, err)
			} else if email != "" {
				add(email)
			}
		}
	}

	// (3) Last-resort fallback.
	if len(out) == 0 {
		for _, addr := range s.fallbackTo {
			add(addr)
		}
	}
	return out
}

func formatFeishuOperator(openID, userID, unionID string) string {
	if openID != "" {
		return fmt.Sprintf(`<at user_id="%s">处理人</at>`, openID)
	}
	if userID != "" {
		return fmt.Sprintf(`<at user_id="%s">处理人</at>`, userID)
	}
	return firstNonEmpty(unionID, "有人")
}

func formatAlertTime(t time.Time) string {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.Local
	}
	return t.In(loc).Format("2006-01-02 15:04:05")
}

func escapeLarkMarkdown(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.TrimSpace(value)
}

func (s *server) decodeFeishuEventBody(body []byte) ([]byte, error) {
	var encrypted struct {
		Encrypt string `json:"encrypt"`
	}
	if err := json.Unmarshal(body, &encrypted); err != nil {
		return nil, err
	}
	if encrypted.Encrypt == "" {
		return body, nil
	}
	if s.feishuEncryptKey == "" {
		return nil, errors.New("FEISHU_ENCRYPT_KEY is required for encrypted events")
	}
	return decryptFeishuEvent(encrypted.Encrypt, s.feishuEncryptKey)
}

func decryptFeishuEvent(encrypted, encryptKey string) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < aes.BlockSize*2 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, errors.New("invalid encrypted payload size")
	}

	key := sha256.Sum256([]byte(encryptKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}

	iv := ciphertext[:aes.BlockSize]
	data := append([]byte(nil), ciphertext[aes.BlockSize:]...)
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(data, data)
	return pkcs7Unpad(data, aes.BlockSize)
}

func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 || len(data)%blockSize != 0 {
		return nil, errors.New("invalid pkcs7 payload")
	}
	padding := int(data[len(data)-1])
	if padding == 0 || padding > blockSize || padding > len(data) {
		return nil, errors.New("invalid pkcs7 padding")
	}
	for _, value := range data[len(data)-padding:] {
		if int(value) != padding {
			return nil, errors.New("invalid pkcs7 padding")
		}
	}
	return data[:len(data)-padding], nil
}

func verifyFeishuSignature(r *http.Request, encryptKey string, body []byte) bool {
	timestamp := r.Header.Get("X-Lark-Request-Timestamp")
	nonce := r.Header.Get("X-Lark-Request-Nonce")
	signature := r.Header.Get("X-Lark-Signature")
	if timestamp == "" || nonce == "" || signature == "" {
		return false
	}
	sum := sha256.Sum256([]byte(timestamp + nonce + encryptKey + string(body)))
	expected := hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(expected), []byte(signature)) == 1
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func challengeFromRawEvent(raw json.RawMessage) string {
	var event struct {
		Challenge string `json:"challenge"`
	}
	_ = json.Unmarshal(raw, &event)
	return event.Challenge
}

func tokenFromRawEvent(raw json.RawMessage) string {
	var event struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(raw, &event)
	return event.Token
}

func typeFromRawEvent(raw json.RawMessage) string {
	var event struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &event)
	return event.Type
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func formatDurationCN(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Minute {
		return "不到 1 分钟"
	}
	days := int(d / (24 * time.Hour))
	d -= time.Duration(days) * 24 * time.Hour
	hours := int(d / time.Hour)
	d -= time.Duration(hours) * time.Hour
	minutes := int(d / time.Minute)
	parts := []string{}
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%d 天", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%d 小时", hours))
	}
	if days == 0 && minutes > 0 {
		parts = append(parts, fmt.Sprintf("%d 分钟", minutes))
	}
	if len(parts) == 0 {
		return "不到 1 分钟"
	}
	return strings.Join(parts, " ")
}

func formatValues(values map[string]any) string {
	if len(values) == 0 {
		return ""
	}
	parts := make([]string, 0, len(values))
	for key, value := range values {
		parts = append(parts, fmt.Sprintf("%s=%v", key, value))
	}
	return strings.Join(parts, ", ")
}

func analyzerFromEnv(timeout time.Duration) Analyzer {
	fallback := RuleBasedAnalyzer{}

	if url := strings.TrimSpace(os.Getenv("COPILOT_RUNNER_URL")); url != "" {
		agent := strings.TrimSpace(os.Getenv("COPILOT_RUNNER_AGENT"))
		if agent == "" {
			agent = "attribution-agent"
		}
		log.Printf("copilot analyzer: HTTP runner url=%s agent=%s", url, agent)
		return FallbackAnalyzer{
			Primary: HTTPRunnerAnalyzer{
				BaseURL:   url,
				AgentName: agent,
				Timeout:   timeout,
			},
			Fallback: fallback,
		}
	}

	command := strings.TrimSpace(os.Getenv("COPILOT_RUNNER_COMMAND"))
	if command == "" {
		return fallback
	}
	log.Printf("copilot analyzer: command runner")
	return FallbackAnalyzer{
		Primary: CommandAnalyzer{
			Command: command,
			Timeout: timeout,
		},
		Fallback: fallback,
	}
}

func durationFromEnv(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return parseDurationValue(name, value, fallback)
}
