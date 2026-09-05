package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type voiceProbeDialer interface {
	IsEnabled() bool
	SingleCallByTts(ctx context.Context, phone string, ttsParam map[string]string) (string, error)
}

type voiceProbeTarget struct {
	OpenID  string `json:"open_id"`
	Name    string `json:"name"`
	Phone   string `json:"phone"`
	Service string `json:"service,omitempty"`
}

type voiceProbeConfig struct {
	TargetsPath string
	Format      string
	DryRun      bool
	ConfirmDial bool
	Limit       int
	OnlyOpenIDs map[string]struct{}
	Interval    time.Duration
	Timeout     time.Duration
	AlertName   string
	Severity    string
}

type voiceProbeResult struct {
	Type        string `json:"type"`
	Index       int    `json:"index"`
	OpenID      string `json:"open_id,omitempty"`
	Name        string `json:"name,omitempty"`
	Service     string `json:"service,omitempty"`
	MaskedPhone string `json:"masked_phone,omitempty"`
	Status      string `json:"status"`
	CallID      string `json:"call_id,omitempty"`
	Error       string `json:"error,omitempty"`
	DryRun      bool   `json:"dry_run"`
	At          string `json:"at"`
}

type voiceProbeSummary struct {
	Type          string `json:"type"`
	TotalInput    int    `json:"total_input"`
	Selected      int    `json:"selected"`
	Duplicates    int    `json:"duplicates"`
	Skipped       int    `json:"skipped"`
	Dialed        int    `json:"dialed"`
	Succeeded     int    `json:"succeeded"`
	Failed        int    `json:"failed"`
	InvalidPhones int    `json:"invalid_phones"`
	DryRun        bool   `json:"dry_run"`
	At            string `json:"at"`
}

func runVoiceProbeCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("voice-probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := voiceProbeConfig{}
	only := ""
	fs.StringVar(&cfg.TargetsPath, "targets", "-", "target CSV/JSONL path, or - for stdin")
	fs.StringVar(&cfg.Format, "format", "auto", "target format: auto, csv, jsonl")
	fs.BoolVar(&cfg.DryRun, "dry-run", true, "validate and print masked results without dialing")
	fs.BoolVar(&cfg.ConfirmDial, "confirm-dial", false, "required when --dry-run=false")
	fs.IntVar(&cfg.Limit, "limit", 0, "max selected targets to process; 0 means no limit")
	fs.StringVar(&only, "only-open-ids", "", "comma separated open_ids to include")
	fs.DurationVar(&cfg.Interval, "interval", 5*time.Second, "sleep between calls")
	fs.DurationVar(&cfg.Timeout, "timeout", 15*time.Second, "per-call timeout")
	fs.StringVar(&cfg.AlertName, "alertname", "电话告警连通性测试", "TTS alertname parameter")
	fs.StringVar(&cfg.Severity, "severity", "TEST", "TTS severity parameter")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg.OnlyOpenIDs = parseVoiceProbeOpenIDSet(only)
	if !cfg.DryRun && !cfg.ConfirmDial {
		_, _ = fmt.Fprintln(stderr, "voice-probe: refusing to dial without --confirm-dial")
		return 2
	}

	input, closeFn, err := openVoiceProbeInput(cfg.TargetsPath, stdin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "voice-probe: open targets: %v\n", err)
		return 1
	}
	defer closeFn()
	targets, err := parseVoiceProbeTargets(input, cfg.Format)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "voice-probe: parse targets: %v\n", err)
		return 1
	}
	var dialer voiceProbeDialer
	if client := NewAliyunVoiceClientFromEnv(); client != nil {
		dialer = client
	}
	if !cfg.DryRun && (dialer == nil || !dialer.IsEnabled()) {
		_, _ = fmt.Fprintln(stderr, "voice-probe: aliyun voice not configured")
		return 1
	}
	if err := runVoiceProbe(context.Background(), cfg, targets, dialer, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "voice-probe: %v\n", err)
		return 1
	}
	return 0
}

