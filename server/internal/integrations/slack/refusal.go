package slack

import (
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
)

// refusalCooldown bounds how often one person can make the bot say anything.
// Without it, anyone can make the bot speak on demand by messaging it
// repeatedly, and every unbound message mints another binding token.
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
