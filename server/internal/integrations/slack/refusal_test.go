package slack

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestRefusalLimiter(t *testing.T) {
	instA := pgtype.UUID{Bytes: [16]byte{0xAA}, Valid: true}
	instB := pgtype.UUID{Bytes: [16]byte{0xBB}, Valid: true}
	now := time.Unix(1_700_000_000, 0)
	newLimiter := func() *refusalLimiter { return newRefusalLimiter(func() time.Time { return now }) }

	t.Run("answers once, then goes quiet", func(t *testing.T) {
		l := newLimiter()
		if !l.allow(instA, "UALICE") {
			t.Fatal("the first refusal must be delivered")
		}
		for i := range 5 {
			if l.allow(instA, "UALICE") {
				t.Fatalf("message %d inside the cooldown must be answered with silence", i+2)
			}
		}
	})

	t.Run("speaks again once the window passes", func(t *testing.T) {
		l := newLimiter()
		l.allow(instA, "UALICE")
		now = now.Add(refusalCooldown + time.Second)
		defer func() { now = now.Add(-(refusalCooldown + time.Second)) }()
		if !l.allow(instA, "UALICE") {
			t.Error("the cooldown must expire, not latch")
		}
	})

	t.Run("one person's cooldown does not silence another", func(t *testing.T) {
		l := newLimiter()
		l.allow(instA, "UALICE")
		if !l.allow(instA, "UBOB") {
			t.Error("the cooldown is per person")
		}
	})

	t.Run("two bots in one Slack workspace hold separate cooldowns", func(t *testing.T) {
		l := newLimiter()
		l.allow(instA, "UALICE")
		if !l.allow(instB, "UALICE") {
			t.Error("installations are separate grants and must not share a cooldown")
		}
	})

	t.Run("an unidentifiable actor is never answered", func(t *testing.T) {
		if newLimiter().allow(instA, "") {
			t.Error("with no user id there is nobody to rate-limit, so there is nobody to answer")
		}
	})
}