func openVoiceProbeInput(path string, stdin io.Reader) (io.Reader, func(), error) {
	if strings.TrimSpace(path) == "" || path == "-" {
		return stdin, func() {}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, func() {}, err
	}
	return f, func() { _ = f.Close() }, nil
}

func parseVoiceProbeOpenIDSet(raw string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out[v] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseVoiceProbeTargets(r io.Reader, format string) ([]voiceProbeTarget, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, errors.New("empty target list")
	}
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" || format == "auto" {
		if strings.HasPrefix(trimmed, "{") {
			format = "jsonl"
		} else {
			format = "csv"
		}
	}
	switch format {
	case "jsonl":
		return parseVoiceProbeJSONL(strings.NewReader(trimmed))
	case "csv":
		return parseVoiceProbeCSV(strings.NewReader(trimmed))
	default:
		return nil, fmt.Errorf("unsupported format %q", format)
	}
}

func parseVoiceProbeJSONL(r io.Reader) ([]voiceProbeTarget, error) {
	var out []voiceProbeTarget
	scanner := bufio.NewScanner(r)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var t voiceProbeTarget
		if err := json.Unmarshal([]byte(line), &t); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		out = append(out, cleanVoiceProbeTarget(t))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("empty jsonl target list")
	}
	return out, nil
}

func parseVoiceProbeCSV(r io.Reader) ([]voiceProbeTarget, error) {
	cr := csv.NewReader(r)
	cr.TrimLeadingSpace = true
	cr.FieldsPerRecord = -1
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New("empty csv target list")
	}
	start := 0
	col := map[string]int{"open_id": 0, "name": 1, "phone": 2, "service": 3}
	if looksLikeVoiceProbeHeader(rows[0]) {
		col = map[string]int{}
		for i, h := range rows[0] {
			col[normalizeVoiceProbeHeader(h)] = i
		}
		start = 1
	}
	if _, ok := col["phone"]; !ok {
		return nil, errors.New("csv missing phone column")
	}
	colIndex := func(key string) int {
		if idx, ok := col[key]; ok {
			return idx
		}
		return -1
	}
	var out []voiceProbeTarget
	for i := start; i < len(rows); i++ {
		row := rows[i]
		if len(row) == 0 || allVoiceProbeCellsBlank(row) {
			continue
		}
		t := voiceProbeTarget{
			OpenID:  csvCell(row, colIndex("open_id")),
			Name:    csvCell(row, colIndex("name")),
			Phone:   csvCell(row, colIndex("phone")),
			Service: csvCell(row, colIndex("service")),
		}
		out = append(out, cleanVoiceProbeTarget(t))
	}
	if len(out) == 0 {
		return nil, errors.New("empty csv target list")
	}
	return out, nil
}

func looksLikeVoiceProbeHeader(row []string) bool {
	for _, cell := range row {
		h := normalizeVoiceProbeHeader(cell)
		if h == "open_id" || h == "phone" || h == "name" || h == "service" {
			return true
		}
	}
	return false
}

func normalizeVoiceProbeHeader(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, " ", "_")
	switch s {
	case "openid", "openid_id", "open_id":
		return "open_id"
	case "姓名", "名称":
		return "name"
	case "手机号", "手机", "mobile":
		return "phone"
	case "服务":
		return "service"
	default:
		return s
	}
}

func csvCell(row []string, idx int) string {
	if idx < 0 || idx >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[idx])
}

