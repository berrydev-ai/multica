package slack

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// policyInstallation is a stored installation with real encrypted tokens, so
// SetAccessPolicy exercises the same decode path production uses.
func policyInstallation(t *testing.T) *db.ChannelInstallation {
	t.Helper()
	box := testBox(t)
	enc, err := box.Seal([]byte("xoxb-test"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	cfg, err := json.Marshal(map[string]any{
		"app_id":              "A1",
		"team_id":             "T1",
		"bot_user_id":         "UBOT",
		"bot_token_encrypted": base64.StdEncoding.EncodeToString(enc),
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return &db.ChannelInstallation{ID: mustUUID(t, "44444444-4444-4444-4444-444444444444"), Config: cfg}
}

func policyService(t *testing.T, q *fakeInstallQueries, roster conversationRoster) *InstallService {
	t.Helper()
	svc := newTestInstallService(t, q)
	svc.authRosterFactory = func(credentials) conversationRoster { return roster }
	return svc
}

func TestSetAccessPolicy(t *testing.T) {
	wsID := mustUUID(t, "11111111-1111-1111-1111-111111111111")
	instID := mustUUID(t, "44444444-4444-4444-4444-444444444444")

	t.Run("stores the policy and keeps the tokens", func(t *testing.T) {
		q := &fakeInstallQueries{getInWorkspace: policyInstallation(t)}
		svc := policyService(t, q, &fakeRoster{perChannel: map[string][]string{"GAUTH": {"UALICE"}}})

		got, err := svc.SetAccessPolicy(context.Background(), instID, wsID, AccessPolicyInput{
			Gate:             ChatGateMembersOnly,
			AuthChannelID:    "  GAUTH  ",
			AllowedChannelID: []string{" C1 ", "", "C2"},
		})
		if err != nil {
			t.Fatalf("SetAccessPolicy: %v", err)
		}
		if got.Gate != ChatGateMembersOnly || got.AuthChannelID != "GAUTH" {
			t.Errorf("policy = %+v", got)
		}
		if len(got.AllowedChannelIDs) != 2 {
			t.Errorf("allowed channels = %v, want the blank entry dropped", got.AllowedChannelIDs)
		}
		// The encrypted bot token must survive a policy edit — losing it would
		// silently disconnect the Bot.
		var saved map[string]any
		if err := json.Unmarshal(q.savedConfig, &saved); err != nil {
			t.Fatalf("decode saved config: %v", err)
		}
		if saved["bot_token_encrypted"] == nil || saved["app_id"] != "A1" {
			t.Errorf("saved config lost fields it did not own: %v", saved)
		}
		if decodeRoomPolicy(q.savedConfig).authChannelID != "GAUTH" {
			t.Error("the persisted blob must read back through the guard's own decoder")
		}
	})

	// A public channel is the failure this endpoint exists to catch: anyone can
	// join one, so anyone could authorize themselves.
	t.Run("refuses a public channel", func(t *testing.T) {
		q := &fakeInstallQueries{getInWorkspace: policyInstallation(t)}
		svc := policyService(t, q, &fakeRoster{errFor: map[string]error{"CPUB": ErrAuthChannelNotPrivate}})

		_, err := svc.SetAccessPolicy(context.Background(), instID, wsID, AccessPolicyInput{
			Gate: ChatGateMembersOnly, AuthChannelID: "CPUB",
		})
		if !errors.Is(err, ErrAuthChannelNotPrivate) {
			t.Fatalf("err = %v, want ErrAuthChannelNotPrivate", err)
		}
		if q.savedConfig != nil {
			t.Error("a rejected policy must not be persisted")
		}
	})

	t.Run("refuses a channel the bot has not joined", func(t *testing.T) {
		q := &fakeInstallQueries{getInWorkspace: policyInstallation(t)}
		svc := policyService(t, q, &fakeRoster{errFor: map[string]error{"GAUTH": ErrAuthChannelNotJoined}})

		_, err := svc.SetAccessPolicy(context.Background(), instID, wsID, AccessPolicyInput{
			Gate: ChatGateMembersOnly, AuthChannelID: "GAUTH",
		})
		if !errors.Is(err, ErrAuthChannelNotJoined) {
			t.Fatalf("err = %v, want ErrAuthChannelNotJoined", err)
		}
	})

	t.Run("reports an unreadable channel as a configuration problem", func(t *testing.T) {
		q := &fakeInstallQueries{getInWorkspace: policyInstallation(t)}
		svc := policyService(t, q, &fakeRoster{errFor: map[string]error{"GAUTH": errors.New("channel_not_found")}})

		_, err := svc.SetAccessPolicy(context.Background(), instID, wsID, AccessPolicyInput{
			Gate: ChatGateMembersOnly, AuthChannelID: "GAUTH",
		})
		if !errors.Is(err, ErrAuthChannelUnreadable) {
			t.Fatalf("err = %v, want ErrAuthChannelUnreadable", err)
		}
	})

	t.Run("rejects an unknown gate", func(t *testing.T) {
		q := &fakeInstallQueries{getInWorkspace: policyInstallation(t)}
		svc := policyService(t, q, &fakeRoster{})
		if _, err := svc.SetAccessPolicy(context.Background(), instID, wsID, AccessPolicyInput{Gate: "maybe"}); !errors.Is(err, ErrInvalidChatGate) {
			t.Fatalf("err = %v, want ErrInvalidChatGate", err)
		}
	})

	// Clearing is stating an empty policy, and it must leave no stale key that
	// the guard would still read.
	t.Run("clearing removes the keys", func(t *testing.T) {
		inst := policyInstallation(t)
		var cfg map[string]json.RawMessage
		if err := json.Unmarshal(inst.Config, &cfg); err != nil {
			t.Fatalf("decode: %v", err)
		}
		cfg["auth_channel_id"] = json.RawMessage(`"GOLD"`)
		cfg["allowed_channel_ids"] = json.RawMessage(`["C9"]`)
		merged, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		inst.Config = merged

		q := &fakeInstallQueries{getInWorkspace: inst}
		svc := policyService(t, q, &fakeRoster{})
		got, err := svc.SetAccessPolicy(context.Background(), instID, wsID, AccessPolicyInput{Gate: ChatGateOpen})
		if err != nil {
			t.Fatalf("SetAccessPolicy: %v", err)
		}
		if got.AuthChannelID != "" || len(got.AllowedChannelIDs) != 0 {
			t.Errorf("policy = %+v, want everything cleared", got)
		}
		if _, still := mustDecodeMap(t, q.savedConfig)["auth_channel_id"]; still {
			t.Error("a cleared authorization channel must not leave its key behind")
		}
	})

	t.Run("a missing installation is not found", func(t *testing.T) {
		q := &fakeInstallQueries{getErr: pgx.ErrNoRows}
		svc := policyService(t, q, &fakeRoster{})
		if _, err := svc.SetAccessPolicy(context.Background(), instID, wsID, AccessPolicyInput{Gate: ChatGateOpen}); !errors.Is(err, ErrInstallationNotFound) {
			t.Fatalf("err = %v, want ErrInstallationNotFound", err)
		}
	})
}

func TestDescribeAccessPolicy(t *testing.T) {
	got := DescribeAccessPolicy(json.RawMessage(`{"chat_gate":"members_only","auth_channel_id":"GAUTH","allowed_channel_ids":[" C1 ",""]}`))
	if got.Gate != ChatGateMembersOnly || got.AuthChannelID != "GAUTH" {
		t.Errorf("policy = %+v", got)
	}
	if len(got.AllowedChannelIDs) != 1 || got.AllowedChannelIDs[0] != "C1" {
		t.Errorf("allowed = %v, want the blank entry dropped and the id trimmed", got.AllowedChannelIDs)
	}
	// An installation that never set a policy reads back as the open default
	// with empty lists, never as a nil the API would render as null.
	empty := DescribeAccessPolicy(json.RawMessage(`{"app_id":"A1"}`))
	if empty.Gate != ChatGateOpen || empty.AllowedChannelIDs == nil {
		t.Errorf("default policy = %+v", empty)
	}
}

func mustDecodeMap(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return out
}
