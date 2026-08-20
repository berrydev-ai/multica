package slack

import (
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
)

// RefusalText is the ONE thing an unauthorized person is told. It is a single
// exported constant so it cannot drift into several near-identical strings,
// each leaking a little more than the last.
//
// What it deliberately does not say: that an allowlist exists, who is on it,
// how to get on it, what the bot does, what project it serves, or what is
// behind it. "You are not authorized" would imply a list and invite a request
// to join it; a contact address would be that request's destination. Terminal
// and boring is the whole design.
const RefusalText = "I'm not set up to take requests."

// WrongPlaceText answers an authorized person standing in an unauthorized room.
// It reads as a property of the location rather than of the person, because
// that is what it is — the same request in a direct message would be served.
const WrongPlaceText = "Not available in this channel."

// MaxRefusalTextRunes caps a custom refusal. It is one line said once to
// somebody who is not getting service; anything longer is a conversation the
// bot is deliberately not having.
const MaxRefusalTextRunes = 280

// NormalizeRefusalText collapses a configured refusal to a single trimmed line,
// or "" for "use the default". Newlines become spaces rather than being
// rejected, so a pasted multi-line value degrades instead of erroring, and the
// result is truncated on a rune boundary.
func NormalizeRefusalText(raw string) string {
	flat := strings.Join(strings.Fields(raw), " ")
	if flat == "" {
		return ""
	}
	if runes := []rune(flat); len(runes) > MaxRefusalTextRunes {
		return strings.TrimSpace(string(runes[:MaxRefusalTextRunes]))
	}
	return flat
}

// refusalCooldown bounds how often one person can make the bot say anything.
// Without it, a refusal is just a slower echo: anyone can make the bot speak on
// demand by messaging it repeatedly, and every unbound message would mint
// another binding token.
const refusalCooldown = 24 * time.Hour

// refusalKey identifies one person at one installation. Two bots in the same
// Slack workspace hold independent cooldowns, which is right: they are separate
// grants, and silence from one should not silence the other.
type refusalKey struct {
	installationID string
	slackUserID    string
}

// refusalLimiter is the in-memory record of who has already been refused.
// Losing it on restart is acceptable and intended: the cost is one extra
// refusal per person, and the alternative is a database write on a path whose
// entire purpose is to do as little work as possible for an unauthorized actor.
type refusalLimiter struct {
	mu    sync.Mutex
	seen  map[refusalKey]time.Time
	now   func() time.Time
	after time.Duration
}

func newRefusalLimiter(now func() time.Time) *refusalLimiter {
	if now == nil {
		now = time.Now
	}
	return &refusalLimiter{seen: map[refusalKey]time.Time{}, now: now, after: refusalCooldown}
}

// allow reports whether this person may be told anything right now, and records
// the decision. It is check-and-set in one step under the lock: two messages
// arriving together must not both win.
func (l *refusalLimiter) allow(installationID pgtype.UUID, slackUserID string) bool {
	if slackUserID == "" {
		return false
	}
	key := refusalKey{installationID: util.UUIDToString(installationID), slackUserID: slackUserID}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if until, ok := l.seen[key]; ok && until.After(now) {
		return false
	}
	for k, until := range l.seen {
		if !until.After(now) {
			delete(l.seen, k)
		}
	}
	l.seen[key] = now.Add(l.after)
	return true
}
