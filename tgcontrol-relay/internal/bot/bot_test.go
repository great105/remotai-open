package bot

import (
	"slices"
	"testing"
)

// Telegram сохраняет последний allowed_updates. Этот контракт защищает
// инлайн-кнопки от тихого отключения на стороне Bot API.
func TestRelayAllowedUpdatesIncludeMessagesAndCallbacks(t *testing.T) {
	for _, updateType := range []string{"message", "callback_query"} {
		if !slices.Contains(relayAllowedUpdates, updateType) {
			t.Fatalf("allowed_updates не содержит %q: %v", updateType, relayAllowedUpdates)
		}
	}
}
