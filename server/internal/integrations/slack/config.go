// Package slack is the Slack integration for the channel-agnostic engine. It
// uses the bring-your-own-app (BYO) model (MUL-3666): each agent's Slack app is
// created and installed by the workspace admin, who pastes its bot token (xoxb-)
// and app-level token (xapp-) into Multica. Each channel_installation therefore
// carries its OWN app-level token and gets its OWN Socket Mode connection,
// supervised per-installation by the engine like Feishu (slack_channel.go) — so
// several agents can each have a distinct bot identity in one Slack workspace.
// Installations are keyed and routed by the real Slack app id
// (config->>'app_id' == the inbound event's api_app_id). The inbound translation
// (Events API payload -> channel.InboundMessage) lives in inbound.go; the
// outbound reply path (chat.postMessage with Markdown->mrkdwn + threading) lives
// in channel.go. The design references the proven Slack adapter in Nous
// Research's Hermes Agent.
package slack

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// installConfig is the JSON shape stored in channel_installation.config for a
// Slack installation. The cross-platform columns stay flat; everything
// Slack-specific lives in this opaque blob (the documented config boundary).
//
// app_id holds the REAL Slack app id (parsed from the xapp- token). It is the
// per-installation routing key: the generic GetChannelInstallationByAppID query
// (config->>'app_id') and the (channel_type, app_id) unique index map an inbound
// event's api_app_id to its installation, so several apps — several agents — in
// one Slack workspace stay distinct. team_id is kept for display only.
//
// bot_token_encrypted (xoxb-, outbound Web API: chat.postMessage) and
// app_token_encrypted (xapp-, this installation's own Socket Mode connection)
// are both stored as base64-encoded secretbox ciphertext, never plaintext
// (mirroring Feishu's app_secret_encrypted). Both are pasted by the admin at
// BYO install time.
//
// chat_gate and allowed_channel_ids are the per-installation access policy the
// room guard runs on (room_guard.go). They live here, on the installation,
// rather than in a process-wide environment variable: one Multica server hosts
// many workspaces and many bots, and "who may talk to Mika" is a property of
// Mika's installation, not of the host.
type installConfig struct {
	AppID             string   `json:"app_id"`
	TeamID            string   `json:"team_id,omitempty"`
	BotUserID         string   `json:"bot_user_id,omitempty"`
	BotTokenEncrypted string   `json:"bot_token_encrypted"`
	AppTokenEncrypted string   `json:"app_token_encrypted,omitempty"`
	ChatGate          string   `json:"chat_gate,omitempty"`
	RefusalText       string   `json:"refusal_text,omitempty"`
	AuthChannelID     string   `json:"auth_channel_id,omitempty"`
	AllowedChannelIDs []string `json:"allowed_channel_ids,omitempty"`
}

// ChatGate selects what an unbound Slack user gets when they message the bot.
type ChatGate string

const (
	// ChatGateOpen is the product default and today's behavior: an unbound
	// sender is offered a single-use link to bind their Slack account, which
	// only a workspace member can redeem. It is what makes a BYO install
	// self-onboarding — the installer of a pasted-token app has no other way to
	// create their first binding.
	ChatGateOpen ChatGate = "open"
	// ChatGateMembersOnly suppresses that prompt. An unbound sender gets the
	// terminal refusal and learns nothing: not the product name, not that an
	// allowlist exists, not that there is any way in. Choose it for an
	// installation whose bindings are already complete, because it removes the
	// only self-service path to creating another one.
	ChatGateMembersOnly ChatGate = "members_only"
)

// roomPolicy is the decoded access policy for one installation.
type roomPolicy struct {
	gate ChatGate
	// authChannelID names a PRIVATE Slack channel whose membership is the
	// authorization roster: to use the bot you must be in that channel. It
	// narrows the binding gate, never widens it — being in the channel does not
	// make you a workspace member, it only keeps a workspace member in scope.
	//
	// Empty means no channel gate, so binding plus workspace membership decides
	// alone. The channel must be private, and the way to guarantee that is to
	// give the app groups:read WITHOUT channels:read: conversations.members then
	// fails on any public channel and the guard denies rather than trusting a
	// room anybody could have walked into.
	authChannelID string
	// refusalText is what a person who may not use this bot is told. Empty
	// means the shipped default.
	//
	// It is configurable because the right answer is not universal. The default
	// says as little as possible, which is correct when the bot's existence is
	// itself sensitive. An operator who would rather their people understand
	// what happened — "you are not on the roster" rather than a shrug — can say
	// so, accepting that the wording admits a roster exists.
	refusalText string
	// allowedChannels holds the channel ids where the bot may do substantive
	// work. Empty — the default — means no channel qualifies, so the bot is
	// reachable in direct messages and authorized multi-party DMs only.
	allowedChannels map[string]struct{}
}

