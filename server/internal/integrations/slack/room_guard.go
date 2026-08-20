package slack

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/slack-go/slack"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The room guard authorizes the CONVERSATION, not only the speaker.
//
// The identity gate upstream (identityResolver.ResolveSender) answers "may this
// person talk to the bot". That is not the whole question, because everything
// the bot says is visible to every human in the room. A reply to an authorized
// speaker in a channel of two hundred people, or in a group DM that contains
// one outsider, publishes the bot's work to all of them. So this seam asks the
// second question — "may this ROOM hear the answer" — and it runs as part of
// the same single gate, before the session is created and before any agent work
// is enqueued.
//
// The rules apply to an installation whose chat_gate is members_only. They
// change WHO can use a bot, so they are opt-in: an installation left at the
// product default keeps today's behavior, where any conversation the bot has
// been invited into is served once the speaker passes the identity gate. What
// is NOT opt-in is how a refusal is delivered — that lives in the replier, and
// no installation should ever announce itself to a room it just turned away.
//
// Under members_only, "authorized" means bound to a Multica user who is still a
// workspace member AND — when the installation names one — present in the
// authorization channel. That channel is the operational control: to grant
// access you invite somebody to it, to revoke access you remove them, and the
// list lives in Slack where the people already are instead of in a file that
// has to be edited and redeployed.
//
// Then one rule per Slack conversation kind:
//
//   - Direct message: allowed when the sender is authorized.
//   - Multi-party DM: allowed only when EVERY non-bot member is authorized. One
//     outsider denies the room even when the speaker is authorized.
//   - Channel (public or private): allowed only when the sender is authorized
//     and the channel id is on the installation's allowlist. The allowlist
//     defaults to empty, so turning the gate on without naming a channel leaves
//     the bot reachable in direct messages and authorized group DMs and
//     nowhere else.
//
// Every uncertainty denies: an unreadable envelope, a conversations.members
// call that fails, a roster that comes back empty. A false denial costs one
// message; a false allowance publishes the bot's output to whoever is standing
// there.

// slackChannelTypeMPIM is Slack's own channel_type for a multi-party DM. It is
// the one value that must be distinguished by name: an mpim and a private
// channel both normalize to a group chat, and they are authorized differently.
const slackChannelTypeMPIM = "mpim"

// rosterTTL bounds how long a conversation's member list is reused. Slack does
// not tell us when somebody is added to a conversation unless the app
// subscribes to the membership events, so the ceiling on "the roster changed
// and we have not noticed" is this TTL. Thirty seconds is short enough that
// removing somebody from the authorization channel revokes their access while
// the admin is still looking at Slack, which is why the membership events are
// not worth their own subscription.
const rosterTTL = 30 * time.Second

// rosterGrace is how long a STALE authorization roster may still be used when
// Slack cannot be reached. It applies to the authorization channel only, and
// the asymmetry is the whole point:
//
//   - A stale authorization roster can only keep serving people who were
//     already on it. Nobody new is admitted, so an outage delays a revocation
//     instead of granting access — and the alternative, hard-failing, takes the
//     bot dark for every authorized user the moment Slack has a bad minute.
//   - A stale multi-party DM roster is the opposite: the people it omits are
//     the ones who joined since, which is exactly the outsider the rule exists
//     to catch. That roster is never served stale.
const rosterGrace = 15 * time.Minute

// conversationRoster reads the membership of a Slack conversation. An error
// from either method means the roster is UNKNOWN, which is never the same thing
// as an empty room.
type conversationRoster interface {
	// PrivateChannelMembers returns every member id of an authorization
	// channel, after proving the channel is actually a private one the bot has
	// joined. It returns ErrAuthChannelNotPrivate or ErrAuthChannelNotJoined
	// for those two verdicts, which are decisions rather than outages and are
	// never softened by the stale-roster fallback.
	PrivateChannelMembers(ctx context.Context, channelID string) ([]string, error)
	// HumanMembers returns the non-bot member ids, for the rule that has to
	// reason about everyone in the room rather than look one person up.
	HumanMembers(ctx context.Context, channelID string) ([]string, error)
}

var (
	// ErrAuthChannelNotPrivate: the configured authorization channel is public
	// (or is a DM / group DM rather than a channel). A public channel cannot be
	// an authorization roster, because anybody can join one and would thereby
	// authorize themselves.
	ErrAuthChannelNotPrivate = errors.New("slack: authorization channel must be a private channel")
	// ErrAuthChannelNotJoined: the bot is not in the authorization channel, so
	// it cannot read the membership at all.
	ErrAuthChannelNotJoined = errors.New("slack: bot is not a member of the authorization channel")
)

