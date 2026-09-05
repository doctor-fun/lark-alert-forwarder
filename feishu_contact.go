package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// FeishuUser is the subset of Feishu contact fields we care about when
// deciding where to deliver an attribution report. We return this struct
// from LookupUser rather than a single email string because the
// recipient resolution logic (guessing a corporate email from a name
// when the email field is empty) belongs to the business layer, not the
// API wrapper. Keeping the wrapper "dumb" also makes it easier to swap
// in batch lookups later without touching the guessing rules.
type FeishuUser struct {
	OpenID          string
	UserID          string
	Name            string // 中文名, e.g. "Alice"
	EnName          string // 英文名, often empty
	Nickname        string // 显示昵称, e.g. "Derek"
	Email           string // flat personal email filled in by the user
	EnterpriseEmail string // org-managed mailbox, empty unless Feishu Mail enabled
}

// UserLookup resolves a Feishu open_id to a contact record. Exists as
// an interface so the summary-card pipeline can mock the network call
// in tests; production uses (*server).LookupUser which talks to
// /open-apis/contact/v3/users/{open_id}.
//
// Errors are returned (rather than swallowed) so the email-delivery
// pipeline can decide whether to fall back to a default mailing list.
type UserLookup interface {
	LookupUser(ctx context.Context, openID string) (FeishuUser, error)
}

// EmailLookup is the narrower interface the delivery pipeline uses in
// the happy path when only an email is needed. We keep it around for
// unit-test ergonomics (tests can stub just an email string without
// constructing a full FeishuUser).
type EmailLookup interface {
	LookupUserEmail(ctx context.Context, openID string) (string, error)
}

