// Owner: Story 1.6

// Package identity is the single implementation of Minion identity keys and of
// every textual key format (AD-5, AD-7, AD-8, AD-11). It is part of the shared
// kernel and imports no other Minion package.
//
// Contents:
//
//   - Ed25519 identity keys with private-file persistence (keys.go, perm_*.go)
//   - fingerprints: lowercase RFC 4648 base32 of SHA-256 of the raw public
//     key, 52 characters (fingerprint.go)
//   - purpose-tagged signatures over "minion/v1/<purpose>" 0x00 data (sign.go)
//   - base64url without padding for binary JSON fields (b64.go)
//   - invitation keys mnk_<fingerprint>.<secret> (invite.go)
//   - API keys mn_<base32> (apikey.go)
//   - pairing codes MN-XXXX-XXXX in Crockford base32 (paircode.go)
//   - the Hub key transition record and chain verification (chain.go)
//   - fingerprint words: six words of the BIP39 English word list for the
//     first 66 bits of a fingerprint, and the parser for what a human types
//     (words.go)
//
// Word list: bip39_english.txt is the English word list of BIP-0039, copied
// unchanged from github.com/bitcoin/bips (bip-0039/english.txt, SHA-256
// 2f5eed53a4727b4bf8880d8f3f199efc90e58503646d9ff8eff3a2ed3b24dbda, checked by
// a test). BIP-0039 is published under the MIT licence (its header states
// "License: MIT"; authors Marek Palatinus, Pavol Rusnak, Aaron Voisine and
// Sean Bowe). The list is frozen: changing it would change every fingerprint
// in word form.
//
// Errors are the sentinels ErrInvalidArgument, ErrInviteInvalid,
// ErrPairingCodeInvalid and ErrUnauthenticated, matched with errors.Is. Wire
// and HTTP codes are declared only in internal/proto/codes.go (AD-2); the
// adapter edge maps each sentinel to the proto code with the same meaning.
// Error text never embeds a code string and never echoes secret input.
//
// Secrets never reach logs: *Key, InviteKey, APIKey and PairingCode implement
// slog.LogValuer with a redacted value (NFR1, AD-20).
package identity
