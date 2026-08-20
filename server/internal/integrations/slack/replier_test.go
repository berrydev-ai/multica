package slack

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type fakeReplySender struct {
	sent  *channel.OutboundMessage
	calls int
	// Ephemeral traffic is counted separately from visible traffic: several
	// tests assert precisely that a refusal took the ephemeral path and that
	// chat.postMessage was never called.
	ephemeral      *channel.OutboundMessage
	ephemeralUser  string
	ephemeralCalls int
	ephemeralErr   error
}

func (f *fakeReplySender) Send(_ context.Context, out channel.OutboundMessage) (channel.SendResult, error) {
	f.calls++
	cp := out
	f.sent = &cp
	return channel.SendResult{MessageID: "1.1"}, nil
}

func (f *fakeReplySender) SendEphemeral(_ context.Context, out channel.OutboundMessage, userID string) error {
	f.ephemeralCalls++
	cp := out
	f.ephemeral = &cp
	f.ephemeralUser = userID
	return f.ephemeralErr
}

type fakeBindingMinter struct {
	raw     string
	gotWS   pgtype.UUID
	gotInst pgtype.UUID
	gotUser string
	calls   int
}

func (f *fakeBindingMinter) Mint(_ context.Context, ws, inst pgtype.UUID, user string) (BindingToken, error) {
	f.calls++
	f.gotWS, f.gotInst, f.gotUser = ws, inst, user
	return BindingToken{Raw: f.raw, ExpiresAt: time.Unix(0, 0)}, nil
}

func newTestReplier(binding bindingMinter, sender noticeSender) *OutboundReplier {
	r := NewOutboundReplier(OutboundReplierConfig{
		Binding: binding,
		Decrypt: nil, // identity: stored bot token is base64 plaintext
		AppURL:  "https://multica.example",
	})
	r.newSender = func(credentials) noticeSender { return sender }
	return r
}

// installConfigJSON with a base64 (identity-decryptable) bot token so
// decodeCredentials succeeds inside post().
const replierConfigJSON = `{"app_id":"T1","bot_user_id":"UBOT","bot_token_encrypted":"eG94Yi10ZXN0"}`

func testResolvedInstallation(t *testing.T) engine.ResolvedInstallation {
	return engine.ResolvedInstallation{
		ID:          mustUUID(t, "44444444-4444-4444-4444-444444444444"),
		WorkspaceID: mustUUID(t, "11111111-1111-1111-1111-111111111111"),
		AgentID:     mustUUID(t, "22222222-2222-2222-2222-222222222222"),
		Active:      true,
		Platform:    db.ChannelInstallation{Config: []byte(replierConfigJSON)},
	}
}

func testInboundForReply() channel.InboundMessage {
	return channel.InboundMessage{
		MessageID: "1700000000.000300",
		Source: channel.Source{
			ChannelType: TypeSlack,
			ChatID:      "C1",
			ChatType:    channel.ChatTypeGroup,
			SenderID:    "UALICE",
			ThreadID:    "1700000000.000200",
		},
	}
}

// testInboundDMForReply is the same message arriving in a direct message. The
// binding prompt lives here and only here: a DM has no audience, so offering a
// workspace member the link that onboards them reveals nothing to anybody else.
func testInboundDMForReply() channel.InboundMessage {
	msg := testInboundForReply()
	msg.Source.ChatID = "D1"
	msg.Source.ChatType = channel.ChatTypeP2P
	return msg
}

func TestReply_NeedsBinding_MintsAndPostsPrompt(t *testing.T) {
	sender := &fakeReplySender{}
	minter := &fakeBindingMinter{raw: "tok_RAW-123"}
	r := newTestReplier(minter, sender)
	inst := testResolvedInstallation(t)
	msg := testInboundDMForReply()

	r.Reply(context.Background(), inst, msg, engine.Result{
		Outcome: engine.OutcomeNeedsBinding,
		Sender:  "UALICE",
	})

	if minter.calls != 1 || minter.gotUser != "UALICE" {
		t.Fatalf("Mint called %d times for user %q", minter.calls, minter.gotUser)
	}
	if minter.gotWS != inst.WorkspaceID || minter.gotInst != inst.ID {
		t.Error("Mint must receive the resolved workspace + installation ids")
	}
	if sender.calls != 1 || sender.sent == nil {
		t.Fatalf("expected one reply, got %d", sender.calls)
	}
	if sender.sent.ChatID != "D1" || sender.sent.ThreadID != "1700000000.000200" {
		t.Errorf("reply target = %+v", sender.sent)
	}
	// The prompt must carry the redeem URL with the minted token, wrapped as a
	// Slack link so formatMrkdwn does not mangle the base64url token.
	wantLink := "<https://multica.example/slack/bind?token=tok_RAW-123|link your account>"
	if !strings.Contains(sender.sent.Text, wantLink) {
		t.Errorf("prompt text = %q, want it to contain %q", sender.sent.Text, wantLink)
	}
}