// LookupUser fetches the full contact record for openID. Requires the
// app to have one of the following scopes for anything other than the
// base fields:
//   - contact:contact.base:readonly (mandatory, base fields like name)
//   - contact:user.email:readonly   (optional, exposes the email field)
//
// 99991661 / 99991672 are surfaced as errFeishuScopeMissing so the
// on-call user gets an actionable message instead of a generic code.
func (s *server) LookupUser(ctx context.Context, openID string) (FeishuUser, error) {
	openID = strings.TrimSpace(openID)
	if openID == "" {
		return FeishuUser{}, errors.New("feishu contact: open_id is required")
	}
	if !s.feishuAppConfigured() {
		return FeishuUser{}, errors.New("feishu contact: app credentials not configured")
	}
	token, err := s.tenantAccessToken(ctx)
	if err != nil {
		return FeishuUser{}, fmt.Errorf("feishu contact: tenant token: %w", err)
	}

	// Single-user GET. We could batch via /contact/v3/users/batch but in
	// practice each card click attribution call has exactly one operator,
	// and the per-user endpoint is cheaper to mock.
	path := "/open-apis/contact/v3/users/" + url.PathEscape(openID) +
		"?user_id_type=open_id"
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			User struct {
				OpenID          string `json:"open_id"`
				UserID          string `json:"user_id"`
				Name            string `json:"name"`
				EnName          string `json:"en_name"`
				Nickname        string `json:"nickname"`
				Email           string `json:"email"`
				EnterpriseEmail string `json:"enterprise_email"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := s.callFeishuAPI(ctx, "GET", path, token, nil, &resp); err != nil {
		return FeishuUser{}, fmt.Errorf("feishu contact: %w", err)
	}
	if resp.Code != 0 {
		// 99991661 / 99991672 both mean "scope missing" in different
		// API versions. Surface them as the same sentinel so the on-call
		// user understands they need to flip a switch in the developer
		// console rather than re-deploying the forwarder.
		if resp.Code == 99991661 || resp.Code == 99991672 {
			return FeishuUser{}, errFeishuScopeMissing
		}
		return FeishuUser{}, fmt.Errorf("feishu contact: code=%d msg=%s", resp.Code, resp.Msg)
	}
	u := resp.Data.User
	return FeishuUser{
		OpenID:          u.OpenID,
		UserID:          u.UserID,
		Name:            strings.TrimSpace(u.Name),
		EnName:          strings.TrimSpace(u.EnName),
		Nickname:        strings.TrimSpace(u.Nickname),
		Email:           strings.TrimSpace(u.Email),
		EnterpriseEmail: strings.TrimSpace(u.EnterpriseEmail),
	}, nil
}

// LookupUserEmail is a thin adapter retained for tests that stub just
// the email string. Prefers enterprise_email > email and does NOT guess
// from name — that decision lives in resolveRecipients so the guess
// rules can evolve without touching this helper.
func (s *server) LookupUserEmail(ctx context.Context, openID string) (string, error) {
	u, err := s.LookupUser(ctx, openID)
	if err != nil {
		return "", err
	}
	if u.EnterpriseEmail != "" {
		return u.EnterpriseEmail, nil
	}
	if u.Email != "" {
		return u.Email, nil
	}
	return "", fmt.Errorf("feishu contact: no email on user %s (name=%q, nickname=%q)",
		openID, u.Name, u.Nickname)
}

// ChatMember is one entry in the result of ListChatMembers. We only
// surface the handful of fields the email broadcast pipeline needs
// (OpenID + name) rather than exposing the raw API shape.
type ChatMember struct {
	OpenID string
	Name   string
}

// ChatMemberLister is the seam the email broadcast pipeline depends on.
// Production uses (*server).ListChatMembers; tests inject a stub.
type ChatMemberLister interface {
	ListChatMembers(ctx context.Context, chatID string) ([]ChatMember, error)
}

// ListChatMembers returns every non-bot member of `chatID`. Handles
// pagination (Feishu caps page_size at 100; on-call chats can grow
// past that once we onboard more services) by walking the page_token
// chain until has_more=false.
//
// Required scope: im:chat.members:read (免审核). The bot must also be
// a member of the chat — Feishu refuses to list membership for rooms
// the caller isn't in.
func (s *server) ListChatMembers(ctx context.Context, chatID string) ([]ChatMember, error) {
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return nil, errors.New("feishu contact: chat_id is required")
	}
	if !s.feishuAppConfigured() {
		return nil, errors.New("feishu contact: app credentials not configured")
	}
	token, err := s.tenantAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("feishu chat members: tenant token: %w", err)
	}

	// Safety cap: pathological chats shouldn't spin this loop forever.
	// 20 pages * 100 members = 2000 users, far more than any real alert
	// room. If we ever hit this, something is wrong with the API.
	const maxPages = 20

	var out []ChatMember
	pageToken := ""
	for i := 0; i < maxPages; i++ {
		path := "/open-apis/im/v1/chats/" + url.PathEscape(chatID) +
			"/members?member_id_type=open_id&page_size=100"
		if pageToken != "" {
			path += "&page_token=" + url.QueryEscape(pageToken)
		}
		var resp struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
			Data struct {
				Items []struct {
					MemberID string `json:"member_id"`
					Name     string `json:"name"`
				} `json:"items"`
				PageToken   string `json:"page_token"`
				HasMore     bool   `json:"has_more"`
				MemberTotal int    `json:"member_total"`
			} `json:"data"`
		}
		if err := s.callFeishuAPI(ctx, "GET", path, token, nil, &resp); err != nil {
			return nil, fmt.Errorf("feishu chat members: %w", err)
		}
		if resp.Code != 0 {
			if resp.Code == 99991661 || resp.Code == 99991672 {
				return nil, errFeishuScopeMissing
			}
			return nil, fmt.Errorf("feishu chat members: code=%d msg=%s", resp.Code, resp.Msg)
		}
		for _, item := range resp.Data.Items {
			if item.MemberID == "" {
				continue
			}
			out = append(out, ChatMember{
				OpenID: item.MemberID,
				Name:   strings.TrimSpace(item.Name),
			})
		}
		if !resp.Data.HasMore || resp.Data.PageToken == "" {
			return out, nil
		}
		pageToken = resp.Data.PageToken
	}
	return out, fmt.Errorf("feishu chat members: pagination exceeded %d pages "+
		"(likely API bug or runaway room)", maxPages)
}

// errFeishuScopeMissing is returned when the contact API rejects our call
// because the app doesn't have the email scope. Callers should match it
// with errors.Is and log an actionable message rather than a stack trace.
var errFeishuScopeMissing = errors.New(
	"feishu contact: app missing scope contact:user.email:readonly " +
		"(add it at https://open.feishu.cn/app and re-publish the app version)",
)
