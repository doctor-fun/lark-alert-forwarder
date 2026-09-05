package main

import (
	"strings"
	"testing"
)

func TestBuildUserFeedbackOncallReplyListsQAAndBackendWithoutAt(t *testing.T) {
	reply := buildUserFeedbackOncallReply("收到新的用户反馈，请跟进", []userFeedbackOncallAssignee{
		{Role: "backend", OpenID: "ou_backend", Name: "Alice"},
		{Role: "qa", OpenID: "ou_qa", Name: "测试同学"},
	})
	for _, want := range []string{
		"收到新的用户反馈，请跟进",
		"值班同学：Alice、测试同学",
	} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply missing %q: %s", want, reply)
		}
	}
	if strings.Contains(reply, "<at ") || strings.Contains(reply, "ou_backend") || strings.Contains(reply, "ou_qa") {
		t.Fatalf("reply should not directly at users or expose open_id: %s", reply)
	}
}
