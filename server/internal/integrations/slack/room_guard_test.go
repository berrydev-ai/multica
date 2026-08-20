package slack

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// rosterQueries answers the identity predicate per Slack user, which the shared
// fakeIdentityQueries cannot do — the room guard's whole job is to reach a
// different verdict for different people in the same conversation.
type rosterQueries struct {
	// authorized maps a Slack user id to the Multica user it is bound to.
	// Absent means unbound; present but in notMembers means bound to somebody
	// who has since left the workspace.
	authorized map[string]pgtype.UUID
	notMembers map[string]bool
	bindErrFor map[string]error

	createCalls int
}

func (q *rosterQueries) GetChannelUserBindingByUserID(_ context.Context, arg db.GetChannelUserBindingByUserIDParams) (db.ChannelUserBinding, error) {
	if err, ok := q.bindErrFor[arg.ChannelUserID]; ok {
		return db.ChannelUserBinding{}, err
	}
	userID, ok := q.authorized[arg.ChannelUserID]
	if !ok {
		return db.ChannelUserBinding{}, pgx.ErrNoRows
	}
	return db.ChannelUserBinding{MulticaUserID: userID, ChannelUserID: arg.ChannelUserID}, nil
}

func (q *rosterQueries) FindReusableChannelUserBinding(_ context.Context, _ db.FindReusableChannelUserBindingParams) (db.ChannelUserBinding, error) {
	return db.ChannelUserBinding{}, pgx.ErrNoRows
}

func (q *rosterQueries) GetMemberByUserAndWorkspace(_ context.Context, arg db.GetMemberByUserAndWorkspaceParams) (db.Member, error) {
	for slackID, userID := range q.authorized {
		if userID == arg.UserID && q.notMembers[slackID] {
			return db.Member{}, pgx.ErrNoRows
		}
	}
	return db.Member{}, nil
}

func (q *rosterQueries) CreateChannelUserBinding(_ context.Context, _ db.CreateChannelUserBindingParams) (db.ChannelUserBinding, error) {
	q.createCalls++
	return db.ChannelUserBinding{}, nil
}

// fakeRoster stands in for conversations.members + users.info. members answers
// any channel not named in perChannel, which is what the single-conversation
// cases want; perChannel lets a case give the authorization channel and the
// conversation different rosters.
type fakeRoster struct {
	members    []string
	perChannel map[string][]string
	err        error
	errFor     map[string]error
	calls      int
}

// PrivateChannelMembers stands in for the conversations.info + members pair.
// A case models a public channel, or one the bot has not joined, by putting the
// matching sentinel in errFor.
func (f *fakeRoster) PrivateChannelMembers(ctx context.Context, channelID string) ([]string, error) {
	return f.lookup(ctx, channelID)
}