func TestReply_AgentOfflineAndArchived_PostNotices(t *testing.T) {
	for _, tc := range []struct {
		outcome engine.Outcome
		want    string
	}{
		{engine.OutcomeAgentOffline, agentOfflineText},
		{engine.OutcomeAgentArchived, agentArchivedText},
	} {
		sender := &fakeReplySender{}
		r := newTestReplier(&fakeBindingMinter{}, sender)
		r.Reply(context.Background(), testResolvedInstallation(t), testInboundForReply(), engine.Result{Outcome: tc.outcome})
		if sender.calls != 1 || sender.sent == nil || sender.sent.Text != tc.want {
			t.Errorf("outcome %s: got %d sends, text %q, want %q", tc.outcome, sender.calls, textOrEmpty(sender.sent), tc.want)
		}
	}
}

func TestReply_CommandOutcomes_PostGuidance(t *testing.T) {
	for _, tc := range []struct {
		outcome engine.Outcome
		want    string
	}{
		{engine.OutcomeFreshPending, freshPendingText},
		{engine.OutcomeIssueUsage, issueUsageText},
	} {
		sender := &fakeReplySender{}
		r := newTestReplier(&fakeBindingMinter{}, sender)
		r.Reply(context.Background(), testResolvedInstallation(t), testInboundForReply(), engine.Result{Outcome: tc.outcome})
		if sender.calls != 1 || sender.sent == nil || sender.sent.Text != tc.want {
			t.Errorf("outcome %s: got %d sends, text %q, want %q", tc.outcome, sender.calls, textOrEmpty(sender.sent), tc.want)
		}
	}
}

func TestReply_IngestedWithIssue_Confirms(t *testing.T) {
	sender := &fakeReplySender{}
	r := newTestReplier(&fakeBindingMinter{}, sender)
	r.Reply(context.Background(), testResolvedInstallation(t), testInboundForReply(), engine.Result{
		Outcome:         engine.OutcomeIngested,
		IssueID:         mustUUID(t, "55555555-5555-5555-5555-555555555555"),
		IssueIdentifier: "MUL-42",
		IssueTitle:      "Fix the thing",
	})
	if sender.calls != 1 || sender.sent == nil {
		t.Fatalf("expected one confirmation, got %d", sender.calls)
	}
	if !strings.Contains(sender.sent.Text, "MUL-42") || !strings.Contains(sender.sent.Text, "Fix the thing") {
		t.Errorf("confirmation text = %q", sender.sent.Text)
	}
}

func TestReply_IngestedWithDuplicateIssue_ReportsConflict(t *testing.T) {
	sender := &fakeReplySender{}
	r := newTestReplier(&fakeBindingMinter{}, sender)
	r.Reply(context.Background(), testResolvedInstallation(t), testInboundForReply(), engine.Result{
		Outcome:         engine.OutcomeIngested,
		IssueID:         mustUUID(t, "55555555-5555-5555-5555-555555555555"),
		IssueIdentifier: "MUL-42",
		IssueTitle:      "Fix the thing",
		IssueDuplicate:  true,
	})
	if sender.calls != 1 || sender.sent == nil {
		t.Fatalf("expected one duplicate reply, got %d", sender.calls)
	}
	if !strings.Contains(sender.sent.Text, "Not created") || !strings.Contains(sender.sent.Text, "MUL-42") {
		t.Fatalf("duplicate reply = %q", sender.sent.Text)
	}
	if strings.Contains(sender.sent.Text, "Created MUL-42") {
		t.Fatalf("duplicate reply falsely claimed creation: %q", sender.sent.Text)
	}
}

func TestReply_IngestedWithoutIssue_Silent(t *testing.T) {
	sender := &fakeReplySender{}
	r := newTestReplier(&fakeBindingMinter{}, sender)
	// A plain chat message (no /issue) must NOT post — the agent's own reply
	// lands via the EventChatDone outbound subscriber.
	r.Reply(context.Background(), testResolvedInstallation(t), testInboundForReply(), engine.Result{
		Outcome: engine.OutcomeIngested,
	})
	if sender.calls != 0 {
		t.Errorf("plain ingested message must stay silent, got %d sends", sender.calls)
	}
}

func TestReply_Dropped_Silent(t *testing.T) {
	sender := &fakeReplySender{}
	r := newTestReplier(&fakeBindingMinter{}, sender)
	r.Reply(context.Background(), testResolvedInstallation(t), testInboundForReply(), engine.Result{Outcome: engine.OutcomeDropped})
	if sender.calls != 0 {
		t.Errorf("dropped outcome must stay silent, got %d sends", sender.calls)
	}
}