func allVoiceProbeCellsBlank(row []string) bool {
	for _, c := range row {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}

func cleanVoiceProbeTarget(t voiceProbeTarget) voiceProbeTarget {
	t.OpenID = strings.TrimSpace(t.OpenID)
	t.Name = strings.TrimSpace(t.Name)
	t.Phone = strings.TrimSpace(t.Phone)
	t.Service = strings.TrimSpace(t.Service)
	return t
}

func runVoiceProbe(ctx context.Context, cfg voiceProbeConfig, targets []voiceProbeTarget, dialer voiceProbeDialer, w io.Writer) error {
	enc := json.NewEncoder(w)
	selected, duplicates, skipped := selectVoiceProbeTargets(targets, cfg)
	summary := voiceProbeSummary{
		Type:       "summary",
		TotalInput: len(targets),
		Selected:   len(selected),
		Duplicates: duplicates,
		Skipped:    skipped,
		DryRun:     cfg.DryRun,
		At:         time.Now().UTC().Format(time.RFC3339),
	}
	if cfg.Interval < 0 {
		cfg.Interval = 0
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	param := map[string]string{
		"alertname": truncateTTSParam(firstNonEmpty(cfg.AlertName, "电话告警连通性测试"), 32),
		"severity":  truncateTTSParam(firstNonEmpty(cfg.Severity, "TEST"), 8),
	}
	for i, target := range selected {
		result := voiceProbeResult{
			Type:        "result",
			Index:       i + 1,
			OpenID:      target.OpenID,
			Name:        target.Name,
			Service:     target.Service,
			MaskedPhone: maskPhone(normalizePhone(target.Phone)),
			DryRun:      cfg.DryRun,
			At:          time.Now().UTC().Format(time.RFC3339),
		}
		normalized := normalizePhone(target.Phone)
		if normalized == "" {
			result.Status = "missing_phone"
			summary.InvalidPhones++
			if err := enc.Encode(result); err != nil {
				return err
			}
			continue
		}
		if !validVoiceProbePhone(normalized) {
			result.Status = "invalid_phone"
			result.Error = "phone must be 8-20 digits after normalization"
			summary.InvalidPhones++
			if err := enc.Encode(result); err != nil {
				return err
			}
			continue
		}
		if cfg.DryRun {
			result.Status = "dry_run"
			if err := enc.Encode(result); err != nil {
				return err
			}
			continue
		}
		if dialer == nil || !dialer.IsEnabled() {
			return errors.New("voice dialer is not enabled")
		}
		callCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		callID, err := dialer.SingleCallByTts(callCtx, target.Phone, param)
		cancel()
		summary.Dialed++
		if err != nil {
			result.Status = "failed"
			result.Error = sanitizeVoiceProbeError(err)
			summary.Failed++
		} else {
			result.Status = "called"
			result.CallID = callID
			summary.Succeeded++
		}
		if err := enc.Encode(result); err != nil {
			return err
		}
		if i < len(selected)-1 && cfg.Interval > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(cfg.Interval):
			}
		}
	}
	summary.At = time.Now().UTC().Format(time.RFC3339)
	return enc.Encode(summary)
}

func selectVoiceProbeTargets(targets []voiceProbeTarget, cfg voiceProbeConfig) ([]voiceProbeTarget, int, int) {
	seen := map[string]struct{}{}
	var selected []voiceProbeTarget
	duplicates := 0
	skipped := 0
	for _, t := range targets {
		if len(cfg.OnlyOpenIDs) > 0 {
			if _, ok := cfg.OnlyOpenIDs[t.OpenID]; !ok {
				skipped++
				continue
			}
		}
		key := t.OpenID
		if key == "" {
			key = "phone:" + normalizePhone(t.Phone)
		}
		if key == "" || key == "phone:" {
			key = "row:" + strconv.Itoa(len(selected)+duplicates+skipped)
		}
		if _, ok := seen[key]; ok {
			duplicates++
			continue
		}
		seen[key] = struct{}{}
		selected = append(selected, t)
		if cfg.Limit > 0 && len(selected) >= cfg.Limit {
			skipped += len(targets) - (len(selected) + duplicates + skipped)
			break
		}
	}
	return selected, duplicates, skipped
}

func validVoiceProbePhone(normalized string) bool {
	if len(normalized) < 8 || len(normalized) > 20 {
		return false
	}
	for _, r := range normalized {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func sanitizeVoiceProbeError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	fields := strings.Fields(msg)
	sort.Slice(fields, func(i, j int) bool { return len(fields[i]) > len(fields[j]) })
	for _, f := range fields {
		trimmed := strings.Trim(f, `"'(),;:`)
		if validVoiceProbePhone(normalizePhone(trimmed)) {
			msg = strings.ReplaceAll(msg, trimmed, maskPhone(normalizePhone(trimmed)))
		}
	}
	return msg
}