func (f *fakeRoster) lookup(_ context.Context, channelID string) ([]string, error) {
	f.calls++
	if err, ok := f.errFor[channelID]; ok {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	if members, ok := f.perChannel[channelID]; ok {
		return members, nil
	}
	return f.members, nil
}

func (f *fakeRoster) HumanMembers(ctx context.Context, channelID string) ([]string, error) {
	return f.lookup(ctx, channelID)
}

func testGuard(t *testing.T, q identityQueries, roster conversationRoster) *roomGuard {
	t.Helper()
	g := newRoomGuard(&identityResolver{q: q}, nil, slog.New(slog.DiscardHandler))
	g.newRoster = func(credentials) conversationRoster { return roster }
	return g
}

// guardInstallation carries a decryptable bot token plus the access policy the
// case under test needs. It is gated by default, because that is the only
// posture in which the guard has an opinion — see TestRoomGuard_OpenInstallation
// for the ungated one.
func guardInstallation(t *testing.T, policy string) engine.ResolvedInstallation {
	t.Helper()
	inst := testResolvedInstallation(t)
	cfg := `{"app_id":"T1","bot_user_id":"UBOT","bot_token_encrypted":"eG94Yi10ZXN0","chat_gate":"members_only"` + policy + `}`
	inst.Platform = db.ChannelInstallation{Config: []byte(cfg)}
	return inst
}

// openInstallation is the product default: no chat_gate set at all.
func openInstallation(t *testing.T) engine.ResolvedInstallation {
	t.Helper()
	inst := testResolvedInstallation(t)
	inst.Platform = db.ChannelInstallation{
		Config: []byte(`{"app_id":"T1","bot_user_id":"UBOT","bot_token_encrypted":"eG94Yi10ZXN0"}`),
	}
	return inst
}

// An installation nobody has locked down must behave exactly as it did before
// the guard existed: every conversation the bot was invited into is served once
// the speaker passes the identity gate. Turning the guard on is a deliberate
// act, not something an upgrade does to a working install.
func TestRoomGuard_OpenInstallationGatesNothing(t *testing.T) {
	roster := &fakeRoster{err: errors.New("missing_scope")}
	g := testGuard(t, &rosterQueries{}, roster)

	for _, msg := range []channel.InboundMessage{
		guardMessage(t, channel.ChatTypeGroup, "channel", "C1", "UALICE"),
		guardMessage(t, channel.ChatTypeGroup, "mpim", "G1", "UALICE"),
		guardMessage(t, channel.ChatTypeP2P, "im", "D1", "UALICE"),
	} {
		if _, err := g.ResolveValidatedInbound(context.Background(), openInstallation(t), engine.ResolvedIdentity{}, msg); err != nil {
			t.Fatalf("chat type %q: an ungated installation must not be denied: %v", msg.Source.ChatType, err)
		}
	}
	if roster.calls != 0 {
		t.Error("an ungated installation must not spend a Slack API call deciding")
	}
}

func guardMessage(t *testing.T, chatType channel.ChatType, slackChannelType, chatID, sender string) channel.InboundMessage {
	t.Helper()
	raw, err := json.Marshal(slackRawEvent{TeamID: "T042", EventType: "message", ChannelType: slackChannelType})
	if err != nil {
		t.Fatalf("marshal raw: %v", err)
	}
	return channel.InboundMessage{
		MessageID: "1700000000.000100",
		Source: channel.Source{
			ChannelType: TypeSlack,
			ChatID:      chatID,
			ChatType:    chatType,
			SenderID:    sender,
		},
		Raw: raw,
	}
}

func TestRoomGuard_DirectMessageIsAllowed(t *testing.T) {
	roster := &fakeRoster{}
	g := testGuard(t, &rosterQueries{}, roster)

	_, err := g.ResolveValidatedInbound(context.Background(), guardInstallation(t, ""), engine.ResolvedIdentity{},
		guardMessage(t, channel.ChatTypeP2P, "im", "D1", "UALICE"))
	if err != nil {
		t.Fatalf("a direct message from an already-authorized sender must pass: %v", err)
	}
	if roster.calls != 0 {
		t.Error("a direct message has one counterpart and needs no roster lookup")
	}
}

func TestRoomGuard_ChannelRequiresAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name    string
		policy  string
		chatID  string
		wantErr bool
	}{
		{name: "empty allowlist denies every channel", policy: "", chatID: "C1", wantErr: true},
		{name: "allowlisted channel passes", policy: `,"allowed_channel_ids":["C1"]`, chatID: "C1"},
		{name: "a different channel is still denied", policy: `,"allowed_channel_ids":["C1"]`, chatID: "C2", wantErr: true},
		// Slack ids are case-sensitive, so a case-folded entry is a different id.
		{name: "case must match exactly", policy: `,"allowed_channel_ids":["c1"]`, chatID: "C1", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := testGuard(t, &rosterQueries{}, &fakeRoster{})
			_, err := g.ResolveValidatedInbound(context.Background(), guardInstallation(t, tc.policy), engine.ResolvedIdentity{},
				guardMessage(t, channel.ChatTypeGroup, "channel", tc.chatID, "UALICE"))
			if tc.wantErr && !errors.Is(err, engine.ErrRoomNotAuthorized) {
				t.Fatalf("err = %v, want ErrRoomNotAuthorized", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected denial: %v", err)
			}
		})
	}
}