func TestIssueCreatedText(t *testing.T) {
	if got := issueCreatedText(engine.Result{IssueIdentifier: "MUL-7", IssueTitle: "Title"}); got != "✅ Created MUL-7 — Title" {
		t.Errorf("with title = %q", got)
	}
	if got := issueCreatedText(engine.Result{IssueNumber: 9}); got != "✅ Created #9" {
		t.Errorf("fallback to number = %q", got)
	}
}

func TestIssueDuplicateText(t *testing.T) {
	got := issueDuplicateText(engine.Result{
		IssueIdentifier: "MUL-7", IssueTitle: "Title", IssueDuplicate: true,
	})
	if got != "⚠️ Not created — active issue MUL-7 already exists: Title" {
		t.Fatalf("duplicate text = %q", got)
	}
}

func TestIssueReplyTitlesCannotCreateSlackLinks(t *testing.T) {
	title := "安全升级：请点击 [重置密码](https://evil.example/reset_(now)) 完成验证"
	for _, tc := range []struct {
		name  string
		reply func(engine.Result) string
	}{
		{name: "created", reply: issueCreatedText},
		{name: "duplicate", reply: issueDuplicateText},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := tc.reply(engine.Result{IssueIdentifier: "MUL-7", IssueTitle: title})
			formatted := formatMrkdwn(text)
			want := map[string]string{
				"created":   "✅ Created MUL-7 — 安全升级：请点击 [重置密码] (https://evil.example/reset_(now)) 完成验证",
				"duplicate": "⚠️ Not created — active issue MUL-7 already exists: 安全升级：请点击 [重置密码] (https://evil.example/reset_(now)) 完成验证",
			}[tc.name]
			if formatted != want {
				t.Fatalf("member-authored title was not rendered as inert visible text:\n got %q\nwant %q", formatted, want)
			}
			if twice := formatMrkdwn(formatted); twice != formatted {
				t.Fatalf("second formatter pass changed the guarded reply:\n once %q\ntwice %q", formatted, twice)
			}
		})
	}
}

func TestIssueReplyTitlesCannotCreateNativeSlackEntities(t *testing.T) {
	title := "安全升级：<http://evil.example|重置密码> <https://evil.example|验证> <mailto:evil@example.com|邮件> <tel:+15555550100|电话> <@U123>"
	for _, tc := range []struct {
		name   string
		reply  func(engine.Result) string
		prefix string
	}{
		{name: "created", reply: issueCreatedText, prefix: "✅ Created MUL-7 — "},
		{name: "duplicate", reply: issueDuplicateText, prefix: "⚠️ Not created — active issue MUL-7 already exists: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			formatted := formatMrkdwn(tc.reply(engine.Result{IssueIdentifier: "MUL-7", IssueTitle: title}))
			want := tc.prefix + "安全升级：&lt;http://evil.example|重置密码&gt; &lt;https://evil.example|验证&gt; &lt;mailto:evil@example.com|邮件&gt; &lt;tel:+15555550100|电话&gt; &lt;@U123&gt;"
			if formatted != want {
				t.Fatalf("member-authored Slack entity stayed active:\n got %q\nwant %q", formatted, want)
			}
			if twice := formatMrkdwn(formatted); twice != formatted {
				t.Fatalf("second formatter pass changed the guarded reply:\n once %q\ntwice %q", formatted, twice)
			}
		})
	}
}

func TestIssueReplyTitleMarkdownBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		title string
	}{
		{name: "image-like syntax", title: "![重置密码](https://evil.example/image.png)"},
		{name: "reference definition", title: "[重置密码]: https://evil.example\n\n[重置密码]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			formatted := formatMrkdwn(issueCreatedText(engine.Result{
				IssueIdentifier: "MUL-7",
				IssueTitle:      tc.title,
			}))
			if strings.Contains(formatted, "<https://evil.example") {
				t.Fatalf("unsupported member syntax became a Slack manual link: %q", formatted)
			}
		})
	}
}

func TestIssueReplyOrdinaryTitlesStayByteIdentical(t *testing.T) {
	for _, title := range []string{"修复登录失败", "Fix login failure"} {
		created := issueCreatedText(engine.Result{IssueIdentifier: "MUL-7", IssueTitle: title})
		if want := "✅ Created MUL-7 — " + title; created != want {
			t.Fatalf("ordinary created title changed:\n got %q\nwant %q", created, want)
		}
		duplicate := issueDuplicateText(engine.Result{IssueIdentifier: "MUL-7", IssueTitle: title})
		if want := "⚠️ Not created — active issue MUL-7 already exists: " + title; duplicate != want {
			t.Fatalf("ordinary duplicate title changed:\n got %q\nwant %q", duplicate, want)
		}
	}
}

