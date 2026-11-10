# Changelog

## v0.5.0

Minion brings terminal sessions together through a Hub and its Workers, or runs locally in Solo mode.

- Open a session from its row to see the live screen and controls. Enter sends a message; Shift+Enter adds a line break. The focused Keys area sends navigation and editing keys to the session.
- Follow messages and reliably captured replies in Conversation. Delivery uncertainty is preserved, and writes are never replayed automatically.
- Configure Worker sharing, invitations, scoped API keys and optional OpenID Connect sign-in. MCP is read-only by default.
- Install the Hub or Worker using the guides in the README. The release provides verified package and image paths.

Windows Worker support is a preview. Reply capture requires a supported agent and tmux 3.8 or later on a suitable terminal screen. The CPace implementation has not received a formal external cryptographic audit. See the security guide for trust and deployment boundaries.