// roomGuard implements engine.ValidatedInboundResolver for Slack.
type roomGuard struct {
	identity  *identityResolver
	decrypt   Decrypter
	newRoster func(creds credentials) conversationRoster
	cache     *rosterCache
	logger    *slog.Logger
}

var _ engine.ValidatedInboundResolver = (*roomGuard)(nil)

// newRoomGuard builds the guard. A nil logger falls back to the default.
func newRoomGuard(identity *identityResolver, decrypt Decrypter, logger *slog.Logger) *roomGuard {
	if logger == nil {
		logger = slog.Default()
	}
	return &roomGuard{
		identity: identity,
		decrypt:  decrypt,
		newRoster: func(c credentials) conversationRoster {
			return &slackRoster{api: slack.New(c.BotToken), botUserID: c.BotUserID}
		},
		cache:  newRosterCache(time.Now),
		logger: logger,
	}
}

// ResolveValidatedInbound authorizes the conversation. It never changes the
// installation it returns; the installation is passed through so the guard can
// occupy the pipeline's post-identity seam.
func (g *roomGuard) ResolveValidatedInbound(ctx context.Context, inst engine.ResolvedInstallation, _ engine.ResolvedIdentity, msg channel.InboundMessage) (engine.ResolvedInstallation, error) {
	row, ok := inst.Platform.(db.ChannelInstallation)
	if !ok {
		g.denied(ctx, msg, "installation row unavailable")
		return inst, engine.ErrRoomNotAuthorized
	}
	policy := decodeRoomPolicy(row.Config)
	if policy.gate != ChatGateMembersOnly {
		return inst, nil
	}
	creds, err := decodeCredentials(row.Config, g.decrypt)
	if err != nil {
		g.denied(ctx, msg, "installation credentials unreadable")
		return inst, engine.ErrRoomNotAuthorized
	}

	// The speaker first, in every conversation kind. Only the channel test runs
	// here: the identity gate upstream already proved they are a bound
	// workspace member, so re-asking the database would buy nothing.
	if policy.authChannelID != "" {
		present, err := g.inAuthChannel(ctx, inst, policy, creds, msg.Source.SenderID)
		if err != nil {
			// An unreadable roster proves nothing about this person, so it is
			// not their denial to receive: refuse the conversation instead.
			g.denied(ctx, msg, "authorization roster unavailable")
			return inst, engine.ErrRoomNotAuthorized
		}
		if !present {
			g.denied(ctx, msg, "sender is not on the authorization roster")
			return inst, engine.ErrSenderNotPermitted
		}
	}
	if msg.Source.ChatType == channel.ChatTypeP2P {
		return inst, nil
	}

	raw, err := decodeSlackRaw(msg)
	if err != nil {
		g.denied(ctx, msg, "inbound envelope unreadable")
		return inst, engine.ErrRoomNotAuthorized
	}
	if raw.ChannelType == slackChannelTypeMPIM {
		return inst, g.authorizeGroupDM(ctx, inst, policy, creds, msg)
	}
	if policy.allowsChannel(msg.Source.ChatID) {
		return inst, nil
	}
	g.denied(ctx, msg, "channel is not on the installation allowlist")
	return inst, engine.ErrRoomNotAuthorized
}

// authorized is the installation's full test for one person: bound to a Multica
// user who is still a workspace member, and — when the installation names an
// authorization channel — present in it.
//
// The order matters for cost and for privacy. The database answers first, so a
// stranger who was never bound is turned away without Slack being asked
// anything about them.
func (g *roomGuard) authorized(ctx context.Context, inst engine.ResolvedInstallation, policy roomPolicy, creds credentials, slackUserID string) (bool, error) {
	if slackUserID == "" {
		return false, nil
	}
	bound, err := g.identity.authorizedSlackUser(ctx, inst, slackUserID)
	if err != nil || !bound {
		return false, err
	}
	if policy.authChannelID == "" {
		return true, nil
	}
	return g.inAuthChannel(ctx, inst, policy, creds, slackUserID)
}

// inAuthChannel reports whether slackUserID is a member of the installation's
// authorization channel.
func (g *roomGuard) inAuthChannel(ctx context.Context, inst engine.ResolvedInstallation, policy roomPolicy, creds credentials, slackUserID string) (bool, error) {
	if slackUserID == "" {
		return false, nil
	}
	members, err := g.authRoster(ctx, inst, creds, policy.authChannelID)
	if err != nil {
		return false, err
	}
	for _, member := range members {
		if member == slackUserID {
			return true, nil
		}
	}
	return false, nil
}