// An app_mention payload carries no channel_type. Treating the unknown case as
// a channel means it needs the allowlist, which is the stricter reading — the
// alternative would let a private channel be authorized by its membership.
func TestRoomGuard_MissingChannelTypeIsTreatedAsChannel(t *testing.T) {
	roster := &fakeRoster{members: []string{"UALICE"}}
	g := testGuard(t, &rosterQueries{authorized: map[string]pgtype.UUID{"UALICE": mustUUID(t, "99999999-9999-9999-9999-999999999999")}}, roster)

	_, err := g.ResolveValidatedInbound(context.Background(), guardInstallation(t, ""), engine.ResolvedIdentity{},
		guardMessage(t, channel.ChatTypeGroup, "", "G1", "UALICE"))
	if !errors.Is(err, engine.ErrRoomNotAuthorized) {
		t.Fatalf("err = %v, want ErrRoomNotAuthorized", err)
	}
	if roster.calls != 0 {
		t.Error("an unknown conversation kind must not be authorized by membership")
	}
}

func TestRoomGuard_GroupDM(t *testing.T) {
	alice := mustUUID(t, "99999999-9999-9999-9999-999999999999")
	bob := mustUUID(t, "88888888-8888-8888-8888-888888888888")

	t.Run("every member authorized passes", func(t *testing.T) {
		q := &rosterQueries{authorized: map[string]pgtype.UUID{"UALICE": alice, "UBOB": bob}}
		g := testGuard(t, q, &fakeRoster{members: []string{"UALICE", "UBOB"}})
		if _, err := g.ResolveValidatedInbound(context.Background(), guardInstallation(t, ""), engine.ResolvedIdentity{},
			guardMessage(t, channel.ChatTypeGroup, "mpim", "G1", "UALICE")); err != nil {
			t.Fatalf("a group DM of authorized people must pass: %v", err)
		}
		if q.createCalls != 0 {
			t.Error("the guard must not write a binding for a bystander it merely inspected")
		}
	})

	t.Run("one outsider denies the room even when the speaker is authorized", func(t *testing.T) {
		q := &rosterQueries{authorized: map[string]pgtype.UUID{"UALICE": alice}}
		g := testGuard(t, q, &fakeRoster{members: []string{"UALICE", "UOUTSIDER"}})
		_, err := g.ResolveValidatedInbound(context.Background(), guardInstallation(t, ""), engine.ResolvedIdentity{},
			guardMessage(t, channel.ChatTypeGroup, "mpim", "G1", "UALICE"))
		if !errors.Is(err, engine.ErrRoomNotAuthorized) {
			t.Fatalf("err = %v, want ErrRoomNotAuthorized", err)
		}
	})

	t.Run("a member who left the workspace is an outsider", func(t *testing.T) {
		q := &rosterQueries{
			authorized: map[string]pgtype.UUID{"UALICE": alice, "UBOB": bob},
			notMembers: map[string]bool{"UBOB": true},
		}
		g := testGuard(t, q, &fakeRoster{members: []string{"UALICE", "UBOB"}})
		_, err := g.ResolveValidatedInbound(context.Background(), guardInstallation(t, ""), engine.ResolvedIdentity{},
			guardMessage(t, channel.ChatTypeGroup, "mpim", "G1", "UALICE"))
		if !errors.Is(err, engine.ErrRoomNotAuthorized) {
			t.Fatalf("err = %v, want ErrRoomNotAuthorized", err)
		}
	})

	// The usual cause is a missing mpim:read scope. An unknown roster must not
	// be read as an empty one.
	t.Run("an unreadable roster denies", func(t *testing.T) {
		g := testGuard(t, &rosterQueries{}, &fakeRoster{err: errors.New("missing_scope")})
		_, err := g.ResolveValidatedInbound(context.Background(), guardInstallation(t, ""), engine.ResolvedIdentity{},
			guardMessage(t, channel.ChatTypeGroup, "mpim", "G1", "UALICE"))
		if !errors.Is(err, engine.ErrRoomNotAuthorized) {
			t.Fatalf("err = %v, want ErrRoomNotAuthorized", err)
		}
	})

	t.Run("an empty roster denies", func(t *testing.T) {
		g := testGuard(t, &rosterQueries{}, &fakeRoster{})
		_, err := g.ResolveValidatedInbound(context.Background(), guardInstallation(t, ""), engine.ResolvedIdentity{},
			guardMessage(t, channel.ChatTypeGroup, "mpim", "G1", "UALICE"))
		if !errors.Is(err, engine.ErrRoomNotAuthorized) {
			t.Fatalf("err = %v, want ErrRoomNotAuthorized", err)
		}
	})

	t.Run("a failed membership lookup denies", func(t *testing.T) {
		q := &rosterQueries{
			authorized: map[string]pgtype.UUID{"UALICE": alice},
			bindErrFor: map[string]error{"UALICE": errors.New("connection reset")},
		}
		g := testGuard(t, q, &fakeRoster{members: []string{"UALICE"}})
		_, err := g.ResolveValidatedInbound(context.Background(), guardInstallation(t, ""), engine.ResolvedIdentity{},
			guardMessage(t, channel.ChatTypeGroup, "mpim", "G1", "UALICE"))
		if !errors.Is(err, engine.ErrRoomNotAuthorized) {
			t.Fatalf("err = %v, want ErrRoomNotAuthorized", err)
		}
	})
}

