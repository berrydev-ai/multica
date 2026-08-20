package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/slack-go/slack"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// This file is the Slack OutboundReplier — the engine seam that delivers a
// verdict-driven reply back to the user (MUL-3666, completing the stage-3
// Replier=nil tail). It posts through the same bot-token Send path as the
// EventChatDone outbound subscriber, so it needs no new transport.
//
// Outcomes handled:
//   - NeedsBinding: the sender is unbound. Mint a single-use binding token and
//     reply with a "link your account" prompt pointing at the in-product redeem
//     page. After they bind, their next message reaches the agent.
//   - AgentOffline / AgentArchived: a status notice so the user is not left
//     wondering why nothing happened.
//   - Ingested with an /issue created: a confirmation of the new issue.

const (
	agentOfflineText  = "⚠️ The agent is offline right now. Your message was received and will be handled once it's back online."
	agentArchivedText = "⚠️ This agent has been archived and can't respond. Please contact your workspace admin."
	freshPendingText  = "✅ Fresh start ready. Your next chat message will run without previous context."
	issueUsageText    = "Please include an issue title. Use:\n\n`/issue <title>`\n`[description]` (optional)"
)

// noticeSender posts the replier's own notices. It is a superset of
// replySender: a refusal must be deliverable WITHOUT the rest of the room
// seeing it, which chat.postMessage cannot do. *slackSender satisfies it.
type noticeSender interface {
	replySender
	SendEphemeral(ctx context.Context, out channel.OutboundMessage, userID string) error
}

// bindingMinter is the binding-token surface the replier needs.
// *BindingTokenService satisfies it.
type bindingMinter interface {
	Mint(ctx context.Context, workspaceID, installationID pgtype.UUID, slackUserID string) (BindingToken, error)
}

// OutboundReplier implements engine.OutboundReplier for Slack.
type OutboundReplier struct {
	binding     bindingMinter
	decrypt     Decrypter
	newSender   func(creds credentials) noticeSender
	appURL      string
	bindingPath string
	logger      *slog.Logger
	refusals    *refusalLimiter
}

// OutboundReplierConfig configures the replier. Binding + AppURL are required
// for the NeedsBinding prompt to work; without them the prompt is skipped (the
// offline/archived/issue notices still fire).
type OutboundReplierConfig struct {
	Binding bindingMinter
	Decrypt Decrypter
	// AppURL is the Multica web app host the user clicks into to redeem the
	// binding token (e.g. https://multica.example). It comes from MULTICA_APP_URL
	// (falling back to FRONTEND_ORIGIN) and is intentionally separate from
	// MULTICA_PUBLIC_URL, which is the backend/API public URL used for webhook and
	// daemon-facing endpoints — the bind page (/slack/bind) is served by the web
	// app, so the link must point at the app host, not the API host. Mirrors the
	// Lark replier's AppURL.
	AppURL      string
	BindingPath string // default "/slack/bind"
	Logger      *slog.Logger
}

var _ engine.OutboundReplier = (*OutboundReplier)(nil)

// NewOutboundReplier builds the replier. The sender factory mirrors the outbound
// subscriber: only the bot token is needed to post.
func NewOutboundReplier(cfg OutboundReplierConfig) *OutboundReplier {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	bindingPath := cfg.BindingPath
	if bindingPath == "" {
		bindingPath = "/slack/bind"
	}
	if !strings.HasPrefix(bindingPath, "/") {
		bindingPath = "/" + bindingPath
	}
	r := &OutboundReplier{
		binding:     cfg.Binding,
		decrypt:     cfg.Decrypt,
		appURL:      strings.TrimRight(cfg.AppURL, "/"),
		bindingPath: bindingPath,
		logger:      logger,
		refusals:    newRefusalLimiter(time.Now),
	}
	r.newSender = func(c credentials) noticeSender {
		return newSlackSender(c, slack.New(c.BotToken), logger)
	}
	return r
}