// authorizeGroupDM allows a multi-party DM only when every non-bot member is
// authorized.
func (g *roomGuard) authorizeGroupDM(ctx context.Context, inst engine.ResolvedInstallation, policy roomPolicy, creds credentials, msg channel.InboundMessage) error {
	members, err := g.humanMembers(ctx, inst, creds, msg.Source.ChatID)
	if err != nil {
		// An unknown roster is not an empty one. Slack refusing the call (a
		// missing mpim:read scope is the usual cause) must not open the room.
		g.denied(ctx, msg, "conversation roster unavailable")
		return engine.ErrRoomNotAuthorized
	}
	if len(members) == 0 {
		g.denied(ctx, msg, "conversation roster is empty")
		return engine.ErrRoomNotAuthorized
	}
	for _, member := range members {
		authorized, err := g.authorized(ctx, inst, policy, creds, member)
		if err != nil {
			g.denied(ctx, msg, "membership lookup failed")
			return engine.ErrRoomNotAuthorized
		}
		if !authorized {
			// The ROOM is the problem here even though a person triggered it:
			// the speaker may be perfectly entitled to use the bot, just not in
			// front of whoever else is in this group DM.
			g.denied(ctx, msg, "conversation contains an unauthorized member")
			return engine.ErrRoomNotAuthorized
		}
	}
	return nil
}

// authRoster returns the authorization channel's membership. On a Slack OUTAGE
// it falls back to the last known roster within rosterGrace: that can only
// prolong somebody's access, never create it, and it keeps a bad minute at
// Slack from silencing the bot for everybody. Past the grace window it gives up
// and the caller denies.
//
// A channel that turns out not to be private, or that the bot has been removed
// from, is a different kind of answer — a verdict, not a failure to reach one —
// and it drops the cached roster instead of leaning on it. Otherwise converting
// the authorization channel to public would keep authorizing people for another
// fifteen minutes, which is the exact window an attacker would want.
func (g *roomGuard) authRoster(ctx context.Context, inst engine.ResolvedInstallation, creds credentials, channelID string) ([]string, error) {
	key := rosterKey{installationID: util.UUIDToString(inst.ID), chatID: channelID, kind: rosterAll}
	if members, ok := g.cache.get(key); ok {
		return members, nil
	}
	members, err := g.newRoster(creds).PrivateChannelMembers(ctx, channelID)
	if err != nil {
		if errors.Is(err, ErrAuthChannelNotPrivate) || errors.Is(err, ErrAuthChannelNotJoined) {
			g.cache.drop(key)
			return nil, err
		}
		stale, ok := g.cache.getStale(key, rosterGrace)
		if !ok {
			return nil, err
		}
		g.logger.WarnContext(ctx, "slack room guard: serving a stale authorization roster",
			"slack_channel_id", channelID, "error", err)
		return stale, nil
	}
	g.cache.put(key, members)
	return members, nil
}

// humanMembers returns the cached roster for a conversation, fetching it when
// the entry is missing or stale. Only a successful fetch is cached, and a
// failure is never served from the cache: the members this roster would omit
// are precisely the ones who joined since it was taken.
func (g *roomGuard) humanMembers(ctx context.Context, inst engine.ResolvedInstallation, creds credentials, chatID string) ([]string, error) {
	key := rosterKey{installationID: util.UUIDToString(inst.ID), chatID: chatID, kind: rosterHumans}
	if members, ok := g.cache.get(key); ok {
		return members, nil
	}
	members, err := g.newRoster(creds).HumanMembers(ctx, chatID)
	if err != nil {
		return nil, err
	}
	g.cache.put(key, members)
	return members, nil
}

// denied records one refusal. It logs WHO and WHERE and nothing else: the point
// of the guard is that the bot does not process other people's conversation,
// and writing the message body to a log would undo exactly that. These lines at
// warn are how a deployment notices someone probing.
func (g *roomGuard) denied(ctx context.Context, msg channel.InboundMessage, reason string) {
	g.logger.WarnContext(ctx, "slack room guard: conversation denied",
		"slack_user_id", msg.Source.SenderID,
		"slack_channel_id", msg.Source.ChatID,
		"chat_type", string(msg.Source.ChatType),
		"reason", reason,
	)
}

// ---- roster cache ----

// rosterKind separates the two shapes a roster is cached in. The same channel
// can be read both ways, and serving one where the other was asked for would
// silently drop the bots — harmless for a membership test, wrong for a rule
// that counts everyone in the room.
type rosterKind uint8

const (
	rosterAll rosterKind = iota
	rosterHumans
)

type rosterKey struct {
	installationID string
	chatID         string
	kind           rosterKind
}

type rosterEntry struct {
	members   []string
	expiresAt time.Time
}