func TestRoomGuard_RosterCacheHasAShortLife(t *testing.T) {
	alice := mustUUID(t, "99999999-9999-9999-9999-999999999999")
	q := &rosterQueries{authorized: map[string]pgtype.UUID{"UALICE": alice}}
	roster := &fakeRoster{members: []string{"UALICE"}}
	g := testGuard(t, q, roster)
	now := time.Unix(1_700_000_000, 0)
	g.cache = newRosterCache(func() time.Time { return now })

	msg := guardMessage(t, channel.ChatTypeGroup, "mpim", "G1", "UALICE")
	inst := guardInstallation(t, "")
	for range 3 {
		if _, err := g.ResolveValidatedInbound(context.Background(), inst, engine.ResolvedIdentity{}, msg); err != nil {
			t.Fatalf("unexpected denial: %v", err)
		}
	}
	if roster.calls != 1 {
		t.Fatalf("roster fetches = %d, want 1 within the TTL", roster.calls)
	}

	// Past the TTL an added outsider has to be noticed, so the roster is read
	// again rather than trusted.
	now = now.Add(rosterTTL + time.Second)
	roster.members = []string{"UALICE", "UOUTSIDER"}
	_, err := g.ResolveValidatedInbound(context.Background(), inst, engine.ResolvedIdentity{}, msg)
	if !errors.Is(err, engine.ErrRoomNotAuthorized) {
		t.Fatalf("err = %v, want the expired roster to be re-read and the room denied", err)
	}
	if roster.calls != 2 {
		t.Fatalf("roster fetches = %d, want a refetch once the entry expired", roster.calls)
	}
}

func TestRoomGuard_UnreadableEnvelopeDenies(t *testing.T) {
	g := testGuard(t, &rosterQueries{}, &fakeRoster{})
	msg := guardMessage(t, channel.ChatTypeGroup, "channel", "C1", "UALICE")
	msg.Raw = []byte(`{"team_id":`) // truncated

	if _, err := g.ResolveValidatedInbound(context.Background(), guardInstallation(t, ""), engine.ResolvedIdentity{}, msg); !errors.Is(err, engine.ErrRoomNotAuthorized) {
		t.Fatalf("err = %v, want ErrRoomNotAuthorized", err)
	}
}

func TestDecodeRoomPolicy(t *testing.T) {
	t.Run("whitespace and empty entries are forgiven, ids are not folded", func(t *testing.T) {
		p := decodeRoomPolicy(json.RawMessage(`{"allowed_channel_ids":[" C123 ","","  ","C456"]}`))
		if len(p.allowedChannels) != 2 {
			t.Fatalf("allowed channels = %v, want exactly two", p.allowedChannels)
		}
		if !p.allowsChannel("C123") || !p.allowsChannel("C456") {
			t.Errorf("trimmed ids must be allowed: %v", p.allowedChannels)
		}
		if p.allowsChannel("c123") {
			t.Error("Slack ids are case-sensitive; a folded id must not match")
		}
	})

	t.Run("the default policy allows no channel and keeps the prompt", func(t *testing.T) {
		p := decodeRoomPolicy(json.RawMessage(`{"app_id":"A1"}`))
		if p.allowsChannel("C1") {
			t.Error("an installation that named no channel must not serve any")
		}
		if p.gate != ChatGateOpen {
			t.Errorf("gate = %q, want the product default", p.gate)
		}
	})

	t.Run("an unreadable config decodes to the closed default", func(t *testing.T) {
		p := decodeRoomPolicy(json.RawMessage(`not json`))
		if p.allowsChannel("C1") || p.gate != ChatGateOpen {
			t.Error("a config that cannot be parsed must never widen access")
		}
	})

	t.Run("members_only is recognised with surrounding whitespace", func(t *testing.T) {
		if got := decodeRoomPolicy(json.RawMessage(`{"chat_gate":" members_only "}`)).gate; got != ChatGateMembersOnly {
			t.Errorf("gate = %q, want members_only", got)
		}
	})

	t.Run("an unknown gate value falls back to the default, not to nothing", func(t *testing.T) {
		if got := decodeRoomPolicy(json.RawMessage(`{"chat_gate":"maybe"}`)).gate; got != ChatGateOpen {
			t.Errorf("gate = %q, want open", got)
		}
	})
}

