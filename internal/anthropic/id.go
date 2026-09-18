package anthropic

import (
	"fmt"
	"math/rand/v2"
)

// NewMessageID returns an Anthropic-shaped id for one response, derived from
// the backend's own id when it has one and random otherwise.
//
// Every response needs its own: Claude Code groups a response's messages by
// this id and mis-accounts context when two responses share one. Walking back
// from the newest usage-bearing message, it treats every preceding message
// carrying the same id as part of the same response — user messages in between
// do not stop the walk — so a constant id anchors the walk at the first
// assistant message of the session. The trailing messages are then estimated
// and added to the real usage, inflating /context's message row and the
// auto-compact input by an estimate of the whole conversation. Its cumulative
// session-token counter dedupes on the same id, so that total stops moving
// after the first response instead.
//
// Uniqueness only has to hold within one session, so a random 64-bit value is
// ample where the backend offers nothing to derive from.
func NewMessageID(upstreamID string) string {
	if upstreamID != "" {
		return "msg_" + upstreamID
	}
	return fmt.Sprintf("msg_%016x", rand.Uint64())
}