// rosterCache memoizes conversation rosters for rosterTTL. It is per-process
// and deliberately unbounded in nothing but time: entries are pruned on write,
// and a Slack workspace has far fewer live conversations than the prune cost.
type rosterCache struct {
	mu      sync.Mutex
	entries map[rosterKey]rosterEntry
	now     func() time.Time
}

func newRosterCache(now func() time.Time) *rosterCache {
	return &rosterCache{entries: map[rosterKey]rosterEntry{}, now: now}
}

func (c *rosterCache) get(key rosterKey) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || !entry.expiresAt.After(c.now()) {
		return nil, false
	}
	return entry.members, true
}

// getStale returns an expired entry that is still within maxAge of its expiry,
// for the caller that would rather serve a slightly old answer than none. It
// never resurrects an entry the prune has already dropped.
func (c *rosterCache) getStale(key rosterKey, maxAge time.Duration) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || !entry.expiresAt.Add(maxAge).After(c.now()) {
		return nil, false
	}
	return entry.members, true
}

// drop forgets an entry outright, for the caller that has learned the cached
// answer must not be used again.
func (c *rosterCache) drop(key rosterKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

func (c *rosterCache) put(key rosterKey, members []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k, entry := range c.entries {
		// Keep expired entries until the grace window closes too, or the
		// stale-on-failure fallback would have nothing to fall back to.
		if !entry.expiresAt.Add(rosterGrace).After(now) {
			delete(c.entries, k)
		}
	}
	c.entries[key] = rosterEntry{members: members, expiresAt: now.Add(rosterTTL)}
}

// ---- Slack-backed roster ----

// membersPageLimit is the conversations.members page size. A multi-party DM
// holds at most nine people, so one page always suffices there; the loop exists
// for allowlisted channels, which the guard does not roster today but would.
const membersPageLimit = 200

// slackRoster reads a conversation's membership through the Web API.
type slackRoster struct {
	api       *slack.Client
	botUserID string
}

// PrivateChannelMembers proves the channel is a private channel the bot has
// joined, then lists it. The proof is not optional and cannot be replaced by
// withholding the channels:read scope: an app that legitimately needs to read
// public channels for other reasons would then silently lose the check.
func (r *slackRoster) PrivateChannelMembers(ctx context.Context, channelID string) ([]string, error) {
	if r.api == nil {
		return nil, errors.New("slack: api client not configured")
	}
	info, err := r.api.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: channelID})
	if err != nil {
		return nil, err
	}
	// is_private is also true for DMs and group DMs, so a channel it is not
	// must be excluded by name rather than inferred from the flag.
	if !info.IsPrivate || info.IsIM || info.IsMpIM {
		return nil, ErrAuthChannelNotPrivate
	}
	if !info.IsMember {
		return nil, ErrAuthChannelNotJoined
	}
	return r.members(ctx, channelID)
}

// members lists every member id, following the cursor to the end. A partial
// read is not returned: a truncated roster would look like an absence.
func (r *slackRoster) members(ctx context.Context, channelID string) ([]string, error) {
	if r.api == nil {
		return nil, errors.New("slack: api client not configured")
	}
	var ids []string
	cursor := ""
	for {
		page, next, err := r.api.GetUsersInConversationContext(ctx, &slack.GetUsersInConversationParameters{
			ChannelID: channelID,
			Cursor:    cursor,
			Limit:     membersPageLimit,
		})
		if err != nil {
			return nil, err
		}
		ids = append(ids, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	return ids, nil
}

// HumanMembers lists the conversation's members and drops the bots. Both calls
// are required to be conclusive: if either fails the roster is unknown, and the
// caller denies.
func (r *slackRoster) HumanMembers(ctx context.Context, channelID string) ([]string, error) {
	if r.api == nil {
		return nil, errors.New("slack: api client not configured")
	}
	ids, err := r.members(ctx, channelID)
	if err != nil {
		return nil, err
	}
	candidates := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || id == r.botUserID {
			continue
		}
		candidates = append(candidates, id)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	users, err := r.api.GetUsersInfoContext(ctx, candidates...)
	if err != nil {
		return nil, err
	}
	if users == nil {
		return nil, errors.New("slack: users.info returned no profiles")
	}
	// Every candidate must come back classified. A profile Slack did not return
	// is a person we cannot rule out, so the roster is incomplete, not shorter.
	classified := make(map[string]bool, len(*users))
	for _, u := range *users {
		classified[u.ID] = u.IsBot || u.ID == "USLACKBOT"
	}
	humans := make([]string, 0, len(candidates))
	for _, id := range candidates {
		isBot, ok := classified[id]
		if !ok {
			return nil, errors.New("slack: users.info omitted a conversation member")
		}
		if isBot {
			continue
		}
		humans = append(humans, id)
	}
	return humans, nil
}