// --- authorization channel --------------------------------------------------
//
// The installation names a private Slack channel whose membership is the
// roster. Granting access is an invite; revoking it is a removal. Nothing has
// to be edited or redeployed, which is the entire point of preferring it to a
// list held somewhere else.

const authChannelPolicy = `,"auth_channel_id":"GAUTH"`

func TestRoomGuard_AuthChannel(t *testing.T) {
	alice := mustUUID(t, "99999999-9999-9999-9999-999999999999")
	bound := func() *rosterQueries {
		return &rosterQueries{authorized: map[string]pgtype.UUID{
			"UALICE": alice,
			"UBOB":   mustUUID(t, "88888888-8888-8888-8888-888888888888"),
		}}
	}

	t.Run("a member of the channel is served in a DM", func(t *testing.T) {
		g := testGuard(t, bound(), &fakeRoster{perChannel: map[string][]string{"GAUTH": {"UALICE", "UBOB"}}})
		if _, err := g.ResolveValidatedInbound(context.Background(), guardInstallation(t, authChannelPolicy), engine.ResolvedIdentity{},
			guardMessage(t, channel.ChatTypeP2P, "im", "D1", "UALICE")); err != nil {
			t.Fatalf("unexpected denial: %v", err)
		}
	})

	// The whole reason to prefer a channel: removing somebody from it is the
	// revocation. A bound workspace member who is not in the channel is out.
	t.Run("a bound workspace member outside the channel is denied", func(t *testing.T) {
		g := testGuard(t, bound(), &fakeRoster{perChannel: map[string][]string{"GAUTH": {"UBOB"}}})
		_, err := g.ResolveValidatedInbound(context.Background(), guardInstallation(t, authChannelPolicy), engine.ResolvedIdentity{},
			guardMessage(t, channel.ChatTypeP2P, "im", "D1", "UALICE"))
		// The person, not the place: they would be refused anywhere.
		if !errors.Is(err, engine.ErrSenderNotPermitted) {
			t.Fatalf("err = %v, want ErrSenderNotPermitted", err)
		}
	})

	// Being in the channel is not a way to become a workspace member. It can
	// only narrow the set the binding gate already produced.
	t.Run("the channel cannot admit somebody who is not bound", func(t *testing.T) {
		q := &rosterQueries{authorized: map[string]pgtype.UUID{"UALICE": alice}}
		g := testGuard(t, q, &fakeRoster{perChannel: map[string][]string{
			"GAUTH": {"UALICE", "USTRANGER"},
			"G1":    {"UALICE", "USTRANGER"},
		}})
		_, err := g.ResolveValidatedInbound(context.Background(), guardInstallation(t, authChannelPolicy), engine.ResolvedIdentity{},
			guardMessage(t, channel.ChatTypeGroup, "mpim", "G1", "UALICE"))
		if !errors.Is(err, engine.ErrRoomNotAuthorized) {
			t.Fatalf("err = %v, want the unbound member to close the room", err)
		}
	})

	t.Run("a group DM of channel members passes", func(t *testing.T) {
		g := testGuard(t, bound(), &fakeRoster{perChannel: map[string][]string{
			"GAUTH": {"UALICE", "UBOB"},
			"G1":    {"UALICE", "UBOB"},
		}})
		if _, err := g.ResolveValidatedInbound(context.Background(), guardInstallation(t, authChannelPolicy), engine.ResolvedIdentity{},
			guardMessage(t, channel.ChatTypeGroup, "mpim", "G1", "UALICE")); err != nil {
			t.Fatalf("unexpected denial: %v", err)
		}
	})

	t.Run("a group DM member outside the channel closes the room", func(t *testing.T) {
		g := testGuard(t, bound(), &fakeRoster{perChannel: map[string][]string{
			"GAUTH": {"UALICE"},
			"G1":    {"UALICE", "UBOB"},
		}})
		_, err := g.ResolveValidatedInbound(context.Background(), guardInstallation(t, authChannelPolicy), engine.ResolvedIdentity{},
			guardMessage(t, channel.ChatTypeGroup, "mpim", "G1", "UALICE"))
		if !errors.Is(err, engine.ErrRoomNotAuthorized) {
			t.Fatalf("err = %v, want ErrRoomNotAuthorized", err)
		}
	})

	// A public channel is not usable as an authorization roster, because anyone
	// can join one and thereby authorize themselves. The app is given
	// groups:read and not channels:read so Slack refuses the read, and the
	// guard turns that refusal into a denial rather than a guess.
	t.Run("a roster Slack will not read denies", func(t *testing.T) {
		g := testGuard(t, bound(), &fakeRoster{errFor: map[string]error{"GAUTH": errors.New("missing_scope")}})
		_, err := g.ResolveValidatedInbound(context.Background(), guardInstallation(t, authChannelPolicy), engine.ResolvedIdentity{},
			guardMessage(t, channel.ChatTypeP2P, "im", "D1", "UALICE"))
		if !errors.Is(err, engine.ErrRoomNotAuthorized) {
			t.Fatalf("err = %v, want ErrRoomNotAuthorized", err)
		}
	})

	t.Run("no channel configured leaves the binding gate alone", func(t *testing.T) {
		roster := &fakeRoster{}
		g := testGuard(t, bound(), roster)
		if _, err := g.ResolveValidatedInbound(context.Background(), guardInstallation(t, ""), engine.ResolvedIdentity{},
			guardMessage(t, channel.ChatTypeP2P, "im", "D1", "UALICE")); err != nil {
			t.Fatalf("unexpected denial: %v", err)
		}
		if roster.calls != 0 {
			t.Error("without an authorization channel there is no roster to read")
		}
	})
}