// Reply routes each outcome to its user-visible message. Errors are logged, not
// propagated: the replier runs detached from the inbound ACK path.
func (r *OutboundReplier) Reply(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, res engine.Result) {
	switch res.Outcome {
	case engine.OutcomeNeedsBinding:
		r.replyUnauthorizedSender(ctx, inst, msg, res)
	case engine.OutcomeAgentOffline:
		if err := r.post(ctx, inst, msg, agentOfflineText); err != nil {
			r.logger.WarnContext(ctx, "slack replier: offline notice failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeAgentArchived:
		if err := r.post(ctx, inst, msg, agentArchivedText); err != nil {
			r.logger.WarnContext(ctx, "slack replier: archived notice failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeFreshPending:
		if err := r.post(ctx, inst, msg, freshPendingText); err != nil {
			r.logger.WarnContext(ctx, "slack replier: fresh-start confirmation failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeIssueUsage:
		if err := r.post(ctx, inst, msg, issueUsageText); err != nil {
			r.logger.WarnContext(ctx, "slack replier: issue usage reply failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeIngested:
		// Only an /issue product result warrants an immediate reply; a plain
		// chat message stays silent (the agent's own reply lands via ChatDone).
		if res.IssueID.Valid {
			text := issueCreatedText(res)
			if res.IssueDuplicate {
				text = issueDuplicateText(res)
			}
			if err := r.post(ctx, inst, msg, text); err != nil {
				r.logger.WarnContext(ctx, "slack replier: issue outcome reply failed",
					"installation_id", util.UUIDToString(inst.ID), "error", err)
			}
		}
	}
}

// replyUnauthorizedSender answers somebody the identity gate turned away, by
// offering them the link that binds their Slack account.
//
// Two things decide what happens. The cooldown comes first: a person who has
// already been answered gets nothing at all, so repeated messages cannot be
// used to make the bot talk on demand or to mint an unbounded number of binding
// tokens. Then the room. Anywhere other people can see, the offer goes out
// ephemerally — a "link your account" card posted into a channel announces that
// a bot is standing there and invites the rest of the room to ask about it, and
// the offer is no use to any of them anyway.
func (r *OutboundReplier) replyUnauthorizedSender(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, res engine.Result) {
	sender := res.Sender
	if sender == "" {
		sender = msg.Source.SenderID
	}
	if !r.refusals.allow(inst.ID, sender) {
		return
	}
	r.logRefusal(ctx, inst, msg, sender, "sender is not a bound workspace member")
	text, err := r.bindingPrompt(ctx, inst, sender)
	if err != nil {
		r.logger.WarnContext(ctx, "slack replier: binding prompt failed",
			"installation_id", util.UUIDToString(inst.ID), "error", err)
		return
	}
	r.deliver(ctx, inst, msg, sender, text)
}

// deliver sends one notice to the person it concerns: visibly in a direct
// message, where the only reader is that person, and ephemerally anywhere else.
func (r *OutboundReplier) deliver(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, userID, text string) {
	if msg.Source.ChatType != channel.ChatTypeP2P {
		r.postEphemeral(ctx, inst, msg, userID, text)
		return
	}
	if err := r.post(ctx, inst, msg, text); err != nil {
		r.logger.WarnContext(ctx, "slack replier: notice failed",
			"installation_id", util.UUIDToString(inst.ID), "error", err)
	}
}

// logRefusal records one denial: who, where, and what kind of conversation.
// Never the message body — the bot is refusing to process this person's words,
// and copying them into a log would be processing them.
func (r *OutboundReplier) logRefusal(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, sender, reason string) {
	r.logger.WarnContext(ctx, "slack replier: request refused",
		"installation_id", util.UUIDToString(inst.ID),
		"slack_user_id", sender,
		"slack_channel_id", msg.Source.ChatID,
		"chat_type", string(msg.Source.ChatType),
		"reason", reason,
	)
}

// postEphemeral delivers text to one person inside a conversation. A failure
// falls back to SILENCE, never to a visible post: a refusal the whole room can
// read is worse than no refusal at all.
func (r *OutboundReplier) postEphemeral(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, userID, text string) {
	row, ok := inst.Platform.(db.ChannelInstallation)
	if !ok {
		r.logger.WarnContext(ctx, "slack replier: ephemeral refusal skipped, installation row unavailable",
			"installation_id", util.UUIDToString(inst.ID))
		return
	}
	creds, err := decodeCredentials(row.Config, r.decrypt)
	if err != nil {
		r.logger.WarnContext(ctx, "slack replier: ephemeral refusal skipped, credentials unreadable",
			"installation_id", util.UUIDToString(inst.ID), "error", err)
		return
	}
	if err := r.newSender(creds).SendEphemeral(ctx, channel.OutboundMessage{
		ChatID:   msg.Source.ChatID,
		Text:     text,
		ThreadID: msg.Source.ThreadID,
	}, userID); err != nil {
		r.logger.WarnContext(ctx, "slack replier: ephemeral refusal failed",
			"installation_id", util.UUIDToString(inst.ID), "error", err)
	}
}

// bindingPrompt mints a single-use token and renders the "link your account"
// text. It only builds the message; the caller decides who gets to read it.
func (r *OutboundReplier) bindingPrompt(ctx context.Context, inst engine.ResolvedInstallation, sender string) (string, error) {
	if sender == "" {
		return "", errors.New("missing sender id")
	}
	if r.binding == nil {
		return "", errors.New("binding service not configured")
	}
	if r.appURL == "" {
		return "", errors.New("app url not configured")
	}
	token, err := r.binding.Mint(ctx, inst.WorkspaceID, inst.ID, sender)
	if err != nil {
		return "", fmt.Errorf("mint binding token: %w", err)
	}
	bindURL := r.appURL + r.bindingPath + "?token=" + url.QueryEscape(token.Raw)
	// Wrap the URL as an explicit Slack link <url|label>: formatMrkdwn protects
	// these from its markdown passes, so the base64url token's `_`/`-` chars are
	// not mangled into italics.
	return "👋 To start chatting with me, link your Slack account to Multica: <" +
		bindURL + "|link your account>\n(This link expires in 15 minutes.)", nil
}

// post resolves the installation's bot token from the carried platform row and
// sends text back into the originating channel / thread.
func (r *OutboundReplier) post(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, text string) error {
	row, ok := inst.Platform.(db.ChannelInstallation)
	if !ok {
		return errors.New("installation platform row unavailable")
	}
	creds, err := decodeCredentials(row.Config, r.decrypt)
	if err != nil {
		return fmt.Errorf("decode credentials: %w", err)
	}
	if _, err := r.newSender(creds).Send(ctx, channel.OutboundMessage{
		ChatID:   msg.Source.ChatID,
		Text:     text,
		ThreadID: msg.Source.ThreadID,
	}); err != nil {
		return fmt.Errorf("post slack reply: %w", err)
	}
	return nil
}

func issueCreatedText(res engine.Result) string {
	id := issueResultIdentifier(res)
	title := memberIssueTitle(strings.TrimSpace(res.IssueTitle))
	if title == "" {
		return "✅ Created " + id
	}
	return "✅ Created " + id + " — " + title
}

func issueDuplicateText(res engine.Result) string {
	id := issueResultIdentifier(res)
	title := memberIssueTitle(strings.TrimSpace(res.IssueTitle))
	if title == "" {
		return "⚠️ Not created — active issue " + id + " already exists."
	}
	return "⚠️ Not created — active issue " + id + " already exists: " + title
}

func memberIssueTitle(title string) string {
	title = channel.BreakMarkdownLinkAdjacency(title)
	// formatMrkdwn deliberately preserves existing Slack entities such as
	// <url|label> and <@user>. Encode their opening delimiter before that pass
	// so member-authored links and mentions are handled as visible text.
	return strings.ReplaceAll(title, "<", "&lt;")
}

func issueResultIdentifier(res engine.Result) string {
	if res.IssueIdentifier != "" {
		return res.IssueIdentifier
	}
	return fmt.Sprintf("#%d", res.IssueNumber)
}
