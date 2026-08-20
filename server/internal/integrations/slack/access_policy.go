package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/slack-go/slack"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// This file is the management surface for an installation's access policy — the
// chat gate, the authorization channel, and the channel allowlist that
// room_guard.go enforces. It exists so the policy is set through the API like
// every other setting, rather than by hand-editing a JSON column.
//
// Validation happens HERE, at configuration time, against live Slack. The guard
// re-checks at runtime because a channel can be converted or the bot removed
// later, but discovering "this channel cannot work" the moment an admin saves it
// is worth far more than discovering it when the bot silently stops answering.

// AccessPolicyInput is the requested policy. Every field is replaced wholesale:
// a PUT states the complete policy, so clearing the authorization channel is
// sending an empty string, not omitting the field.
type AccessPolicyInput struct {
	Gate             ChatGate
	RefusalText      string
	AuthChannelID    string
	AllowedChannelID []string
}

// AccessPolicy is the stored policy read back for the API response.
type AccessPolicy struct {
	Gate              ChatGate `json:"chat_gate"`
	RefusalText       string   `json:"refusal_text"`
	AuthChannelID     string   `json:"auth_channel_id"`
	AllowedChannelIDs []string `json:"allowed_channel_ids"`
}

var (
	// ErrInvalidChatGate: the requested gate is not one of the two values.
	ErrInvalidChatGate = errors.New("slack: chat_gate must be \"open\" or \"members_only\"")
	// ErrAuthChannelUnreadable: Slack refused to describe the channel. The
	// usual causes are a wrong id and a missing groups:read scope.
	ErrAuthChannelUnreadable = errors.New("slack: cannot read the authorization channel")
)

// DescribeAccessPolicy reads the stored policy off an installation row.
func DescribeAccessPolicy(config json.RawMessage) AccessPolicy {
	var cfg installConfig
	_ = json.Unmarshal(config, &cfg)
	policy := decodeRoomPolicy(config)
	allowed := make([]string, 0, len(policy.allowedChannels))
	for _, id := range cfg.AllowedChannelIDs {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			allowed = append(allowed, trimmed)
		}
	}
	return AccessPolicy{
		Gate:              policy.gate,
		RefusalText:       policy.refusalText,
		AuthChannelID:     policy.authChannelID,
		AllowedChannelIDs: allowed,
	}
}

// SetAccessPolicy validates the requested policy against Slack and persists it,
// preserving every other field of the installation's config blob (the encrypted
// tokens above all). It returns the stored policy.
func (s *InstallService) SetAccessPolicy(ctx context.Context, id, wsID pgtype.UUID, in AccessPolicyInput) (AccessPolicy, error) {
	gate := ChatGate(strings.TrimSpace(string(in.Gate)))
	if gate == "" {
		gate = ChatGateOpen
	}
	if gate != ChatGateOpen && gate != ChatGateMembersOnly {
		return AccessPolicy{}, ErrInvalidChatGate
	}

	// Workspace-scoped, so a guessed installation UUID cannot reach another
	// workspace's bot even if the route guard were ever loosened.
	row, err := s.q.GetChannelInstallationInWorkspace(ctx, db.GetChannelInstallationInWorkspaceParams{
		ID:          id,
		WorkspaceID: wsID,
		ChannelType: string(TypeSlack),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AccessPolicy{}, ErrInstallationNotFound
		}
		return AccessPolicy{}, fmt.Errorf("load installation: %w", err)
	}

	refusalText := NormalizeRefusalText(in.RefusalText)
	authChannelID := strings.TrimSpace(in.AuthChannelID)
	if authChannelID != "" {
		if err := s.verifyAuthChannel(ctx, row.Config, authChannelID); err != nil {
			return AccessPolicy{}, err
		}
	}

	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(row.Config, &cfg); err != nil {
		return AccessPolicy{}, fmt.Errorf("decode installation config: %w", err)
	}
	if cfg == nil {
		cfg = map[string]json.RawMessage{}
	}
	allowed := make([]string, 0, len(in.AllowedChannelID))
	for _, raw := range in.AllowedChannelID {
		if trimmed := strings.TrimSpace(raw); trimmed != "" {
			allowed = append(allowed, trimmed)
		}
	}
	if err := putJSONField(cfg, "chat_gate", string(gate)); err != nil {
		return AccessPolicy{}, err
	}
	if err := putJSONField(cfg, "refusal_text", refusalText); err != nil {
		return AccessPolicy{}, err
	}
	if err := putJSONField(cfg, "auth_channel_id", authChannelID); err != nil {
		return AccessPolicy{}, err
	}
	if err := putJSONField(cfg, "allowed_channel_ids", allowed); err != nil {
		return AccessPolicy{}, err
	}

	merged, err := json.Marshal(cfg)
	if err != nil {
		return AccessPolicy{}, fmt.Errorf("encode installation config: %w", err)
	}
	if err := s.q.SetChannelInstallationConfig(ctx, db.SetChannelInstallationConfigParams{
		ID:     id,
		Config: merged,
	}); err != nil {
		return AccessPolicy{}, fmt.Errorf("persist access policy: %w", err)
	}
	return AccessPolicy{
		Gate:              gate,
		RefusalText:       refusalText,
		AuthChannelID:     authChannelID,
		AllowedChannelIDs: allowed,
	}, nil
}

// putJSONField sets one field, dropping it entirely when the value is empty so
// a cleared policy leaves no misleading key behind.
func putJSONField(cfg map[string]json.RawMessage, key string, value any) error {
	switch v := value.(type) {
	case string:
		if v == "" {
			delete(cfg, key)
			return nil
		}
	case []string:
		if len(v) == 0 {
			delete(cfg, key)
			return nil
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode %s: %w", key, err)
	}
	cfg[key] = encoded
	return nil
}

// verifyAuthChannel proves, against live Slack, that the channel can serve as
// an authorization roster: it exists, it is a private channel, and the bot is
// in it. Refusing here turns a silent runtime lockout into an error message the
// admin reads while they still have the channel open.
func (s *InstallService) verifyAuthChannel(ctx context.Context, config json.RawMessage, channelID string) error {
	creds, err := decodeCredentials(config, s.box.Open)
	if err != nil {
		return fmt.Errorf("decode installation credentials: %w", err)
	}
	roster := s.newAuthRoster(creds)
	if _, err := roster.PrivateChannelMembers(ctx, channelID); err != nil {
		if errors.Is(err, ErrAuthChannelNotPrivate) || errors.Is(err, ErrAuthChannelNotJoined) {
			return err
		}
		return fmt.Errorf("%w: %v", ErrAuthChannelUnreadable, err)
	}
	return nil
}

// newAuthRoster builds the Slack-backed roster reader. It is a field-free hook
// so tests can substitute a fake without a network.
func (s *InstallService) newAuthRoster(creds credentials) conversationRoster {
	if s.authRosterFactory != nil {
		return s.authRosterFactory(creds)
	}
	return &slackRoster{api: slack.New(creds.BotToken), botUserID: creds.BotUserID}
}