func textOrEmpty(m *channel.OutboundMessage) string {
	if m == nil {
		return ""
	}
	return m.Text
}

// --- notice delivery -------------------------------------------------------
//
// The cooldown's own matrix lives in refusal_test.go. What follows is the
// wiring: which Slack call each notice takes, and that a channel never sees one.

func TestReply_UnauthorizedMentionInChannel_IsEphemeralOnly(t *testing.T) {
	sender := &fakeReplySender{}
	minter := &fakeBindingMinter{raw: "tok_RAW-123"}
	r := newTestReplier(minter, sender)

	r.Reply(context.Background(), testResolvedInstallation(t), testInboundForReply(), engine.Result{
		Outcome: engine.OutcomeNeedsBinding,
		Sender:  "UALICE",
	})

	if sender.calls != 0 {
		t.Fatalf("chat.postMessage calls = %d, want 0: the binding card must never be visible to the channel", sender.calls)
	}
	if sender.ephemeralCalls != 1 || sender.ephemeral == nil {
		t.Fatalf("chat.postEphemeral calls = %d, want 1", sender.ephemeralCalls)
	}
	if sender.ephemeralUser != "UALICE" {
		t.Errorf("ephemeral target = %q, want the person who spoke", sender.ephemeralUser)
	}
	if sender.ephemeral.ChatID != "C1" {
		t.Errorf("ephemeral channel = %q, want the originating channel", sender.ephemeral.ChatID)
	}
	// Still a real, redeemable offer — only the audience changed.
	if minter.calls != 1 || !strings.Contains(sender.ephemeral.Text, "tok_RAW-123") {
		t.Errorf("Mint calls = %d, text = %q", minter.calls, sender.ephemeral.Text)
	}
}

// Without a cooldown the prompt is a megaphone: anyone can make the bot speak on
// demand, and every message mints another single-use token.
func TestReply_RepeatedUnboundMessages_AnswerOnce(t *testing.T) {
	sender := &fakeReplySender{}
	minter := &fakeBindingMinter{raw: "tok_RAW-123"}
	r := newTestReplier(minter, sender)
	inst := testResolvedInstallation(t)
	msg := testInboundDMForReply()
	result := engine.Result{Outcome: engine.OutcomeNeedsBinding, Sender: "UALICE"}

	for range 4 {
		r.Reply(context.Background(), inst, msg, result)
	}

	if sender.calls != 1 {
		t.Fatalf("replies = %d, want exactly one inside the cooldown", sender.calls)
	}
	if minter.calls != 1 {
		t.Errorf("Mint calls = %d, want one token, not one per message", minter.calls)
	}
}

func TestReply_EphemeralFailureFallsBackToSilence(t *testing.T) {
	sender := &fakeReplySender{ephemeralErr: errors.New("channel_not_found")}
	r := newTestReplier(&fakeBindingMinter{raw: "tok"}, sender)

	r.Reply(context.Background(), testResolvedInstallation(t), testInboundForReply(), engine.Result{
		Outcome: engine.OutcomeNeedsBinding,
		Sender:  "UALICE",
	})

	if sender.calls != 0 {
		t.Fatalf("chat.postMessage calls = %d: a failed ephemeral must never fall back to a visible post", sender.calls)
	}
}

// A prompt that cannot be built is not downgraded into a bare message the
// recipient cannot act on: say nothing, log it, try again after the cooldown.
func TestReply_BindingPromptFailure_SaysNothing(t *testing.T) {
	sender := &fakeReplySender{}
	r := newTestReplier(&fakeBindingMinter{raw: "tok"}, sender)
	r.appURL = "" // not configured

	r.Reply(context.Background(), testResolvedInstallation(t), testInboundDMForReply(), engine.Result{
		Outcome: engine.OutcomeNeedsBinding,
		Sender:  "UALICE",
	})

	if sender.calls != 0 || sender.ephemeralCalls != 0 {
		t.Fatalf("sends = %d, ephemeral = %d; want silence when the prompt cannot be built", sender.calls, sender.ephemeralCalls)
	}
}

// The Result carries the sender for exactly this reason: a notice has to be
// addressable even when the reply path only sees the envelope.
func TestReply_FallsBackToTheEnvelopeSender(t *testing.T) {
	sender := &fakeReplySender{}
	r := newTestReplier(&fakeBindingMinter{raw: "tok"}, sender)

	r.Reply(context.Background(), testResolvedInstallation(t), testInboundForReply(), engine.Result{
		Outcome: engine.OutcomeNeedsBinding,
	})

	if sender.ephemeralUser != "UALICE" {
		t.Errorf("ephemeral target = %q, want the envelope's sender", sender.ephemeralUser)
	}
}