// An outage must not take the bot dark for people already on the roster, and
// must not let anybody new in. Those two requirements are what the grace window
// buys, and they are asymmetric on purpose.
func TestRoomGuard_AuthChannelSurvivesASlackOutage(t *testing.T) {
	alice := mustUUID(t, "99999999-9999-9999-9999-999999999999")
	q := &rosterQueries{authorized: map[string]pgtype.UUID{
		"UALICE": alice,
		"UCAROL": mustUUID(t, "77777777-7777-7777-7777-777777777777"),
	}}
	roster := &fakeRoster{perChannel: map[string][]string{"GAUTH": {"UALICE"}}}
	g := testGuard(t, q, roster)
	now := time.Unix(1_700_000_000, 0)
	g.cache = newRosterCache(func() time.Time { return now })
	inst := guardInstallation(t, authChannelPolicy)

	// Prime the roster while Slack is healthy.
	if _, err := g.ResolveValidatedInbound(context.Background(), inst, engine.ResolvedIdentity{},
		guardMessage(t, channel.ChatTypeP2P, "im", "D1", "UALICE")); err != nil {
		t.Fatalf("unexpected denial: %v", err)
	}

	roster.errFor = map[string]error{"GAUTH": errors.New("slack unavailable")}
	now = now.Add(rosterTTL + time.Minute)

	if _, err := g.ResolveValidatedInbound(context.Background(), inst, engine.ResolvedIdentity{},
		guardMessage(t, channel.ChatTypeP2P, "im", "D1", "UALICE")); err != nil {
		t.Errorf("somebody already on the roster must keep working through an outage: %v", err)
	}
	if _, err := g.ResolveValidatedInbound(context.Background(), inst, engine.ResolvedIdentity{},
		guardMessage(t, channel.ChatTypeP2P, "im", "D2", "UCAROL")); !errors.Is(err, engine.ErrSenderNotPermitted) {
		t.Errorf("err = %v, want a stale roster to admit nobody new", err)
	}

	// Past the grace window the guard stops guessing. There is no longer any
	// evidence about this person either way, so the conversation is refused
	// rather than the person being told they are not permitted.
	now = now.Add(rosterGrace)
	if _, err := g.ResolveValidatedInbound(context.Background(), inst, engine.ResolvedIdentity{},
		guardMessage(t, channel.ChatTypeP2P, "im", "D1", "UALICE")); !errors.Is(err, engine.ErrRoomNotAuthorized) {
		t.Errorf("err = %v, want the grace window to close", err)
	}
}

