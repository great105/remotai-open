package wsutil

// Private WebSocket close codes (the 4000–4999 range is reserved for
// application use by RFC 6455). They let us propagate WHY a bridged stream
// tore down across the relay → agent → local-PTY hops, so connstat can
// attribute a disconnect precisely instead of recording an opaque
// connection-lost for every drop.
const (
	// CloseClientGone — the far (relay/client) leg of a bridged PTY/screen
	// stream closed before the local leg did. The relay-stream pump on the
	// agent sends this to the local /ws/pty handler so the disconnect is
	// recorded as client-initiated — a phone going to background, a tab
	// closing, the mobile link dropping — rather than a real local or tunnel
	// failure. Only the cloud-bridged path can set it; LAN clients that vanish
	// still surface as connection-lost (no middle pump to detect the order).
	CloseClientGone = 4001
)
