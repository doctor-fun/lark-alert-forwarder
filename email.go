package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

// Mailer is the seam between the forwarder and an SMTP server. We define
// it as an interface (rather than reaching for net/smtp directly) for two
// concrete reasons:
//
//  1. Tests inject a `mailerSpy` to capture the rendered HTML body and
//     verify that operator-specific fields (e.g. each fact citation) made
//     it through, without standing up an MTA.
//  2. Production deployments may swap to Aliyun DirectMail or any other
//     transactional provider just by writing a new Mailer implementation;
//     the Send call site never changes.
type Mailer interface {
	// Send delivers `msg` to the listed To/Cc addresses. Implementations
	// MUST honour ctx (cancel on deadline) and MUST return a non-nil error
	// when the underlying transport fails — silent failures are
	// catastrophic for an alert path.
	Send(ctx context.Context, msg EmailMessage) error
}

// EmailMessage is the transport-agnostic envelope. Body and BodyHTML may
// both be set; Mailer implementations MAY choose to send a multipart
// message or just the HTML variant. We keep the plain-text body as a
// fallback so screen readers / threat scanners still see something useful.
type EmailMessage struct {
	From     string
	FromName string
	To       []string
	Cc       []string
	Subject  string
	BodyText string
	BodyHTML string
}

// SMTPConfig is the minimum needed to talk to a typical enterprise mailbox
// (Tencent EXMail, Alibaba EXMail, generic Postfix). We deliberately do
// NOT support more advanced auth (XOAUTH2, IMAP discovery, etc.) until a
// real deployment requires it; sticking to plain LOGIN keeps the on-call
// "why isn't email landing" debug surface small.
type SMTPConfig struct {
	// Host is the SMTP server hostname (e.g. smtp.exmail.qq.com).
	Host string
	// Port is the SMTP server port; 465 for SMTPS, 587 for STARTTLS.
	Port int
	// Username is the full mailbox address used for authentication.
	Username string
	// Password is the SMTP password (NOT the mail-account password for
	// providers that issue separate "client" passwords).
	Password string
	// FromAddress is the envelope sender; defaults to Username when blank.
	FromAddress string
	// FromName is the human-friendly display name (e.g. "Alert Copilot").
	FromName string
	// UseTLS forces SMTPS (full TLS handshake on connect, port 465 style).
	// When false we use STARTTLS, which works on port 587. We do NOT
	// support cleartext SMTP — refusing it at the config layer keeps
	// secrets off the wire even if someone misconfigures Port 25.
	UseTLS bool
	// Timeout caps the entire send (connect + AUTH + DATA). Defaults to
	// 15 s if zero.
	Timeout time.Duration
}

// SMTPMailer is the production Mailer that talks to a remote MTA. It is
// safe to share across goroutines — net/smtp creates a new connection
// per Send call.
type SMTPMailer struct {
	cfg SMTPConfig
}