// A multi-party DM roster is never served stale: the members a stale copy omits
// are exactly the outsiders the rule exists to catch.
func TestRoomGuard_GroupDMRosterIsNeverServedStale(t *testing.T) {
	alice := mustUUID(t, "99999999-9999-9999-9999-999999999999")
	q := &rosterQueries{authorized: map[string]pgtype.UUID{"UALICE": alice}}
	roster := &fakeRoster{perChannel: map[string][]string{"G1": {"UALICE"}}}
	g := testGuard(t, q, roster)
	now := time.Unix(1_700_000_000, 0)
	g.cache = newRosterCache(func() time.Time { return now })
	inst := guardInstallation(t, "")
	msg := guardMessage(t, channel.ChatTypeGroup, "mpim", "G1", "UALICE")

	if _, err := g.ResolveValidatedInbound(context.Background(), inst, engine.ResolvedIdentity{}, msg); err != nil {
		t.Fatalf("unexpected denial: %v", err)
	}
	roster.errFor = map[string]error{"G1": errors.New("slack unavailable")}
	now = now.Add(rosterTTL + time.Second)

	if _, err := g.ResolveValidatedInbound(context.Background(), inst, engine.ResolvedIdentity{}, msg); !errors.Is(err, engine.ErrRoomNotAuthorized) {
		t.Fatalf("err = %v, want the room closed rather than judged on an old roster", err)
	}
}

func TestDecodeRoomPolicy_AuthChannel(t *testing.T) {
	if got := decodeRoomPolicy(json.RawMessage(`{"auth_channel_id":"  GAUTH  "}`)).authChannelID; got != "GAUTH" {
		t.Errorf("authChannelID = %q, want the trimmed id", got)
	}
	if got := decodeRoomPolicy(json.RawMessage(`{"auth_channel_id":"   "}`)).authChannelID; got != "" {
		t.Errorf("authChannelID = %q, want a blank entry to mean no channel gate", got)
	}
}

// A channel can be converted to public, or the Bot removed from it, after the
// policy was validated. Those are verdicts rather than outages: the guard must
// deny immediately instead of coasting on the cached roster, because the grace
// window would otherwise be exactly the window an attacker wants.
func TestRoomGuard_AuthChannelVerdictsBypassTheGraceWindow(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "converted to public", err: ErrAuthChannelNotPrivate},
		{name: "bot removed from the channel", err: ErrAuthChannelNotJoined},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alice := mustUUID(t, "99999999-9999-9999-9999-999999999999")
			q := &rosterQueries{authorized: map[string]pgtype.UUID{"UALICE": alice}}
			roster := &fakeRoster{perChannel: map[string][]string{"GAUTH": {"UALICE"}}}
			g := testGuard(t, q, roster)
			now := time.Unix(1_700_000_000, 0)
			g.cache = newRosterCache(func() time.Time { return now })
			inst := guardInstallation(t, authChannelPolicy)
			msg := guardMessage(t, channel.ChatTypeP2P, "im", "D1", "UALICE")

			if _, err := g.ResolveValidatedInbound(context.Background(), inst, engine.ResolvedIdentity{}, msg); err != nil {
				t.Fatalf("unexpected denial while healthy: %v", err)
			}
			roster.errFor = map[string]error{"GAUTH": tc.err}
			now = now.Add(rosterTTL + time.Second)

			if _, err := g.ResolveValidatedInbound(context.Background(), inst, engine.ResolvedIdentity{}, msg); !errors.Is(err, engine.ErrRoomNotAuthorized) {
				t.Fatalf("err = %v, want an immediate denial", err)
			}
			// And the cached roster is gone, so a later outage cannot resurrect it.
			roster.errFor = map[string]error{"GAUTH": errors.New("slack unavailable")}
			if _, err := g.ResolveValidatedInbound(context.Background(), inst, engine.ResolvedIdentity{}, msg); !errors.Is(err, engine.ErrRoomNotAuthorized) {
				t.Fatalf("err = %v, want the dropped roster to stay dropped", err)
			}
		})
	}
}