// decodeRoomPolicy reads the access policy out of a stored installation config.
// It fails CLOSED on every doubt: an undecodable blob yields the default policy
// (members-only channel access denied, prompt behavior open), never a
// permissive one.
func decodeRoomPolicy(raw json.RawMessage) roomPolicy {
	var cfg installConfig
	_ = json.Unmarshal(raw, &cfg)
	p := roomPolicy{gate: ChatGateOpen, allowedChannels: map[string]struct{}{}}
	if ChatGate(strings.TrimSpace(cfg.ChatGate)) == ChatGateMembersOnly {
		p.gate = ChatGateMembersOnly
	}
	p.authChannelID = strings.TrimSpace(cfg.AuthChannelID)
	p.refusalText = NormalizeRefusalText(cfg.RefusalText)
	for _, id := range cfg.AllowedChannelIDs {
		// Slack ids are case-sensitive, so the entry is compared verbatim.
		// Only surrounding whitespace and empty entries are forgiven.
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			p.allowedChannels[trimmed] = struct{}{}
		}
	}
	return p
}

// refusal returns what to tell somebody who may not use this bot.
func (p roomPolicy) refusal() string {
	if p.refusalText == "" {
		return RefusalText
	}
	return p.refusalText
}

// allowsChannel reports whether substantive work may happen in channelID.
func (p roomPolicy) allowsChannel(channelID string) bool {
	_, ok := p.allowedChannels[channelID]
	return ok
}

// credentials is the decoded, decrypted form the outbound sender runs on. The
// installation IDENTITY (workspace / agent / installer) is deliberately absent:
// it is resolved per message by the Router's InstallationResolver, exactly as
// the Feishu adapter does.
type credentials struct {
	TeamID    string
	BotUserID string
	BotToken  string
}

// Decrypter turns stored ciphertext into plaintext. The wiring injects a
// secretbox-backed implementation; tests inject an identity decrypter (or nil,
// which treats the stored bytes as plaintext).
type Decrypter func(ciphertext []byte) (plaintext []byte, err error)

// decodeCredentials parses the per-installation config blob and decrypts the
// stored tokens. It is the single place the Slack config JSON is interpreted.
func decodeCredentials(raw json.RawMessage, decrypt Decrypter) (credentials, error) {
	if len(raw) == 0 {
		return credentials{}, errors.New("slack: empty installation config")
	}
	var cfg installConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return credentials{}, fmt.Errorf("decode slack installation config: %w", err)
	}
	botToken, err := decryptToken(cfg.BotTokenEncrypted, decrypt)
	if err != nil {
		return credentials{}, fmt.Errorf("decrypt bot token: %w", err)
	}
	teamID := cfg.TeamID
	if teamID == "" {
		teamID = cfg.AppID
	}
	return credentials{
		TeamID:    teamID,
		BotUserID: cfg.BotUserID,
		BotToken:  botToken,
	}, nil
}

// PublicConfig is the non-secret subset of an installation config, safe to
// surface on the management API (the encrypted bot token is never included).
type PublicConfig struct {
	AppID     string
	TeamID    string
	BotUserID string
}

// DecodePublicConfig extracts the display-safe fields from a stored config blob.
// A decode miss yields a zero-value PublicConfig rather than an error: the
// management list should still render the row's identity columns.
func DecodePublicConfig(raw json.RawMessage) PublicConfig {
	var cfg installConfig
	_ = json.Unmarshal(raw, &cfg)
	teamID := cfg.TeamID
	if teamID == "" {
		teamID = cfg.AppID
	}
	return PublicConfig{AppID: cfg.AppID, TeamID: teamID, BotUserID: cfg.BotUserID}
}

// decryptToken base64-decodes the stored ciphertext (tolerating the MIME
// newline wrapping PostgreSQL's encode(...,'base64') emits) and runs it through
// the injected Decrypter. An empty stored value decodes to an empty token; a
// nil Decrypter treats the decoded bytes as plaintext (test convenience).
func decryptToken(enc string, decrypt Decrypter) (string, error) {
	if enc == "" {
		return "", nil
	}
	ciphertext, err := base64.StdEncoding.DecodeString(stripWhitespace(enc))
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	if decrypt == nil {
		return string(ciphertext), nil
	}
	plaintext, err := decrypt(ciphertext)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// stripWhitespace removes ASCII whitespace so a MIME-wrapped base64 string
// (newlines every 64 chars) and an unwrapped one decode identically.
func stripWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