// NewSMTPMailer returns a configured SMTPMailer or an error when the
// config is obviously incomplete. We validate at construction time so
// startup logs make the missing field obvious instead of failing only on
// the first alert.
func NewSMTPMailer(cfg SMTPConfig) (*SMTPMailer, error) {
	if strings.TrimSpace(cfg.Host) == "" {
		return nil, errors.New("smtp: host is required")
	}
	if cfg.Port <= 0 {
		return nil, errors.New("smtp: port is required")
	}
	if strings.TrimSpace(cfg.Username) == "" || strings.TrimSpace(cfg.Password) == "" {
		return nil, errors.New("smtp: username and password are required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if strings.TrimSpace(cfg.FromAddress) == "" {
		cfg.FromAddress = cfg.Username
	}
	return &SMTPMailer{cfg: cfg}, nil
}

// Send renders msg into RFC 5322 wire format and dispatches via SMTP.
//
// We intentionally avoid bigger libraries (net/mail, go-mail/mail) because
// the alert path needs zero dependencies it could trip over at incident
// time; a misencoded Subject header is recoverable, an unreachable mail
// library is not.
func (m *SMTPMailer) Send(ctx context.Context, msg EmailMessage) error {
	if len(msg.To) == 0 {
		return errors.New("smtp: at least one To address is required")
	}
	from := strings.TrimSpace(msg.From)
	if from == "" {
		from = m.cfg.FromAddress
	}
	fromName := strings.TrimSpace(msg.FromName)
	if fromName == "" {
		fromName = m.cfg.FromName
	}

	wire, err := renderRFC5322(msg, from, fromName)
	if err != nil {
		return fmt.Errorf("render email: %w", err)
	}

	addr := fmt.Sprintf("%s:%d", m.cfg.Host, m.cfg.Port)
	auth := smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)

	// Run the actual SMTP exchange on a goroutine and wait on ctx so an
	// incident-time deadline doesn't get stuck behind a slow MX server.
	done := make(chan error, 1)
	go func() {
		done <- sendSMTP(addr, m.cfg.Host, m.cfg.UseTLS, auth, from,
			append(msg.To, msg.Cc...), wire)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sendSMTP is broken out from Send so unit tests of renderRFC5322 don't
// have to import net/smtp at all. Production-only path.
func sendSMTP(addr, host string, useTLS bool, auth smtp.Auth, from string, recipients []string, wire []byte) error {
	if useTLS {
		// SMTPS: dial TLS first, then run the SMTP handshake on the
		// already-encrypted connection. This is the classic "port 465"
		// behaviour, still the default on Tencent / Aliyun EXMail.
		tlsConfig := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
		conn, err := tls.Dial("tcp", addr, tlsConfig)
		if err != nil {
			return fmt.Errorf("smtps dial: %w", err)
		}
		defer conn.Close()
		c, err := smtp.NewClient(conn, host)
		if err != nil {
			return fmt.Errorf("smtps new client: %w", err)
		}
		defer c.Quit()
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("smtps auth: %w", err)
		}
		if err := c.Mail(from); err != nil {
			return fmt.Errorf("smtps mail from: %w", err)
		}
		for _, rcpt := range recipients {
			if err := c.Rcpt(rcpt); err != nil {
				return fmt.Errorf("smtps rcpt %s: %w", rcpt, err)
			}
		}
		w, err := c.Data()
		if err != nil {
			return fmt.Errorf("smtps data: %w", err)
		}
		if _, err := w.Write(wire); err != nil {
			return fmt.Errorf("smtps write: %w", err)
		}
		return w.Close()
	}
	// STARTTLS path uses the stdlib helper which also handles AUTH.
	return smtp.SendMail(addr, auth, from, recipients, wire)
}

// renderRFC5322 produces a multipart/alternative MIME body so picky mail
// clients (Outlook, gmail's filter) accept the message and so users on
// minimal clients still see plain text. We hand-roll the headers because
// net/mail.Message is encode-only via the standard library's tree of
// helpers we'd otherwise need to vendor.
func renderRFC5322(msg EmailMessage, from, fromName string) ([]byte, error) {
	var buf bytes.Buffer

	if fromName != "" {
		fmt.Fprintf(&buf, "From: %s <%s>\r\n", encodeHeaderUTF8(fromName), from)
	} else {
		fmt.Fprintf(&buf, "From: %s\r\n", from)
	}
	fmt.Fprintf(&buf, "To: %s\r\n", strings.Join(msg.To, ", "))
	if len(msg.Cc) > 0 {
		fmt.Fprintf(&buf, "Cc: %s\r\n", strings.Join(msg.Cc, ", "))
	}
	fmt.Fprintf(&buf, "Subject: %s\r\n", encodeHeaderUTF8(msg.Subject))
	fmt.Fprintf(&buf, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	buf.WriteString("MIME-Version: 1.0\r\n")

	hasHTML := strings.TrimSpace(msg.BodyHTML) != ""
	hasText := strings.TrimSpace(msg.BodyText) != ""
	if hasHTML && hasText {
		boundary := "----=_AlertCopilot_" + fmt.Sprintf("%d", time.Now().UnixNano())
		fmt.Fprintf(&buf, "Content-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", boundary)
		fmt.Fprintf(&buf, "--%s\r\n", boundary)
		writeMIMEBase64Part(&buf, "text/plain; charset=\"utf-8\"", msg.BodyText)
		buf.WriteString("\r\n")
		fmt.Fprintf(&buf, "--%s\r\n", boundary)
		writeMIMEBase64Part(&buf, "text/html; charset=\"utf-8\"", msg.BodyHTML)
		fmt.Fprintf(&buf, "\r\n--%s--\r\n", boundary)
		return buf.Bytes(), nil
	}
	if hasHTML {
		writeMIMEBase64Part(&buf, "text/html; charset=\"utf-8\"", msg.BodyHTML)
		buf.WriteString("\r\n")
		return buf.Bytes(), nil
	}
	// Pure text fallback — used when html template fails to render.
	writeMIMEBase64Part(&buf, "text/plain; charset=\"utf-8\"", msg.BodyText)
	buf.WriteString("\r\n")
	return buf.Bytes(), nil
}

// writeMIMEBase64Part writes a MIME part with base64 CTE and 76-octet line
// folding. QQ / enterprise SMTP rejects 8bit bodies whose HTML lines exceed
// RFC 5321's 998-octet limit (common with inline-CSS email templates).
func writeMIMEBase64Part(buf *bytes.Buffer, contentType, body string) {
	fmt.Fprintf(buf, "Content-Type: %s\r\n", contentType)
	buf.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	enc := base64.StdEncoding.EncodeToString([]byte(body))
	const fold = 76
	for len(enc) > fold {
		buf.WriteString(enc[:fold])
		buf.WriteString("\r\n")
		enc = enc[fold:]
	}
	buf.WriteString(enc)
	buf.WriteString("\r\n")
}

// encodeHeaderUTF8 wraps non-ASCII header values in MIME encoded-word
// (RFC 2047). A bare CJK Subject otherwise breaks Outlook completely.
func encodeHeaderUTF8(v string) string {
	if isASCII(v) {
		return v
	}
	// =?utf-8?B?<base64>?= per RFC 2047. We avoid net/mail's
	// mime.QEncoding here because B-encoding is more compact for CJK.
	return mimeBEncode(v)
}

func isASCII(s string) bool {
	for _, r := range s {
		if r > 0x7f {
			return false
		}
	}
	return true
}

// mimeBEncode is a tiny base64 MIME encoded-word helper. We avoid the
// stdlib's mime.BEncoding.Encode because it is in `mime` not `net/mail`
// and pulling it for a single header pollutes imports. Tests cover the
// CJK path explicitly so a hand-rolled bug surfaces fast.
func mimeBEncode(v string) string {
	const tag = "=?utf-8?B?"
	const end = "?="
	enc := base64Encode([]byte(v))
	return tag + enc + end
}

// base64Encode is a thin wrapper so the usage site stays readable; we
// keep it as a function rather than calling base64.StdEncoding inline so
// future tweaks (e.g. line-folding for very long Subjects) live in one
// place.
func base64Encode(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

// reportEmailHTMLTemplate is the rendered HTML body. We intentionally
// inline all CSS (no <style> tag, no external assets) because corporate
// mail clients aggressively strip <style> blocks; inline styles always
// survive.
//
// Sections collapse gracefully when their slice is empty: the template
// uses {{if}} guards so a tiny rule-based report still looks tidy.
const reportEmailHTMLTemplate = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"/></head>
<body style="margin:0;padding:0;background:#f4f5f7;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,'Helvetica Neue',Arial,'PingFang SC','Microsoft YaHei',sans-serif;color:#1f2329;">
  <table width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#f4f5f7;padding:24px 0;">
    <tr><td align="center">
      <table width="640" cellpadding="0" cellspacing="0" border="0" style="background:#ffffff;border-radius:8px;overflow:hidden;box-shadow:0 1px 3px rgba(0,0,0,0.06);">
        <tr><td style="background:{{.HeaderColor}};color:#ffffff;padding:18px 24px;font-size:16px;font-weight:600;">
          🤖 {{.Title}}
        </td></tr>

        <tr><td style="padding:16px 24px 8px;font-size:13px;color:#646a73;">
          {{if .Operator}}<div style="margin-bottom:6px;">{{.Operator}} 触发了 AI 归因</div>{{end}}
          <div>
            {{if .Service}}<span style="margin-right:14px;"><b>服务</b> <code style="background:#f5f6f7;padding:2px 6px;border-radius:3px;">{{.Service}}</code></span>{{end}}
            {{if .Env}}<span style="margin-right:14px;"><b>环境</b> {{.Env}}</span>{{end}}
            {{if .Severity}}<span style="margin-right:14px;"><b>级别</b> {{.Severity}}</span>{{end}}
            {{if .AlertName}}<span><b>告警</b> {{.AlertName}}</span>{{end}}
          </div>
        </td></tr>

        {{if .Facts}}
        <tr><td style="padding:6px 24px;">
          <h3 style="margin:18px 0 8px;font-size:14px;color:#1f2329;border-left:3px solid #3370ff;padding-left:8px;">事实</h3>
          <ul style="margin:0;padding-left:20px;color:#1f2329;font-size:13px;line-height:1.7;">
            {{range .Facts}}<li>{{.}}</li>{{end}}
          </ul>
        </td></tr>
        {{end}}

        {{if .Judgement}}
        <tr><td style="padding:6px 24px;">
          <h3 style="margin:18px 0 8px;font-size:14px;color:#1f2329;border-left:3px solid #ff7d00;padding-left:8px;">判断</h3>
          <ul style="margin:0;padding-left:20px;color:#1f2329;font-size:13px;line-height:1.7;">
            {{range .Judgement}}<li>{{.}}</li>{{end}}
          </ul>
        </td></tr>
        {{end}}

        {{if .NextSteps}}
        <tr><td style="padding:6px 24px;">
          <h3 style="margin:18px 0 8px;font-size:14px;color:#1f2329;border-left:3px solid #00b96b;padding-left:8px;">建议下一步</h3>
          <ol style="margin:0;padding-left:22px;color:#1f2329;font-size:13px;line-height:1.7;">
            {{range .NextSteps}}<li style="margin-bottom:6px;">{{.}}</li>{{end}}
          </ol>
        </td></tr>
        {{end}}

        {{if .FlowSteps}}
        <tr><td style="padding:6px 24px;">
          <h3 style="margin:18px 0 6px;font-size:14px;color:#1f2329;border-left:3px solid #722ed1;padding-left:8px;">RCA 证据链（展开版）</h3>
          <div style="margin:0 0 10px;color:#646a73;font-size:12px;line-height:1.6;">按告警现象、已确认事实、待补证、当前判断和处理动作展开，避免只给一个结论。</div>
          <table width="100%" cellpadding="0" cellspacing="0" border="0" style="border-collapse:collapse;">
            {{range .FlowSteps}}
            <tr><td style="padding:0;">
              <table width="100%" cellpadding="0" cellspacing="0" border="0" style="border-collapse:collapse;background:{{.Background}};border:1px solid #e5e6eb;border-left:4px solid {{.Color}};border-radius:6px;">
                <tr><td style="padding:10px 12px;">
                  <div style="font-size:12px;color:#646a73;margin-bottom:4px;">{{.Label}}</div>
                  <div style="font-size:14px;font-weight:600;color:#1f2329;margin-bottom:6px;">{{.Title}}</div>
                  {{if .Items}}
                  <ul style="margin:0;padding-left:18px;color:#1f2329;font-size:13px;line-height:1.6;">
                    {{range .Items}}<li>{{.}}</li>{{end}}
                  </ul>
                  {{end}}
                </td></tr>
              </table>
            </td></tr>
            {{if .ShowArrow}}
            <tr><td align="center" style="padding:4px 0;color:#8f959e;font-size:16px;line-height:18px;">&darr;</td></tr>
            {{end}}
            {{end}}
          </table>
        </td></tr>
        {{end}}

        {{if .References}}
        <tr><td style="padding:6px 24px 18px;">
          <h3 style="margin:18px 0 8px;font-size:14px;color:#1f2329;border-left:3px solid #8f959e;padding-left:8px;">参考</h3>
          <ul style="margin:0;padding-left:20px;color:#646a73;font-size:12px;line-height:1.7;">
            {{range .References}}<li>{{.}}</li>{{end}}
          </ul>
        </td></tr>
        {{end}}

        {{if .Link}}
        <tr><td style="padding:6px 24px 18px;">
          <a href="{{.Link}}" style="color:#3370ff;text-decoration:none;font-size:13px;">📊 打开 Grafana 看板 →</a>
        </td></tr>
        {{end}}

        <tr><td style="padding:14px 24px;background:#fafbfc;color:#8f959e;font-size:12px;border-top:1px solid #eef0f3;">
          生成时间：{{.GeneratedAt}}<br/>
          报告由 matrix-alert-forwarder + matrix-agent-runner 自动生成。
        </td></tr>
      </table>
    </td></tr>
  </table>
</body></html>`

// reportEmailData is the typed payload the HTML template consumes. Using
// a typed struct (rather than map[string]any) lets the html/template
// package catch field typos at parse time instead of silently rendering
// nothing.
type reportEmailData struct {
	Title           string
	HeaderColor     string
	Operator        string
	Service         string
	Env             string
	Severity        string
	AlertName       string
	Facts           []string
	Judgement       []string
	NextSteps       []string
	References      []string
	DiagramPlantUML string
	FlowSteps       []reportEmailFlowStep
	Link            string
	GeneratedAt     string
}

type reportEmailFlowStep struct {
	Label      string
	Title      string
	Items      []string
	Color      string
	Background string
	ShowArrow  bool
}

// renderReportEmail turns an AnalysisRequest + AnalysisReport into a
// ready-to-send EmailMessage. Subject line is opinionated to be
// scannable in a busy inbox: "[Copilot][prod/P1] matrix-api 5xx detected
// — title summary".
func renderReportEmail(req AnalysisRequest, report AnalysisReport, to []string) (EmailMessage, error) {
	if len(to) == 0 {
		return EmailMessage{}, errors.New("renderReportEmail: at least one recipient required")
	}
	t, err := template.New("report").Parse(reportEmailHTMLTemplate)
	if err != nil {
		return EmailMessage{}, fmt.Errorf("parse template: %w", err)
	}
	data := reportEmailData{
		Title:           firstNonEmpty(report.Title, "告警 Copilot 归因结论"),
		HeaderColor:     severityHex(req.Severity),
		Operator:        req.Operator,
		Service:         req.Service,
		Env:             req.Env,
		Severity:        req.Severity,
		AlertName:       req.AlertName,
		Facts:           report.Facts,
		Judgement:       report.Judgement,
		NextSteps:       report.NextSteps,
		References:      report.References,
		DiagramPlantUML: strings.TrimSpace(report.DiagramPlantUML),
		FlowSteps:       buildReportEmailFlowSteps(req, report),
		Link:            req.Link,
		GeneratedAt:     formatAlertTime(reportGeneratedAt(report)),
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return EmailMessage{}, fmt.Errorf("execute template: %w", err)
	}
	subject := fmt.Sprintf("[Copilot][%s/%s] %s — %s",
		nonEmpty(req.Env, "prod"),
		nonEmpty(req.Severity, "P?"),
		nonEmpty(req.AlertName, "alert"),
		truncate(data.Title, 60))
	return EmailMessage{
		To:       to,
		Subject:  subject,
		BodyText: report.FormatText(),
		BodyHTML: buf.String(),
	}, nil
}

func buildReportEmailFlowSteps(req AnalysisRequest, report AnalysisReport) []reportEmailFlowStep {
	alertItems := compactEmailItems([]string{
		firstNonEmpty(req.Service, "-") + " / " + firstNonEmpty(req.Env, "-") + " / " + firstNonEmpty(req.Severity, "-"),
		req.Status,
		req.Summary,
		req.CurrentValue,
	})
	factItems := limitEmailItems(report.Facts, 8)
	judgementItems := limitEmailItems(report.Judgement, 5)
	nextStepItems := limitEmailItems(report.NextSteps, 6)
	gapItems := buildEvidenceGapItems(report)

	if len(alertItems) == 0 && len(factItems) == 0 && len(gapItems) == 0 && len(judgementItems) == 0 && len(nextStepItems) == 0 {
		return nil
	}

	steps := []reportEmailFlowStep{
		{
			Label:      "1. 告警现象",
			Title:      firstNonEmpty(req.AlertName, report.Title, "Grafana 告警"),
			Items:      alertItems,
			Color:      severityHex(req.Severity),
			Background: "#fff7e6",
		},
		{
			Label:      "2. 已确认事实",
			Title:      fmt.Sprintf("已提取 %d 条关键事实", len(factItems)),
			Items:      withEmailFallback(factItems, "暂无关键事实，需补充 SLS / 指标 / 发布记录"),
			Color:      "#3370ff",
			Background: "#f0f5ff",
		},
		{
			Label:      "3. 待补证 / 排除项",
			Title:      "还不能跳过的验证",
			Items:      withEmailFallback(gapItems, "未发现需要单独列出的证据缺口"),
			Color:      "#8f959e",
			Background: "#fafafa",
		},
		{
			Label:      "4. 当前判断",
			Title:      "归因结论与置信度",
			Items:      withEmailFallback(judgementItems, "暂无明确判断"),
			Color:      "#fa8c16",
			Background: "#fff7e6",
		},
		{
			Label:      "5. 建议动作",
			Title:      "下一步处理 / 修复 / 降噪",
			Items:      withEmailFallback(nextStepItems, "继续补证后再决策"),
			Color:      "#00b96b",
			Background: "#f6ffed",
		},
	}
	for i := range steps {
		steps[i].ShowArrow = i < len(steps)-1
	}
	return steps
}

func buildEvidenceGapItems(report AnalysisReport) []string {
	candidates := append([]string{}, report.Judgement...)
	candidates = append(candidates, report.NextSteps...)
	keywords := []string{"证据不足", "待", "仍需", "需要", "缺少", "未确认", "不能确认", "查 ", "确认", "验证"}
	out := make([]string, 0, 4)
	for _, item := range candidates {
		trimmed := strings.TrimSpace(item)
		if trimmed == "" {
			continue
		}
		for _, keyword := range keywords {
			if strings.Contains(trimmed, keyword) {
				out = append(out, truncate(trimmed, 180))
				break
			}
		}
		if len(out) >= 4 {
			return out
		}
	}
	return out
}

func limitEmailItems(items []string, max int) []string {
	out := make([]string, 0, max)
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		out = append(out, truncate(item, 220))
		if len(out) >= max {
			return out
		}
	}
	return out
}

func compactEmailItems(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item = strings.TrimSpace(item); item != "" && item != "-" {
			out = append(out, item)
		}
	}
	return out
}

func withEmailFallback(items []string, fallback string) []string {
	if len(items) > 0 {
		return items
	}
	return []string{fallback}
}

// severityHex returns the header bar colour, matching the Feishu card's
// red/orange/blue palette so the email and chat card feel consistent at
// a glance.
func severityHex(severity string) string {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical", "p0", "p1":
		return "#d83931" // red
	case "warning", "warn", "p2":
		return "#fa8c16" // orange
	case "info", "p3", "p4":
		return "#3370ff" // blue
	default:
		return "#8f959e" // grey
	}
}

func nonEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// reportGeneratedAt returns the report's GeneratedAt or now() so the
// email always shows a timestamp even when the analyzer forgot to set
// one.
func reportGeneratedAt(r AnalysisReport) time.Time {
	if !r.GeneratedAt.IsZero() {
		return r.GeneratedAt
	}
	return time.Now()
}

// smtpMailerFromEnv reads SMTP_* env vars and returns a configured
// mailer. Returns (nil, nil) when SMTP is intentionally disabled (no
// SMTP_HOST set) — that lets main() distinguish "not yet provisioned"
// from "misconfigured".
func smtpMailerFromEnv() (*SMTPMailer, error) {
	host := strings.TrimSpace(os.Getenv("SMTP_HOST"))
	if host == "" {
		return nil, nil
	}
	port, err := strconv.Atoi(strings.TrimSpace(os.Getenv("SMTP_PORT")))
	if err != nil || port <= 0 {
		return nil, fmt.Errorf("SMTP_PORT must be a positive integer, got %q", os.Getenv("SMTP_PORT"))
	}
	useTLS := !strings.EqualFold(strings.TrimSpace(os.Getenv("SMTP_USE_TLS")), "false")
	cfg := SMTPConfig{
		Host:        host,
		Port:        port,
		Username:    strings.TrimSpace(os.Getenv("SMTP_USERNAME")),
		Password:    os.Getenv("SMTP_PASSWORD"), // do NOT TrimSpace, password may include whitespace
		FromAddress: strings.TrimSpace(os.Getenv("SMTP_FROM_ADDRESS")),
		FromName:    nonEmpty(strings.TrimSpace(os.Getenv("SMTP_FROM_NAME")), "Alert Copilot"),
		UseTLS:      useTLS,
	}
	return NewSMTPMailer(cfg)
}

// splitEmailList parses "a@x.com,b@x.com, c@x.com" into a clean slice.
// Whitespace and empty entries are dropped; we do NOT validate the
// address format because RFC 5321 is unforgiving and the SMTP server
// will reject bogus addresses anyway with a clearer error.
func splitEmailList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}