// The two channel lists answer different questions and must not be confused:
// #mika-ops decides WHO, the work channels decide WHERE. This pins the four
// corners of that grid.
func TestRoomGuard_RosterAndWorkChannelsAreSeparate(t *testing.T) {
	const policy = `,"auth_channel_id":"GOPS","allowed_channel_ids":["CWORK"]`
	operator := mustUUID(t, "99999999-9999-9999-9999-999999999999")
	colleague := mustUUID(t, "88888888-8888-8888-8888-888888888888")
	q := func() *rosterQueries {
		return &rosterQueries{authorized: map[string]pgtype.UUID{"UOPS": operator, "UOTHER": colleague}}
	}
	roster := func() *fakeRoster {
		return &fakeRoster{perChannel: map[string][]string{"GOPS": {"UOPS"}}}
	}

	for _, tc := range []struct {
		name    string
		chatID  string
		sender  string
		wantErr error
	}{
		{
			name:   "an operator in a work channel is served",
			chatID: "CWORK", sender: "UOPS",
		},
		{
			// The person is the problem, so the answer must be about the person.
			name:   "a non-operator in a work channel is refused as a person",
			chatID: "CWORK", sender: "UOTHER", wantErr: engine.ErrSenderNotPermitted,
		},
		{
			// The person is fine; this is simply not a room the bot works in.
			name:   "an operator in an unlisted channel is refused as a place",
			chatID: "CELSE", sender: "UOPS", wantErr: engine.ErrRoomNotAuthorized,
		},
		{
			name:   "a non-operator in an unlisted channel is still a person problem",
			chatID: "CELSE", sender: "UOTHER", wantErr: engine.ErrSenderNotPermitted,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := testGuard(t, q(), roster())
			_, err := g.ResolveValidatedInbound(context.Background(), guardInstallation(t, policy), engine.ResolvedIdentity{},
				guardMessage(t, channel.ChatTypeGroup, "channel", tc.chatID, tc.sender))
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected denial: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}

	// Holding the roster does not make #mika-ops a place the bot works. It has
	// to be listed as a work channel too, like any other.
	t.Run("the roster channel is not implicitly a work channel", func(t *testing.T) {
		g := testGuard(t, q(), roster())
		_, err := g.ResolveValidatedInbound(context.Background(), guardInstallation(t, policy), engine.ResolvedIdentity{},
			guardMessage(t, channel.ChatTypeGroup, "channel", "GOPS", "UOPS"))
		if !errors.Is(err, engine.ErrRoomNotAuthorized) {
			t.Fatalf("err = %v, want ErrRoomNotAuthorized", err)
		}
	})

	// An unreadable roster proves nothing about the person, so it must not be
	// reported to them as a personal refusal.
	t.Run("an unreadable roster denies the room, not the person", func(t *testing.T) {
		r := roster()
		r.errFor = map[string]error{"GOPS": errors.New("slack unavailable")}
		g := testGuard(t, q(), r)
		_, err := g.ResolveValidatedInbound(context.Background(), guardInstallation(t, policy), engine.ResolvedIdentity{},
			guardMessage(t, channel.ChatTypeGroup, "channel", "CWORK", "UOPS"))
		if !errors.Is(err, engine.ErrRoomNotAuthorized) {
			t.Fatalf("err = %v, want ErrRoomNotAuthorized", err)
		}
	})
}

func TestNormalizeRefusalText(t *testing.T) {
	if got := NormalizeRefusalText("  Not an authorized Mika operator  "); got != "Not an authorized Mika operator" {
		t.Errorf("got %q", got)
	}
	if got := NormalizeRefusalText("two\nlines\there"); got != "two lines here" {
		t.Errorf("got %q, want a single line", got)
	}
	if got := NormalizeRefusalText("   "); got != "" {
		t.Errorf("got %q, want empty to mean the default", got)
	}
	long := NormalizeRefusalText(strings.Repeat("é", MaxRefusalTextRunes+50))
	if runes := []rune(long); len(runes) != MaxRefusalTextRunes {
		t.Errorf("length = %d runes, want a cap on a rune boundary", len(runes))
	}
	if decodeRoomPolicy(json.RawMessage(`{"app_id":"A1"}`)).refusal() != RefusalText {
		t.Error("an installation that set no text must get the shipped default")
	}
	if decodeRoomPolicy(json.RawMessage(`{"refusal_text":"Not an authorized Mika operator"}`)).refusal() != "Not an authorized Mika operator" {
		t.Error("a configured refusal must win over the default")
	}
}
