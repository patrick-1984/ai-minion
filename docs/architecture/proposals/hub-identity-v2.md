# Proposal: two-tier Hub identity (root key, certified operational key)

> **Status: Proposal for 0.7.0 — DRAFT, revision 3. Not implemented. Review status: two design review rounds done; the three findings of round 2 are addressed in this revision and need a final review before implementation.** 0.6.2 ships a single Hub identity key (see [docs/security.md](../../security.md)).
> This page describes no shipped behaviour. [ARCHITECTURE-SPINE.md](../ARCHITECTURE-SPINE.md) keeps describing 0.6; it is changed only when this design is implemented (the mapping is in the last section). Every new decision is marked "(design decision)". This is the redesign of ticket T7 after the 0.6.0 attempt was reviewed for two rounds and excluded from that release; everything the reviews confirmed is kept, and the five findings that stayed open are answered in the sections named in the next table. This is revision 2 after the first review of the redesign: the activation, durability and recovery rules are now defined once (section 9), the same procedure serves renewal, offline installation and root rotation, and the scope is cut (see the non-goals). Revision 3 answers the three findings of review round 2: first-certificate bootstrap (section 9.7), supersession of a promoted reservation by renewal and root rotation (section 9.8) and the certificate ratchet of a self-retired identity (section 12.5).

| Open finding of review round 2 | Where answered |
| --- | --- |
| HIGH: same-key renewal extends a stolen operational key | "One rule: every certificate has a new operational key" |
| HIGH: an interrupted offline install cannot resume | "Activation, durability and recovery" (crash tables, recovery) and "Backup and restore" (snapshot rule) |
| HIGH: pairing recovery assumed code idempotency | "Pairing: idempotent per (code, Worker key) and owner re-issue" |
| MEDIUM: root-excluded restore conflicts with the missing-root rule | "Backup and restore" |
| MEDIUM: global serial non-reuse is impossible under rollback | "Serials" |

## 1. Goal and non-goals

Today (0.6.2) a Hub has one Ed25519 identity key, `identity.key`. Workers pin its fingerprint, `mnk_` invitation keys name it, it signs every handshake and every revoke, and a copy of it sits in every backup. Whoever holds that file is the Hub for every Worker, for ever, and a stolen backup is a permanent Hub impersonation that only a new identity and re-enrollment of all Workers repairs.

Goal (owner roadmap F-05): the Worker verifies the Hub through a short-lived certificate, the long-lived key can stay off the Hub, and onboarding stays as simple as it is now: the same `mnk_<fingerprint>.<secret>` invitation, the same `MN-XXXX-XXXX` code, no extra step for a normal user.

Non-goals (design decision): no Worker certificates (a Worker proves its key as today; the owner roadmap sketch for F-05 also mentions them, and this design does not deliver them); no certificate revocation list; no change of the invitation key format; no harden or unharden command (a documented procedure with the Hub stopped); no certificate history table; no status UI block; no change of the CPace construction. **Out of 0.7 (design decision):** zero-touch pairing resume, hardware-backed or remote signers for the root, and a UI or command that lists orphan key files.

## 2. Threat model: what this protects and what it does not

| Event | Result in 0.7 (this design) | Result in 0.6.2 |
| --- | --- | --- |
| The operational key (`op.key`) is stolen (process memory, a backup made without the root, a copy of the data directory of a hardened Hub) | The thief can impersonate the Hub only to a Worker that has not yet seen a later certificate, and only until the `not_after` of the certificate that names the stolen key (90 days by default, at most 400). The operator runs `renew` (section 8), which creates a **new** operational key; every Worker that connects afterwards refuses the old certificate (serial ratchet). The thief gets no new certificate: only the root can sign one. | Permanent. |
| A backup without the root is disclosed (hardened Hub, or `--exclude-root`) | The thief holds **every operational key in the archive**: the current one and, if a reservation was outstanding, the prepared one. Bounded as the operational key, but only if the operator follows the compromise procedure of section 13: a certificate signed for a request that existed before the disclosure would certify the thief's key. | Permanent. |
| A backup that contains `root.key` is disclosed (once decrypted, or if unencrypted) | **Root compromise.** The certificate lifetime does not bound it. Workers must be re-pinned out of band: the Hub gets a new identity and every Worker is reset and enrolled again, as 0.6.2 already documents. | Same. |
| The root is stolen from a simple-mode Hub | Root compromise, as above. A chain transition does not help, because the thief can sign it too. | Same. |
| The root is kept off the Hub (hardened mode) and the Hub is fully compromised | The attacker has the operational key and the database, not the root: the damage is bounded by the certificate as in the first row, and the owner can replace the operational key from the offline root. The attacker still reads whatever the Hub stores of the sessions; that is the existing trust in the Hub. | Not possible. |
| A man in the middle presents its own key | Refused: the root is pinned (or named by the invitation) and the certificate must verify under it. | Same. |
| A Worker clock is wrong | The cut-off moves by the same amount; a Worker whose clock is more than one hour before `not_before` refuses the Hub (`cert_not_yet_valid`). Documented in `docs/security.md`. | Not applicable. |
| A removed Worker is offline when the certificate expires | It receives its revoke at the first successful handshake after renewal; until then it stays paired and refuses the Hub. | Not applicable. |

What the design does **not** do: it does not make the Hub trustworthy for session content (see "Who sees what" in `docs/security.md`), it does not protect against a root compromise, it does not protect a Worker that is offline for the whole remaining life of a stolen certificate, and it does not review the CPace construction (the independent review stated in `docs/security.md` is still pending).

## 3. Key hierarchy and files

- **Root key** (`root.key`, Ed25519, the file format of today's `identity.key`: header `minion-ed25519-seed-v1:` plus the seed, mode 0600). It is the Hub's long-lived identity. Its fingerprint is what Workers pin and what `mnk_` keys and pairing name. It signs only hub certificates (purpose `hub-cert`) and root transition records (purpose `transition`). In hardened mode it is not on the Hub.
- **Operational key** (`op.key`, same format). It signs the handshake (`hs-hub`), the CPace confirmation (`pake-confirm`) and revocations (`revoke`). It always lives on the Hub.
- The two keys differ; a certificate with `root_pub == op_pub` is invalid.
- Fingerprints, encodings and the signing rule are those of 0.6: a fingerprint is the lowercase base32 (no padding) of the SHA-256 of the 32-byte public key, 52 characters; binary JSON fields are base64url without padding; every signature is Ed25519 over `"minion/v1/<purpose>" || 0x00 || data`, applied once by `identity.Sign`. The purposes are `hs-hub`, `hs-worker`, `transition`, `revoke`, `pake-confirm` and the new `hub-cert`; the list stays closed (`internal/identity/sign.go`).
- "The Hub fingerprint" always means the root fingerprint.

Data directory files (design decision for the names; modes 0600 except where noted):

| File | Meaning |
| --- | --- |
| `root.key` | the root (simple mode only) |
| `root.key.next` | the root of an interrupted root rotation (the role of today's `identity.key.next`) |
| `op.key` | the operational key in use |
| `op.key.next` | an operational key that is staged or whose certificate is committed, not yet renamed over `op.key` |
| `op.key.prepared-<op_fp>` | the operational key of the outstanding reservation, named by the 52-character fingerprint of its public key (not by the serial: the key must exist on disk before the serial is allocated) |
| `op.key.orphan-<unix-time>` | a key file that matched nothing; kept for the operator, never deleted by the Hub |
| `data.key`, TLS files | unchanged |

The current certificate is stored in the database only (no `hub.cert` file), so it can never disagree with the serial state.

## 4. One rule: every certificate has a new operational key (design decision)

There is **no same-key renewal**. Every issuance, whether scheduled, manual, direct or offline, generates a new operational key and certifies that key. "Renew" and "rotate the operational key" are one operation, named `renew` (there is no second name).

Consequence: a stolen operational key is bounded by the validity of **its own** certificate. Issuing a later certificate never extends it, because the later certificate names a different key that the thief does not have. The thief cannot use the new certificate (certificates are public, but a handshake needs `sig_hub` by `cert.op_pub`), and a Worker that has seen the new serial refuses the old one.

The price is that every issuance closes all Worker sessions (section 9.6; they must be recycled anyway, because a Worker fixes its deadline at the handshake). With the 90-day default and renewal 14 days before expiry that is one reconnect per Worker about every 76 days.

## 5. Certificate

All integers are big-endian two's complement. No optional field, no padding, no trailing data.

| Offset | Size | Field | Rule |
| --- | --- | --- | --- |
| 0 | 1 | `version` | exactly `0x01` |
| 1 | 32 | `root_pub` | Ed25519 public key of the root |
| 33 | 32 | `op_pub` | Ed25519 public key of the operational key; differs from `root_pub` |
| 65 | 8 | `serial` | unsigned, `1 .. 2^63-1` (top bit clear) |
| 73 | 8 | `not_before` | signed, Unix milliseconds UTC, `>= 0` |
| 81 | 8 | `not_after` | signed, Unix milliseconds UTC |
| 89 | 64 | `signature` | Ed25519 by the root over the payload (offsets 0 to 88) |

- **Payload** = offsets 0 to 88, 89 bytes. **Wire certificate** = payload followed by the signature, **153 bytes**; JSON form base64url without padding, 204 characters.
- **Signature** = `identity.Sign(root, "hub-cert", payload)`, which signs `"minion/v1/hub-cert" || 0x00 || payload`. The prefix is applied once, by `Sign`; the payload does not contain it.
- **Validity.** `d = not_after - not_before` is computed with overflow checking; valid only when `0 < d <= 34,560,000,000` ms (400 days). All arithmetic on these fields (the difference, the `not_before - 1 h` tolerance, the serial increment) is checked: an overflow is a rejection, never a wrap.
- **Identity of a certificate** for the Worker's equal-serial rule is the SHA-256 of the 153 bytes.
- Default validity 90 days (`not_before` = the instant the serial is allocated). `--validity D` (whole days, 1 to 400) applies to **that issuance only**; there is no persistent validity setting. The hourly automatic renewal reuses the validity `d` of the certificate it replaces (derived from the certificate, so nothing else is stored).

**Parse and verify order** (`identity.ParseHubCert`, then `Verify(rootPub)`): (1) length 153; (2) `version == 1`; (3) `root_pub != op_pub`; (4) `serial` in `1..2^63-1`; (5) `not_before >= 0` and `0 < not_after - not_before <= 400 d`, checked; (6) `cert.root_pub` equals the root the caller expects and the signature verifies under it. The first failing step names the reason: `cert_malformed` for 1 to 5, `cert_root_mismatch` or `cert_bad_signature` for 6. `ParseHubCertPayload` applies steps 2 to 5 to the 89-byte payload alone, for the signing tool.

### 5.1 Golden vectors

Keys are Ed25519 seeds: root seed = bytes `00 01 02 ... 1f`, operational seed = bytes `80 81 82 ... 9f`. These vectors were produced with the Go standard library and confirmed by review round 1; the implementing ticket must reproduce them exactly.

- `root_pub` = `03a107bff3ce10be1d70dd18e74bc09967e4d6309ba50d5f1ddc8664125531b8`
- `op_pub` = `cd14b37f956e953194ff7fb73b3d81dcc561d61a7538094b7c3e1a643ee5f3aa`

**Vector `typical`:** `serial = 1731283200000`, `not_before = 1731283200000`, `not_after = 1739059200000` (90 days).

- payload (hex): `0103a107bff3ce10be1d70dd18e74bc09967e4d6309ba50d5f1ddc8664125531b8cd14b37f956e953194ff7fb73b3d81dcc561d61a7538094b7c3e1a643ee5f3aa0000019318852800000001931885280000000194e8018000`
- signature (hex): `56ab66fd00f85671959e42d3ee316115e087df3d8512f3df5f34d3df97184f12b8101278561b9bfab6822f359369bdbb7414c0f59957b236787a70b638d20700`
- wire certificate (base64url): `AQOhB7_zzhC-HXDdGOdLwJln5NYwm6UNXx3chmQSVTG4zRSzf5VulTGU_3-3Oz2B3MVh1hp1OAlLfD4aZD7l86oAAAGTGIUoAAAAAZMYhSgAAAABlOgBgABWq2b9APhWcZWeQtPuMWEV4IffPYUS899fNNPflxhPErgQEnhWG5v6toIvNZNpvbt0FMD1mVeyNnh6cLY40gcA`
- SHA-256 of the 153 bytes: `958750b172672c290adec122358e6639aa506fe2077e5fee58d523678c819e31`

**Vector `extremes`:** `serial = 9223372036854775807` (2^63-1), `not_before = 0`, `not_after = 34560000000` (exactly 400 days).

- payload (hex): `0103a107bff3ce10be1d70dd18e74bc09967e4d6309ba50d5f1ddc8664125531b8cd14b37f956e953194ff7fb73b3d81dcc561d61a7538094b7c3e1a643ee5f3aa7fffffffffffffff0000000000000000000000080befc000`
- signature (hex): `bd82d8750483fffca04874cfa919523350a9b9b7c2965b5cb7006edf5500a1993ad372ba000cc2440c8432acfc04066b537eefa51ba41dcfd614b677be3dab09`
- wire certificate (base64url): `AQOhB7_zzhC-HXDdGOdLwJln5NYwm6UNXx3chmQSVTG4zRSzf5VulTGU_3-3Oz2B3MVh1hp1OAlLfD4aZD7l86p__________wAAAAAAAAAAAAAACAvvwAC9gth1BIP__KBIdM-pGVIzUKm5t8KWW1y3AG7fVQChmTrTcroADMJEDIQyrPwEBmtTfu-lG6Qdz9YUtne-PasJ`
- SHA-256 of the 153 bytes: `71773984ff4998e4589feed80f5ae794203f92dbb44d5ddac340afbc50e52517`

**Test data file** `internal/identity/testdata/hub_cert_vectors.json`: one JSON array; each element has `name`, `root_seed_hex`, `op_seed_hex`, `payload_hex` (178 hex), `signature_hex` (128 hex), `cert_b64url` (204 chars), `cert_sha256_hex`, `expect` (`ok` or `cert_malformed`, `cert_root_mismatch`, `cert_bad_signature`, `cert_not_yet_valid`, `cert_expired`), `now_ms` (for the window check; omitted for structure-only vectors) and, for negatives, a `note` naming the single change. Required negatives, each one change of a positive vector: length 152 and 154; version 0 and 2; `root_pub == op_pub`; serial 0 and 2^63; negative `not_before`; `not_after == not_before`; `not_after < not_before`; validity 400 days plus 1 ms; `not_before = 0` with `not_after = 2^63-1`; one flipped bit in each of root, op, serial, times and signature; a signature made over the payload without the prefix, with another purpose, and by the operational key instead of the root. The encoder, parser and verifier tests all read this one file; a test never regenerates it. Overflow of the checked helper is a unit test, not a vector.

### 5.2 Certificate request and certificate files

These public files (no key inside, each at most 4 KiB, UTF-8 JSON, one object, trailing newline) carry a certificate through offline signing. The tools create them without overwriting an existing file. In both files `format` and `version` must match exactly and **unknown fields are refused** (design decision: one rule for both files).

**Certificate request** (written by the running Hub for a durable reservation):

```json
{
  "format": "minion-hub-cert-request",
  "version": 1,
  "serial": 1731283200000,
  "root_fp": "<52 chars>",
  "op_fp": "<52 chars>",
  "not_before": 1731283200000,
  "not_after": 1739059200000,
  "payload": "<119 chars>"
}
```

- `payload` is the 89-byte payload in base64url (119 characters); it is the only field that is signed or trusted. There is no `kind` field (design decision: there is one issuance operation, so a constant would carry nothing; a future second kind raises `version`). The other fields are informational copies; `sign-cert` decodes `payload`, recomputes them and refuses the request if any differs.
- `sign-cert` also runs `ParseHubCertPayload` and requires `payload.root_pub` to be the public key of the root file it was given. It applies no clock rule: the window is checked by the Hub at `install-cert`.

**Certificate file** (written by `sign-cert`):

```json
{
  "format": "minion-hub-cert",
  "version": 1,
  "cert": "<204 chars>"
}
```

- `install-cert` accepts the certificate only when its first 89 bytes equal the payload of the Hub's outstanding reservation, byte for byte, and the signature verifies under the recorded root. Signing the same request twice gives the same 153 bytes (Ed25519 is deterministic).
- Reference for the `typical` vector: `payload` = `AQOhB7_zzhC-HXDdGOdLwJln5NYwm6UNXx3chmQSVTG4zRSzf5VulTGU_3-3Oz2B3MVh1hp1OAlLfD4aZD7l86oAAAGTGIUoAAAAAZMYhSgAAAABlOgBgAA`, `root_fp` = `kzdvvj2umnduyauf35o36k6kw462mujvra46tn3uqgzovmihocga`, `op_fp` = `kgok5e7jhlibdv4fgvmjiqc63d5icywg4vfnuez3xnw3xezn6v5q`. Test data: `testdata/hub_cert_request_typical.json`, `testdata/hub_cert_file_typical.json`, with negatives (wrong `format`, `version` 2, extra field in either file, a `kind` field, 88- and 90-byte payload, informational field that disagrees, payload for another root, 152- and 154-byte `cert`).

## 6. Serials

- **Allocation.** `serial = max(last_issued + 1, now_ms, floor + 1)`, where `now_ms` is the Hub wall clock in Unix milliseconds and `floor` is the operator's `--serial-floor N` (0 when absent; "the new serial must be greater than N"). The increment is checked; an allocation beyond 2^63-1 fails. Allocation sets `last_issued = serial` in the same transaction that stores the certificate (direct signing) or the reservation (offline signing). A floor above `now_ms + 366 days` is refused with `invalid_argument` unless `--allow-far-floor` is given, so a typing error cannot burn the serial space.
- **The guarantee, scoped (design decision).** *Within one issuance lineage, no serial is allocated twice and no serial is ever issued with two different payloads.* An issuance lineage is one database in which every allocation was committed before use and that was never replaced by an older copy, lost, or forked into two Hubs that both issue. A wall-clock rollback alone cannot repeat a serial, because `last_issued` is durable and the allocation takes the maximum. Uniqueness is lost only when the issuance state itself is **restored from an older copy, lost, or forked** (for example two Hubs started from one backup): the database may then not know serials that Workers have seen, and a new allocation can equal one of them. The design does not try to prevent that; it **detects** it at the Worker and gives the operator a recovery command.
- **Worker memory.** A Worker stores, in its enrollment row and scoped to the pinned root, `max_serial` and `cert_sha256` (SHA-256 of the 153 bytes seen at `max_serial`). It raises `max_serial` and never lowers it. It refuses a certificate with `serial < max_serial` (`cert_serial_too_low`) and one with `serial == max_serial` but a different hash (`cert_serial_conflict`). They reset only when a verified root transition changes the pin (a new root starts a new scope at 0). A pin from 0.6 has `max_serial = 0` and no hash.
- **Recovery (N1; operator-controlled, no report on the wire).** A refusing Worker ends the handshake after `sig_hub` verified, without sending `sig_worker` and without any frame that names its value, and writes one local log line with the certificate serial, its own `max_serial` and the exact command: `minion hub identity renew --serial-floor N` with `N = max_serial` (add `--prepare` when the root is not on the Hub). The Hub never learns the value from the wire, so a hostile Worker cannot move the serial counter. The operator takes the highest `max_serial` among the refusing Workers and runs the command; Workers reconnect and accept the new certificate.
- **Restore.** With the root present, a restore issues a fresh certificate inside the restore (section 13), `serial = max(restored last_issued + 1, now_ms)`. Without the root the restored Hub starts with the backed-up certificate, and the operator uses the recovery command (in its `--prepare` form) if Workers refuse it.
- Two separate cases, two separate test groups (section 17): an **ordinary restore** (older state, correct clock) and a **collision** (restored or forked issuance state allocates a serial a Worker already holds with different bytes).

## 7. Durable Hub state (forward-only migration)

One forward-only `sqlmigrate` migration (design decision) extends the existing singleton `hub_identity` row (no history table; the history is the audit log). All serials fit SQLite's signed integer.

| Column | Meaning |
| --- | --- |
| `last_issued` INTEGER NOT NULL DEFAULT 0 | the highest serial ever allocated, including reserved ones |
| `current_serial` INTEGER NOT NULL DEFAULT 0, `current_cert` BLOB (NULL or 153 bytes) | the certificate in use ("C") |
| `root_storage` TEXT NOT NULL DEFAULT 'simple' | `simple` or `hardened` |
| `resv_serial` INTEGER, `resv_payload` BLOB (NULL or 89 bytes), `resv_created_at` INTEGER | the single reservation ("R"; the payload names the new operational public key) |

Further columns for pairing recovery are in section 12. The migration creates no certificate and no key; those are written by idempotent Go code after it (section 14). Every change of these columns is one `BEGIN IMMEDIATE` transaction followed by `MakeDurable`; the ordering and failure rules are those of section 9.1, defined once. On the Worker, `hub_enrollment` gains `max_serial` INTEGER NOT NULL DEFAULT 0 and `cert_sha256` BLOB (NULL or 32 bytes) in a forward-only migration with an existing-database fixture.

## 8. Commands and process ownership (design decision)

`renew`, `install-cert` and every `--prepare` form are **authenticated administrator operations executed by the running Hub** (catalog actions `hub_identity_renew` and `hub_identity_install_cert`, same authorization and CLI mechanism as the existing admin operation `hub_identity_rotate`). The Hub holds its **rotation mutex** for the whole of every identity change (renew, install-cert, root rotation, the hourly automatic renewal, the storage-mode change at start). Because the process that owns the keys is the one that acts, no file locking against a running Hub is needed. The only commands that run without a Hub are the offline `sign-cert`, the public read `minion hub identity`, and `minion hub backup` / `restore` (restore with the Hub stopped).

| Command | Behaviour |
| --- | --- |
| `minion hub identity` | Offline-capable read. Prints the root fingerprint (as today) and, from the database, the operational key fingerprint, current serial, `not_before`, `not_after` with the days left, `last_issued`, the storage mode and the outstanding reservation if any. It needs no key file, so it works on a hardened Hub. |
| `minion hub identity renew [--validity D] [--serial-floor N] [--allow-far-floor] [--prepare --out REQ [--replace]]` | New operational key and certificate. **Direct form** (simple mode only): the Hub runs the activation procedure of section 9.2 with the root. **`--prepare` form** (the only form in hardened mode): the Hub generates the key, stores it as `op.key.prepared-<op_fp>`, allocates and durably reserves the complete immutable payload, and writes the request file to `--out` (never overwriting an existing file). Nothing is activated. |
| `minion hub identity sign-cert --root FILE --request REQ --out CERT` | **Offline**, needs no data directory and no Hub. Validates the request, requires `payload.root_pub` to be the public key of `FILE`, prints serial, operational fingerprint and validity for the operator to check, signs **exactly the payload in the request**, writes the certificate file. It allocates nothing and has no serial or time logic of its own, so it cannot produce a second certificate for a serial. It never overwrites `--out`. |
| `minion hub identity install-cert --cert CERT` | Parses the file and accepts the certificate only if all hold: the 89-byte payload is **byte-equal to the outstanding reservation's payload**; the root signature verifies under the recorded root; the window is current (`now >= not_before - 1 h` and `now < not_after`); the reservation is not superseded (`resv_serial > current_serial`). Anything else is refused with the failed check named and, for an expired window or a superseded reservation, the hint that a new `--prepare` is needed. A certificate signed for a payload the Hub never reserved is refused whatever its signature says. On success it runs the activation procedure (section 9.2). **Idempotent after success (design decision):** if the file's certificate is byte-equal to `current_cert`, the command reports "already active" and changes nothing. After an unconfirmed commit (9.1, D3) the same command confirms and completes. |
| root rotation | Stays the existing admin operation `hub_identity_rotate` (chain transition). It needs the root: in hardened mode it is refused with `conflict` naming the stopped-Hub procedure. In simple mode it runs the **same activation procedure** (9.2) with a compound new set: a new root, a new operational key and a certificate under the new root, committed in one transaction together with the transition record and the revocation of invitations (as today). It voids any reservation (the root in its payload is stale). |
| `minion hub backup [--exclude-root]`, `minion hub restore` | Section 13. |
| `minion hub worker repair-pairing WORKER` (REST operation `worker_pairing_reissue`) | Section 12.3. |

**Reservation rules.** At most one reservation exists. `--prepare` while one is outstanding with the same options returns the same request again (idempotent, and re-writes `--out` if that file is missing); with different options it is refused with `conflict` unless `--replace` is given. **`--replace` (design decision)** always voids the outstanding reservation, whatever the options, and always generates a **fresh** operational key; it never returns the old request. Its serial stays burned. A reservation is **voided** when a certificate with a higher serial becomes current (any activation, including a root rotation) or when `--replace` is used; voiding disposes of its key file under D4 (section 9.1; for a key already promoted to N see 9.8), writes an audit event and never lowers `last_issued`. Every command is audited (`hub_identity.renew`, `hub_identity.prepare`, `hub_identity.install_cert`, `hub_identity.reservation_void`, `hub_identity.storage_mode`) with the serial and the operational fingerprint. After a disclosure of a backup, the operator must void every reservation that the backup could hold: the procedure is in section 13.

**Expiry and automatic renewal.** In simple mode the Hub checks at start and every hour: when fewer than `min(14 days, d / 3)` remain (`d` = the validity of the current certificate; or the certificate is expired) it performs the direct `renew` with the same validity `d` (same function, same mutex). In hardened mode the check only warns: a log line at warn level once an hour from that margin before expiry (error level after expiry), mentioning an outstanding reservation. While its certificate is expired the Hub refuses Worker connections with HTTP 503 and code `hub_cert_expired` (a new code in `proto/codes.go`) before the upgrade; the web UI and REST API keep working so that the operator can fix it.

## 9. Activation, durability and recovery (one implementation)

Direct renewal, offline installation and root rotation use **the same procedure, the same crash table and the same recovery code**. They differ only in how the new key set is staged. Notation: K = `op.key`, N = `op.key.next`, P = `op.key.prepared-<op_fp>`, RK = `root.key`, RN = `root.key.next`, C = the current certificate, R = the reservation. The restore reissue does not use this procedure (it runs on staged files with the Hub stopped, section 13).

### 9.1 Durability rules (the only definition)

- **D1 Whole files.** A private key file is written to a temporary name, synced, and renamed to its final name; it never exists under its name incomplete.
- **D2 Directory sync before commit.** Every key file that a transaction is about to name is on disk **and its directory entry synced** before that transaction commits: P before the reservation transaction; for the activation transaction N (and RN in a root rotation) staged, or P renamed to N, then the directory synced. The directory is synced again after every later rename (P to N, N to K, RN to RK).
- **D3 Committed but unconfirmed** (the branch of `rotate.go` near lines 270 and 446). Every transaction ends with `MakeDurable`. If it returns `ErrCommitNotDurable` (a checkpoint failure, or a request canceled between COMMIT and the checkpoint), the commit may or may not survive a power loss. The operation retries the confirmation once, detached from the request context. Until a confirmation succeeds nothing may use the new state: no handshake, no in-memory swap, no rename over K or RK; N and RN stay where they are, the Hub keeps its previous pair, records the unconfirmed activation in memory, and refuses every other identity change with `conflict` ("repeat the same command to confirm"). The same command (or the next start) confirms and completes it.
- **D4 Preservation.** A key file whose public key equals `C.op_pub` or `R.op_pub` (for roots: the recorded root) is never deleted, overwritten or moved aside, under any of the names K, N, P, RK, RN; it only moves forward (P to N to K, RN to RK). A key file that matches **neither** C nor R is moved to `op.key.orphan-<unix-time>` and logged, never deleted; the Hub never deletes an orphan. Voiding a reservation whose key is still at P deletes P (it was never certified); if the key was already promoted to N, it becomes an orphan.
- **D5 Failed rename after a durable commit.** The Hub keeps serving from its in-memory (key, certificate) pair, logs an error naming the files, and refuses every further identity change with `conflict` until a retry of the rename succeeds. It never writes another N.

### 9.2 The activation procedure

Under the mutex:

1. **Recover** any pending state with the rules of 9.5; if something is unresolved, refuse with `conflict` naming the files.
   **1a. Supersede** an outstanding reservation that this procedure does not itself satisfy (direct renewal and root rotation; section 9.8). `install-cert` skips this step because it satisfies the reservation.
2. **Stage** the new keys durably (D1, D2): direct form, a fresh key to N (refused with `conflict` if N exists after steps 1 and 1a); install-cert, the reserved key found by rule A and promoted P to N; root rotation, a fresh root to RN **and** a fresh operational key to N, both staged and the directory synced before step 4.
3. **Certificate:** direct form and root rotation allocate the serial and sign with the root; install-cert uses the certificate it validated.
4. **Transaction:** store the certificate as `current_cert`/`current_serial`; clear a reservation it satisfies or void a lower one; for a root rotation also the transition record, the new identity row and the revoked invitations; commit; `MakeDurable` (D3). A certificate is served only after this step returned success.
5. **Swap** the (operational key, certificate, root) set in memory as one value and increment the activation generation (9.6).
6. **Rename** RN over RK (root rotation), N over K; sync the directory (D2, D5).
7. **Close** all registered Worker sessions (9.6).

### 9.3 Offline staging states

| State | Durable facts | Entered by |
| --- | --- | --- |
| **S0 staged key** | P exists (synced, directory synced), no reservation names its public key | `renew --prepare` wrote P, before the reservation transaction |
| **S1 reserved** | R exists and its `op_pub` is the public key of P | the reservation transaction committed and confirmed (the request file is written afterwards and is not part of the state) |
| **S2 key promoted** | R exists, the key is at N (P renamed to N, directory synced), C unchanged | `install-cert` step 2 |
| **S3 certificate installed** | C is the new certificate (committed; confirmed or awaiting confirmation), R cleared, N holds its key, K the old key | step 4 |
| **S4 activated** | K holds the new key, no N, in-memory pair swapped, generation incremented, sessions closed | steps 5 to 7 |

### 9.4 Crash points

Recovery runs before any listener opens, and again at the start of every identity change under the mutex. "Power loss" means the database may be older than the files (a commit that was not yet durable can vanish); D2 and D3 exclude the opposite case (files older than a durable database).

**Offline form and install:**

| # | Crash point | Durable facts | Recovery | Operator-visible result |
| --- | --- | --- | --- | --- |
| C1 | after P is written and synced, before the reservation commits | S0 | P is named by no reservation: remove it | `--prepare` again |
| C2 | after the reservation commits, before the request file is written | S1 | keep everything | `--prepare` again returns the same request and writes `--out` |
| C3 | while the request waits for signing (any number of restarts) | S1 | keep P while R is outstanding | `install-cert` later |
| C4 | `install-cert` validated, before P is renamed | S1 | none needed | repeat `install-cert` |
| C5 | after P renamed to N, before the activation transaction commits, or the commit is lost to power loss | S2 | N matches `R.op_pub`: keep it | repeat `install-cert`: it finds the key at N (rule A) |
| C6 | transaction committed (visible), `MakeDurable` not returned or failed | S3 | N matches `C.op_pub`: confirm durability first; if that fails the Hub refuses to start and keeps N; then rename N over K | repeat `install-cert` confirms and completes, or reports "already active" |
| C7 | commit confirmed, before the rename of N over K | S3 | N matches `C.op_pub`: rename N over K, then serve | repeat `install-cert` reports "already active" |
| C8 | after the rename of N over K | S4 | none | none |
| C9 | after the swap, before sessions are closed | S4 in memory | the process is gone, so no session exists | none |

**Supersession of S2 (section 9.8; direct renewal and root rotation while the reserved key is at N):**

| # | Crash point | Durable facts | Recovery | Operator-visible result |
| --- | --- | --- | --- | --- |
| SS1 | before the void transaction commits, or the commit is lost to power loss | S2 | N matches `R.op_pub`: keep it | repeat the renewal; it supersedes again |
| SS2 | void committed, not confirmed | database may show R or not | D3: nothing new is used; N is untouched until confirmation | repeat the same command to confirm |
| SS3 | void confirmed, N not yet moved | R cleared, `last_issued` unchanged, N matches nothing | rule B moves N to `op.key.orphan-<unix-time>` | repeat the renewal (or the start completes the orphaning) |
| SS4 | N moved to the orphan name, replacement not staged | R cleared, no N | none needed | repeat the renewal |
| SS5 | replacement staged, activation not committed | as C5 of the direct form | N matches neither C nor R: orphan it | repeat the renewal |

**Direct form:** the same rows C5 to C9 with "the key at N" staged by step 2; a crash after N is staged and before the commit (or a commit lost to power loss) leaves N matching neither C nor R, so rule B moves it to `op.key.orphan-<unix-time>`; the certificate was never used.

**Root rotation (compound; two private keys):**

| # | Crash point | Durable facts | Recovery |
| --- | --- | --- | --- |
| X1 | RN and N staged (synced), transaction not committed or lost | database records the old root; RN matches nothing, N matches nothing | RN is removed by the existing rotation recovery (it never signed anything); N becomes an orphan |
| X2 | committed, not confirmed | database may be old or new | as C6 for both files; the Hub keeps its previous identity until D3 confirms |
| X3 | committed and confirmed, neither file renamed | RN matches the recorded root, N matches `C.op_pub` | rename RN over RK and N over K, each decided independently |
| X4 | RK renamed, N not (or the reverse) | one pending file left | complete the pending one; the decision for each file looks only at its own match |
| X5 | both renamed | complete | none |

### 9.5 Recovery rules

- **A. Key lookup (design decision).** `install-cert` accepts a reservation-matching key found at either name, P or N, and continues from there. If both exist they must be byte-equal, otherwise it refuses with `conflict` and names both files.
- **B. Pending N at start** (D4 applies). Public key equals `C.op_pub`: confirm durability (D3), then rename over K. Equals `R.op_pub`: leave it. Matches nothing: orphan it. Afterwards K must equal `C.op_pub`, or the Hub refuses to start and names the files.
- **C. P at start.** Kept while its public key equals `R.op_pub`; otherwise removed (never reserved or voided). Never removed while the reservation is outstanding.
- **D. Root files.** Of RK and RN the one whose public key equals the recorded root is the root; an RN that matches is renamed over RK after the confirmation of D3; an RN that does not match is removed (existing rotation recovery). This is the same choice rule the backup uses (section 13).

- **E. Supersession (design decision).** A key at N that matches `R.op_pub` is kept only while R is outstanding. Once the void of 9.8 is durable, the same key matches neither C nor R, so rule B orphans it. Rule E adds no new file action; it states that the void transaction, not the file move, is the commit point.

### 9.6 Activation fence (design decision)

A certificate becomes current in the one procedure above. The **activation generation** is a 64-bit counter in the Hub process starting at 1 that rises at step 5 of every activation: renew, install-cert and root rotation. **A root rotation is not an exception: it swaps the full snapshot (root, operational key, certificate), increments the generation, and closes all sessions.** A Worker handshake takes the (key, certificate) pair and the generation **once, together**, at admission. Registration in the Hub's registry compares that generation with the current one under the registry lock; if it differs, the connection is closed and the Worker retries with backoff. Since step 5 precedes step 7, a handshake that stalled across the swap either registered before step 5 (and step 7 closes it) or fails at registration; no session survives with the old signer. After a root rotation the Workers reconnect and follow the chain (section 11.2).

### 9.7 Bootstrap: the first certificate (design decision)

The first certificate has no previous certificate C and no previous operational key K. The recovery of 9.5 assumes both, so bootstrap has its own states. The rule that decides which state a Hub is in is the one of released `internal/hub/workers/identity.go` (near line 120): a key file that is missing while the database records the identity is **never regenerated**; a key file and a record are created together only when **neither** exists.

| State | Durable facts | Start does |
| --- | --- | --- |
| **B0 pristine** | no `hub_identity` row; none of `root.key`, `root.key.next`, `identity.key`, `identity.key.next`, `op.key`, `op.key.next`, `op.key.prepared-*` | generate the root with the exclusive create of `createKeyFile` (a lost race adopts the winner's file), write the row (`pub`, `fingerprint`, `created_at`, `root_storage = 'simple'`), then continue as B1 |
| **B1 identity without certificate** | row exists, `current_cert` NULL and `current_serial` 0, `root.key` present (or `identity.key` of a 0.6 identity, renamed by section 14) and its public key equals the recorded root | run the activation procedure of 9.2 once: fresh key staged at N, serial `max(last_issued + 1, now_ms)`, 90 days, transaction stores C, rename N to K (K is absent; a stray K that matches nothing is moved to `op.key.orphan-<unix-time>` first) |
| **B2 established** | row exists with `current_cert` set | normal recovery of 9.5; **fail closed**: a missing `root.key` (simple mode) or a missing `op.key` that no `op.key.next` can complete refuses to start, names the files and generates nothing |

A row without a certificate whose root file is missing is **not** B0: it is the fail-closed case of `identity.go` (restore the file from a backup). A hardened database without a certificate is refused, because a hardened Hub cannot issue; the operator uses the stopped-Hub procedure of section 10 and then `--prepare`. Hub-wide, a pristine Hub and a migrated 0.6 Hub do the same thing after B0: no listener opens before C exists.

**Crash points of the first issuance.**

| # | Crash point | Durable facts | Recovery |
| --- | --- | --- | --- |
| BS1 | B0: root file written, row not yet written | key without a record | the existing rule of `identity.go`: record the key |
| BS2 | row written, nothing else | B1 | issue the first certificate |
| BS3 | N staged (synced, directory synced), transaction not committed or lost | B1, N matches nothing | orphan N, stage a new one |
| BS4 | committed, not confirmed | database may show B1 or B2 | D3: no listener opens; confirm, then proceed as BS5; if confirmation keeps failing the Hub refuses to start and keeps N |
| BS5 | committed and confirmed, N not renamed | B2, N matches `C.op_pub`, K absent | rename N to K, then serve (rule B, K absent counts as "pending" only while N matches C) |
| BS6 | N renamed | B2 complete | none |

After a power loss that loses the commit but keeps N, the state is BS3. The Hub never serves a Worker before B2 is complete and confirmed; the registry accepts no connection while `current_cert` is NULL.

### 9.8 Supersession of a promoted reservation (design decision)

State S2 (reserved key promoted to N, certificate not installed) would otherwise block direct renewal (step 2 refuses when N exists) and root rotation (it stages a fresh N). **Chosen: void the reservation durably first, orphan the promoted key, then stage the replacement.** Separate staging names were considered and rejected: a second pending name would double the crash table and break the rule that exactly one of N and RN is pending per key.

Under the rotation mutex, step 1a of 9.2:

1. If R is outstanding and a direct renewal or a root rotation is requested, run **one `BEGIN IMMEDIATE` transaction** that clears `resv_serial`, `resv_payload` and `resv_created_at` and leaves `last_issued` unchanged, so the serial of R stays burned and `install-cert` for it is refused as superseded. `MakeDurable` follows (D3). Write the audit event `hub_identity.reservation_void` with the reason `superseded_by_renew` or `superseded_by_root_rotation`.
2. After the confirmed commit, a key at N that matched `R.op_pub` matches neither C nor R: move it to `op.key.orphan-<unix-time>` (D4), sync the directory. A key still at P is deleted (it was never certified).
3. Continue with step 2 of 9.2 (stage the replacement at N, and at RN in a root rotation).

**Failure and recovery.** If the void transaction fails or is unconfirmed, nothing else happens: N stays, the Hub refuses other identity changes with `conflict` and the same command repeats the supersession (D3). If the orphan rename fails after a durable void, the Hub keeps its previous pair, refuses identity changes with `conflict` naming the files (D5) and retries on the next command or start; rule B performs the same move at start. A crash at any point is covered by SS1 to SS5 of 9.4. The root rotation already voids R in its transaction; step 1a makes the order explicit so that its fresh N never meets the promoted one.

## 10. Root storage

- **Simple (default).** `root.key` is in the data directory, mode 0600. The Hub signs and renews its own certificates. A simple Hub whose `root.key` (and the `root.key.next` that its recorded identity needs) is missing refuses to start; no new key is ever generated.
- **Hardened.** The root is absent from the Hub; `root_storage` is `hardened` in the database. Certificates are reserved by the Hub, signed elsewhere with `sign-cert` and activated with `install-cert`.
- **Fail closed on a residual root.** At start, before anything is served, a hardened Hub that finds any of `root.key`, `root.key.next`, `identity.key`, `identity.key.next` in its data directory refuses to start and names the files and the two ways out. It does not use, delete or move them.
- **No harden or unharden command.** The switch is a documented procedure with the Hub **stopped**: (1) take an encrypted backup; (2) copy `root.key` to where the root will live and compare byte for byte (`cmp`); (3) complete or discard any `root.key.next`; (4) remove `root.key` from the data directory; (5) start once with `MINION_ROOT_STORAGE=hardened`. The return is the reverse: place `root.key` in the data directory, start once with `MINION_ROOT_STORAGE=simple`.
- **`MINION_ROOT_STORAGE` semantics (design decision; complete).** The variable is read at every start and compared with the database value:

| Variable | Database | Result |
| --- | --- | --- |
| absent | any | use the database value (the residual-root rule applies to `hardened`; `simple` needs the root file) |
| equal to the database value | same | nothing changes |
| `hardened` | `simple` | switch: allowed only if no root-bearing file is present; records `hardened` |
| `simple` | `hardened` | return: allowed only if `root.key` is present, its public key is the recorded root, and no other root-bearing name is present; records `simple` |
| any other value | any | refuse to start |

Every recorded change is audited (`hub_identity.storage_mode`) and happens before anything is served; any check that fails refuses to start and changes nothing.

## 11. Protocol 3.0, handshake and expiry

### 11.1 Version (design decision)

The wire protocol becomes **3.0**. The handshake changes meaning (the Hub is authenticated through a certified key, not through the pinned key itself), which is a major change under the AD-4 rule. A 0.7 Hub and its Workers **upgrade together**; negotiation accepts only 3.x; there is no 2.x fallback and no downgrade, and a 2.0 Worker gets `proto_incompatible` and keeps retrying with backoff until it is upgraded. The signed release manifest carries `protocol_range` `3.0..3.0`. A minor bump (2.1) negotiated with 2.0 Workers was considered and rejected: a 2.0 Worker would have to accept a connection whose Hub signs with a key that its pin does not name, which is exactly the downgrade this design closes. Every field that 3.0 adds to the handshake is mandatory; a missing or malformed value is rejected (`invalid_argument`), nothing is defaulted. The transcript label `minion-hs-v1` stays: the protocol version in the hello, which is hashed, already separates the generations.

### 11.2 Frames

1. **Worker `hello`**, unchanged in its fields: version range, ops and features, `worker_pub`, ephemeral X25519 `eph_w`, a 32-byte nonce, `mode` (`connect`, `enroll_invite`, `pair_code`), its name and, in `pair_code` mode, the public pairing id.
2. **Hub `hello`** `{type, version, hub_pub, hub_cert, eph_h, nonce, chain?}`: `version` is `3.0`; `hub_pub` is the **root** public key and must equal `cert.root_pub`; `hub_cert` is the 153-byte certificate in base64url, mandatory in every mode; `chain` only in `connect` mode. The Hub sends it only after admission and negotiation succeeded, and only with a certificate whose `not_after` is in the future (else `hub_cert_expired`).
3. **`transcript_hash`** = SHA-256 of `"minion-hs-v1"`, then the length-prefixed Worker hello as sent, then the same for the Hub hello as sent. The certificate is in the Hub hello, so it is bound into the transcript before any PAKE and any signature.
4. **The Worker validates the Hub hello in this order** and stops at the first failure with nothing written to disk (steps a to d are the same in all modes, e differs) (N2):
   - a. *Shape*: lengths of `hub_pub`, `eph_h`, `nonce` (32 each); `version` in range; `hub_cert` present and 153 bytes; a chain only in `connect`.
   - b. *Root*: `connect`: resolve the chain from the stored pin into a **temporary candidate root** (`VerifyChain`; the candidate is the pin when the chain is empty), `hub_pub` must equal it (`ErrPinMismatch`). `enroll_invite`: the fingerprint of `hub_pub` must equal the invitation's (the root, never the operational key). `pair_code`: the root is only a claim until pairing completed and is not pinned; it is the candidate for the next checks, **and if a `pair_resume` record exists (section 12.2) it must equal the root stored in that record** (`resume_root_mismatch`: the Worker closes before `sig_worker`, keeps the record and the code).
   - c. *Certificate*: structural validation, `cert.root_pub == hub_pub == candidate`, signature under the candidate.
   - d. *Window*: the Worker reads wall clock `now_ms` and, back to back, the monotonic clock `mono0`. It accepts iff `not_before - 3,600,000 <= now_ms <= not_after` (checked; one hour before `not_before`, nothing after `not_after`). A certificate accepted at exactly `now_ms == not_after` admits no operation (its deadline is `mono0` and the `>=` rule closes it).
   - e. *Serial scope (verdict deferred)*: `connect`: if the candidate equals the stored root, compare with `max_serial` and `cert_sha256` (`cert_serial_too_low`, `cert_serial_conflict`); for a different, verified root the scope is new and `max_serial` counts as 0. `enroll_invite`: no scope; any valid serial is the first. `pair_code` with a `pair_resume` record: the record's serial and hash are the scope, with the same two verdicts; without a record any valid serial is the first. The Worker keeps the verdict and continues.
5. **`sig_hub`**: purpose `hs-hub` over `transcript_hash`, signed with the **operational key**; the Worker verifies with `cert.op_pub`, never with `hub_pub`. In `pair_code` mode it comes after the CPace exchange. If `sig_hub` is invalid: `hub_sig_invalid`, close, verdict dropped. If valid and a verdict is pending: close **without `sig_worker`**, report locally with the command of section 6; nothing is written.
6. **`sig_worker`** (purpose `hs-worker`) only now, and only if no verdict is pending. In `pair_code` mode the `pair_resume` record is durable before this frame is sent (section 12.2).
7. **Key schedule** unchanged: HKDF-SHA256 over X25519(eph_w, eph_h), `transcript_hash` as salt, plus the CPace output in `pair_code` mode; two directional ChaCha20-Poly1305 keys.
8. **Commit (Worker)**, one SQLite transaction under the enrollment-generation guard, by mode:
   - `connect`: after step 7 and **before any operation is admitted**: the pin (root and chain position, if changed), `max_serial = max(max_serial, cert.serial)` and `cert_sha256` for the highest serial (a new root resets both first). A commit is needed also when only the serial rose.
   - `enroll_invite` and `pair_code`: the enrollment is **uncommitted until a valid `welcome` arrived inside the channel**. Until then the candidate root, chain position 0, serial and certificate hash are in memory (and, for `pair_code`, in the `pair_resume` record, which `connect` mode ignores: it is not an enrollment and not a pin). On `welcome`, in one transaction: the enrollment, the root as pin, chain position 0, `max_serial` (the higher of the record's and this session's), `cert_sha256` of the highest, and the deletion of the `pair_resume` record; success is reported after the commit. Anything earlier (rejected invitation, wrong or expired code, error frame, closed connection, crash) leaves no pin, root, serial or enrollment. Recovery of a Hub-committed enrollment whose Worker commit did not happen is section 12.
9. Everything else, including the invitation secret, `welcome` and revoke, travels inside the channel; the invitation secret is sent only after step 7.

**Precedence** when several conditions hold, the earliest wins: shape (`cert_missing`/`cert_malformed`), root (`cert_root_mismatch`, `resume_root_mismatch`), signature (`cert_bad_signature`), window (`cert_not_yet_valid`, `cert_expired`), in `pair_code` the CPace tag (`pake_confirm_invalid`) and confirmation signature (`pake_sig_invalid`), `hub_sig_invalid`, then `cert_serial_too_low` / `cert_serial_conflict`. There is no code on the wire for these reasons; the Worker closes the connection and records the reason in its status and log (transport error codes: `invalid_argument` for `cert_missing` and `cert_malformed`, `pairing_code_invalid` for `pake_confirm_invalid`, `unauthenticated` for all others). Only `hub_cert_expired` (HTTP 503 with `Retry-After`) is a new answer on the wire. There are **no in-band floor reports** of any kind.

### 11.3 Worker-enforced expiry (N3)

- At step 4d the Worker records `expiry_wall = not_after` and `deadline_mono = mono0 + (not_after - now_ms)` for this connection. No allowance is added after `not_after`.
- The session is over when `now_wall > expiry_wall` **or** `mono_now >= deadline_mono` (equal counts as expired). Both clocks are read at each of: a monotonic timer set to the deadline and every keepalive tick (30 s); when a stream is accepted, either direction; **after the request is decoded and immediately before the operation is admitted** (op admission recheck); and immediately before a `revoke` is applied (a revoke at or after the cut-off is not applied, the session closes, the pairing stays). A wall clock moved backwards cannot extend the session beyond the monotonic deadline; after a sleep, the first check sees the wall clock.
- In-flight work: on expiry no new operation is admitted; a read in flight fails with `connection_lost`; a write that already started completes locally and the Hub reports its outcome `unknown`, never replayed. The Hub also closes every session at `not_after`.
- Expiry never touches the enrollment or the pin. The connector's injected clock provides `Wall()` and `Mono()` so the cases are testable.

## 12. Enrollment, pairing and recovery

### 12.1 Invitations

The format is unchanged: `mnk_<root fingerprint>.<secret>`; the secret is 32 random bytes in lowercase base32. Keys created by a 0.6 Hub stay valid because `identity.key` becomes `root.key` unchanged. The Worker sends the secret only after the certificate, window and `sig_hub` are verified, and commits only after `welcome` (step 8). Enrollment is already idempotent per `worker_pub` (the Hub's `EnrollWithInvitation` returns the existing Worker for the same key without consuming another use), so a crash between `welcome` and the Worker's commit is repaired by repeating the enrollment. Renewal never revokes invitations; a root rotation does.

### 12.2 Pairing: idempotent per (pairing code, Worker key) (design decision)

Today (`internal/hub/workers/pairing.go`) a Hub commit that the Worker never records leaves the Worker stuck: `Claim` refuses a used code (about line 476, `PairingCodeUsed`), `loadRow` refuses a used pairing id (about line 397), `create` answers `already_paired` for a known Worker key (about line 529), and `CompletePairing` answers `ErrPairingCodeUsed` (about line 560). The Worker also retires its code after any exposed connection that did not pair (`classify` in `internal/worker/agent/pair.go`, about line 113), so after a lost `welcome` or a crash it would show a different code.

**Hub side: the pairing commit is recorded.** The existing table `used_pairing_codes` (digest, `used_at`) is extended by the forward-only migration with `pairing_id` TEXT, `worker_pub` BLOB, `worker_id` TEXT, `owner_id` TEXT, `expires_at` INTEGER (the code's expiry, copied from the pending row) and `resume_attempts` INTEGER NOT NULL DEFAULT 0. `CompletePairing` writes them in the same transaction that inserts the Worker. Rows from before the migration have `expires_at` NULL and never resume (today's behaviour). **The stored digest is weak protection:** a pairing code has few unknown bits (about 20), so anyone who can read the database can recover it from the digest by guessing. Resume therefore never relies on the digest's secrecy; it relies on the same Worker key, the commit's owner, the expiry and the attempt counter below.

**Worker side: the code and the authenticated Hub identity survive (design decision).** The Pairer persists a record in the Worker state (new table `pair_resume`, forward-only migration; the state directory is private to the Worker's user and the code is already printed on its console): `{code, pairing_id, expires_at, root_pub, chain_position 0, serial, cert_sha256}`, where the last four are the **authenticated candidate root and its certificate ratchet** from this very handshake. The record is written, with a durable commit flushed to disk, at the moment the Hub is authenticated (CPace confirmation tag and signature, certificate and `sig_hub` verified) and **before** the Worker sends `sig_worker`. The Hub cannot create the Worker before it has received `sig_worker`, so the record always precedes any Hub commit. The record is deleted in the same transaction as the enrollment commit, at expiry, and by `minion worker reset`. **Abandoning the identity retires the old code:** a Worker that is reset, or whose record has expired, deletes the record and shows a new code; until then it refuses any Hub whose root differs from the record (section 11.2 step 4b) and keeps the record. This closes identity substitution on resume: a peer that learned the code could pass CPace, but cannot present another root, and cannot lower the certificate serial the record holds.

`classify` changes: a connection that ends after the Hub was authenticated and before `welcome` keeps the code and reuses it until expiry. This is safe against the interceptor concern that motivated the retirement (one guess per connection), because only a peer that knows the code can produce the Hub confirmation: the rule never keeps a code after an unauthenticated peer. After a restart the Pairer reads an unexpired record, reuses the code, prints "Resuming pairing: enter the same code again in the Hub (Add worker)", and sends the same pairing id.

**Resume at the Hub.**
1. *Hello.* In `loadRow`, a pairing id whose row is `used` is refused as today **unless** a `used_pairing_codes` row exists for that `pairing_id` with `worker_pub` equal to the hello's key and `expires_at > now`. Then the Worker waits as for a pending code, with a resume ticket and no new pending row.
2. *Claim.* The owner types the same code. In `Claim`, the used-code check moves behind the live-entry lookup: a used code is accepted only if the live entry carries a resume ticket whose digest equals the typed code's digest **and** the claimant is the commit's `owner_id`; otherwise the message is today's "this code was already used". Wrong typed codes are counted in `resume_attempts` (five at most) instead of the pending row.
3. *Fresh CPace.* The handshake runs normally with the typed code (the Hub holds no password, only the digest). Nothing about the first run is reused; the Worker applies the root and serial checks of its record (11.2 steps 4b and 4e).
4. *Create.* After the tombstone check (a removed Worker still gets `revoked`), `create` with a resume ticket loads the Worker row named by `worker_id` and returns it as the existing enrollment: no insert, no new name suffix, no new digest, audit `worker.pair_resume`, and `welcome` carries the current row (including its pending-approval state). Without a ticket, a known Worker key still gets `already_paired`.
5. *Other keys.* A different key with the same code finds no matching commit row: the hello is refused as today. The same key with a **different** code and no repair window (12.3) gets `already_paired`.

Cases covered: **(a) lost welcome** (the Hub committed and sent it, the connection died): the Worker kept its record and the code, resumes as above. **(b) crash after a valid welcome and before the Worker's commit**: the record is on disk, same path. **(c) Hub crash before its commit**: no commit row, the pending row is still pending, the owner claims again as today. **(d) Hub committed, Hub crashed before sending welcome**: same as (a). **(e) a different Hub identity answers the resume**: refused by the Worker, record kept.

### 12.3 After code expiry: owner re-issue (design decision)

If the code lifetime (10 minutes) passes before the retry, the Worker is stuck between the Hub commit and its own: it has no pin, the Hub has a Worker row. Recovery is by the owner in the Hub:

- **Operation** `worker_pairing_reissue` (CLI `minion hub worker repair-pairing WORKER`; Workers page button "Re-issue pairing" shown for a Worker whose `first_connected_at` is empty and whose code has expired). Allowed for the Worker's owner and for administrators. Eligibility: the Worker exists, is not tombstoned, and has **never completed a connect handshake** (new column `workers.first_connected_at`, set when a `connect` handshake registers; backfilled with `created_at` for rows that exist at migration, so no existing Worker is eligible); otherwise `conflict`.
- **Effect:** sets `workers.repair_until = now + 10 minutes` (new column, default 0) and audits `worker.pairing_reissue`. Nothing else changes.
- **Completion:** the Worker, which has no state to keep, simply shows a new code as in any pairing (`classify` renews it at expiry; a user may also rerun `minion worker pair`). The owner (or an administrator) enters the new code. In `create`, when the Worker key is known, `repair_until > now`, `first_connected_at` is empty and the claimant is the row's owner or an administrator, a repair transaction marks the new pending row used, records a new `used_pairing_codes` row (with the commit fields), clears `repair_until`, and returns the existing row; otherwise `already_paired` as today. The Worker keeps the same Worker id, name and approval state.
- **Atomic eligibility (design decision).** Each eligibility check and the write it guards are one transaction with a conditional update: setting `repair_until` is `UPDATE ... WHERE id = ? AND tombstoned = 0 AND first_connected_at IS NULL` and must change exactly one row; the repair in `create` clears `repair_until` with `WHERE repair_until > now AND first_connected_at IS NULL` in the same transaction as the used-row and commit-row writes. A connect handshake that sets `first_connected_at` at the same moment therefore wins or loses cleanly; no check is read in one transaction and acted on in another.
- If the Worker key itself was lost, there is no recovery of that row: the owner removes it and enrolls the new key.
- The Worker's error text for `already_paired` after expiry says: "The Hub already knows this Worker key. Ask the owner to use Re-issue pairing for this Worker."

### 12.4 Revoke signer (AD-9)

A `revoke` is signed with purpose `revoke` over the `transcript_hash` of that very connection by the **operational key whose certificate was presented in that handshake**. The Worker verifies it with the `cert.op_pub` it validated in that handshake and keeps for the session; a revoke signed by the root, a previous operational key or another connection's key is ignored with a warning, nothing is deleted. The Hub takes the signer once per connection (key and certificate as one pair, with the generation of 9.6) into the connection's registry entry; the live revoke and the tombstone answer at reconnect both sign with that entry's key. Hardened mode needs no root for a revoke. A removed Worker that was offline across a renewal is revoked at its first handshake afterwards. Pairing never ends by timer; a Worker wipes its pairing only on a signed revoke. The expiry of the Hub certificate never unpairs a Worker.

### 12.5 Self-retirement carries the certificate ratchet (design decision)

Released code replaces a Worker identity in `internal/worker/agent/pair.go` (near line 815): it adds the old key to the `RetiredStore` and then calls `Wipe`, which deletes the enrollment row. With section 6 the row also holds the root-scoped ratchet (`max_serial`, `cert_sha256`), so a wipe would reset it and a stale certificate (still inside its window) could answer `not_enrolled` on the retirement connection of `internal/worker/agent/pair_retire.go` (near lines 82 and 398) and make the Worker discard the pending revocation.

1. **Carry before wipe.** `RetiredIdentity` and `retiredMeta` gain `max_serial` (uint64) and `cert_sha256` (32 bytes or absent) next to `hub_pub` and `pin_seq`, and `retiredMeta.version` becomes 2. `RetiredStore.Add` writes them from the enrollment row **in the same private metadata file** as the key, durably (D1 and a directory sync), and only then does the caller `Wipe`. An `Add` that fails aborts the replacement before the wipe, as today.
2. **Validate on every retirement connection.** The retirement handshake runs steps a to d of 11.2 with the retired pin: the certificate chains to the pinned root (or follows the chain transition as in 11.2), its window is current, `serial >= max_serial`, and with `serial == max_serial` the hash equals `cert_sha256`. A failure is the same refusal as on a normal connection (`cert_serial_too_low`, `cert_serial_conflict`); the retirement stays pending and nothing is discarded.
3. **Advance durably.** After `sig_hub` verified and **before** the Worker acts on `revoke`, `not_enrolled` or `welcome`, a higher serial (or the first hash at an equal serial) is written to the retired metadata file (temporary file, sync, rename, directory sync). Only a durable write permits the result `already_revoked` or `not_enrolled` to retire the entry. A failed write ends the connection with the entry kept.
4. **Only a validated connection can answer `not_enrolled`.** A `not_enrolled` frame counts only when the handshake above passed; a stale operational certificate cannot produce it.
5. **Migration of existing retired metadata.** Version-1 files have no ratchet. On load they are read as `max_serial = 0` with `ratchet_seeded = false`. The first validated retirement connection seeds the ratchet (step 3) but may only complete through a signed `revoke` (`already_revoked`) or `welcome`; a `not_enrolled` on that first connection leaves the entry pending, and a later validated connection with `serial >= seeded serial` may retire it. The file is rewritten as version 2 at the seed. Residual risk, stated: until the seed, a version-1 entry has no memory of the root's earlier serials.

## 13. Backup and restore

- **Contents (design decision for the list; the archive format and age encryption are the 0.6.x backup work).** Database snapshot (it holds the certificate state and the reservation), `data.key`, the operational key files chosen by the snapshot rule below, TLS material, and the root file **only** when the Hub is in simple mode and `--exclude-root` is not given.
- **Snapshot rule (the only definition; design decision).** The snapshot takes the database copy first, then selects the keys that belong to that copy, each independently: (1) the **root file**, simple mode only: of `root.key` and `root.key.next` the one whose public key equals the root recorded in the copy (the algorithm of `snapshotWithIdentity` in `internal/hub/store/backup.go`, near line 337, extended from `identity.key`); (2) the **current operational key**: the key whose public key equals the copy's `C.op_pub`; (3) the **reserved key**, if the copy has R: the key whose public key equals `R.op_pub`. Operational keys are searched in `op.key`, `op.key.next` and every `op.key.prepared-*`, because the state may have moved on since the copy (P may have become N or K, a root rotation may have renamed files). If any selected key is not found the whole snapshot is repeated, at most five times (`maxSnapshotAttempts`); then the backup fails with an error naming the facts. The archive stores them under normalised names: `root.key`, `op.key`, `op.key.prepared-<op_fp>`. A restore therefore always yields state S1 (with a reservation) or S4 (without one), never a half state, and `install-cert` after restore works from S1. A snapshot taken at C5 restores as S1; taken at C6 or C7 restores as S4 with the new certificate and key.
- **Root-bearing names are excluded unconditionally** when the database says `hardened` or `--exclude-root` is given: `root.key`, `root.key.next` and the legacy `identity.key`, `identity.key.next`, whether or not present.
- **The manifest records the decision (design decision).** `manifest.json` gets `format` 2 and a mandatory boolean `root_excluded` (true when the archive was written without the root, for either reason), plus `root_excluded_reason` (`hardened` or `flag`, empty when false). Format-1 archives (0.6.x) have neither field and are read as `root_excluded = false`; they carry `identity.key`, which a 0.7 restore accepts as the root of a migrating database.
- **Restore is two phases (design decision).** The 0.6.x restore verifies the staged files against the archive manifest (`copyStaged` in `backup.go`, near line 897, compares names, sizes and hashes). Transforming a staged file changes its hash, so the phases are separate:
  1. *Verify the immutable archive.* Decrypt and unpack into a private temporary directory; verify every entry against the manifest; reject unknown names; apply the consistency rules below. The archive, its manifest and the verified files are not modified afterwards.
  2. *Migrate and transform the staged copy* (Hub stopped, still in the private directory): migrate a format-1 database and rename `identity.key` to `root.key`; for a root-excluded archive set `root_storage = 'hardened'`; with the root present, reissue (below); checkpoint and close SQLite so no `-wal` or `-shm` file remains; run the integrity checks; **validate the identity** (the root file's public key is the recorded root; the operational key matches `C.op_pub`; the reserved key matches `R.op_pub`). Then build a **trusted internal install plan**: the list of final names, sizes and SHA-256 values computed now, in this process, from the transformed files. The installation (`installRename` near line 1249, with its rollback) verifies against that plan and never against the archive manifest.
- **Restore rules** (both phases complete before the destination is touched; the consistency checks are unconditional):
  1. `root_excluded = false`: the archive must contain the root file (`root.key`, or for format 1 `identity.key`) whose public key is the database's recorded root. A missing root is rejected with today's error. This is the only case in which a missing root is a rejection.
  2. `root_excluded = true`: the archive must contain **no** root-bearing name, and the restore requires the independent operator intent flag **`--root-excluded`** (design decision). The manifest is plaintext inside the archive and anyone who holds the age recipient key can write one, so it cannot by itself waive the root check; without the flag the restore is rejected with the missing-root error and a hint naming the flag. The archive must carry the operational key for the copy's `C.op_pub` (and the reserved key if the copy has a reservation), else it is refused. `--root-excluded` with an archive that has `root_excluded = false` or contains a root-bearing name is refused too.
  3. An archive with a root-bearing file whose database says `hardened`, or with `root_excluded = false` and a `hardened` database, is refused as inconsistent. A root-bearing file in a `root_excluded = true` archive is refused.
- **Reissue.** With the root present, phase 2 issues a fresh certificate on the staged files (new operational key, `serial = max(restored last_issued + 1, now_ms)`), voids and removes any reservation and its prepared key, and stores the result as state S4. No generation or sessions are involved: the Hub is stopped. Without the root no reissue happens; a reservation stays outstanding, and `install-cert` completes it from the archived prepared key (but see the compromise procedure).
- **Disclosure and compromise procedure** (N4, to be written into `docs/security.md` and the backup guide). A backup that contains `root.key`, once decrypted or if unencrypted, is a **root compromise** when disclosed; the certificate lifetime does not bound it. A backup without the root discloses **every operational key it holds, including a prepared (reserved) one**. The lifetime bound applies only if the operator then replaces all of them: in simple mode run `renew` (it voids any reservation and creates a fresh key); in hardened mode run **`renew --prepare --replace`** (it voids every reservation, burns its serial and creates a fresh key), sign the new request offline and `install-cert` it. **Never install a certificate for a request that was created before the disclosure**, because it certifies a key the thief has. The same applies to a Hub restored from a possibly disclosed archive that comes back in state S1: run `--prepare --replace` before `install-cert`.

## 14. Migration from 0.6.x (design decision)

0.6.2 has `identity.key`, a `hub_identity` row (`pub`, `fingerprint`, `created_at`), the transition chain, and a Worker `hub_enrollment` pin. **The 0.7 Hub and all Workers upgrade together to protocol 3.0** (section 11.1); there is no mixed operation. Nothing needs re-enrollment: pins stay valid and every `mnk_` invitation keeps working, because the root is the old identity key unchanged.

Hub, first 0.7 start, before the first listener opens (Go code after the forward-only SQL migration; idempotent, so a crash at any point is repaired by the next start):
1. Complete or discard a pending `identity.key.next` with the existing rotation recovery; rename it to `root.key.next` if still needed.
2. If `identity.key` exists and `root.key` does not, rename `identity.key` to `root.key` unchanged. If both exist: identical bytes, remove `identity.key`; different bytes, refuse to start and name both files.
3. Do not create `op.key` here: the first certificate must carry a fresh key (section 4), so the key is staged at `op.key.next` by the activation procedure. A stray `op.key` without a certificate is orphaned (9.7).
4. Decide the bootstrap state of 9.7: an identity without a certificate is B1 and gets its first certificate (serial `max(last_issued + 1, now_ms)`, 90 days) through the activation procedure of 9.2; set `root_storage = 'simple'`. A pristine data directory is B0. A recorded identity whose key file is missing is **not** regenerated (fail closed, as released `identity.go`).

Worker, first 0.7 start: forward migration adds `max_serial = 0`, `cert_sha256 = NULL` and the `pair_resume` table. The first successful `connect` against the upgraded Hub accepts the certificate under the pin and commits the serial.

**Upgrade procedure caveat.** 0.7.0 declares protocol range `3.0..3.0` against 0.6.2's `2.0..2.0`. What 0.6.2 actually does: `minion update` contains a guard (`checkProtocol` in `internal/version/update.go`, near line 358, called through `hubRange` in `internal/cli/update.go`, near line 261) that would refuse a protocol-disjoint release, but its source of the connected Hub's range (`SetHubRange`) is **not wired in production**, so without the `--hub-proto-range` flag the 0.6.2 updater performs no protocol check and installs the release. What is intended: the guard refuses once the source is wired (a 0.6.x patch would be needed; open question 3). Either way the order is the same: take an encrypted backup of the 0.6.2 Hub; upgrade the Hub first (package or `bootstrap.sh`); then each Worker. A Worker updated earlier reports `proto_incompatible` and retries with backoff until the Hub is upgraded. There is no database downgrade; the backup is the way back. A restore of a 0.6.x archive into 0.7 is supported (format 1, section 13).

## 15. Windows

`root.key`, `op.key` and their `.next`, `.prepared-*` and `.orphan-*` files follow the existing Windows rules for private files and replace-rename; the states of section 9 are the same. The Windows Worker remains a preview.

## 16. Documentation changes at implementation

`docs/security.md` (identity section, backup disclosure and compromise procedure, clock requirement of Workers, protocol 3.0), `docs/guides/backup-restore.md` (root-excluded archives and `--root-excluded`, hardened procedure, disclosure), `docs/guides/pairing.md` (resume and Re-issue pairing), and ARCHITECTURE-SPINE.md: AD-4 (versioning), AD-5 (identity), AD-6 (handshake), AD-7 (invitations), AD-8 (pairing), AD-9 (revoke), AD-16 (migrations) and the deployment table (backup contents). This proposal is deleted or marked "implemented" in the same change.

## 17. Test obligations

Each item is a named test; crash tests use injected failure points at the boundaries named in section 9.4, and error-injection tests use injected failures of `MakeDurable`, of the directory sync and of each rename.

**Certificate and files.** Golden vectors (positive, negative, extremes) read by encoder, parser and verifier; request and certificate file parsing (malformed JSON, wrong `format` or `version`, wrong-length base64, disagreeing informational field, payload for another root, unknown field in either file, a `kind` field).

**Renewal is rotation.** Every issuance (direct, offline, hourly automatic) produces a different `op_pub`; **a stolen previous operational key cannot authenticate with any newly issued certificate** (the thief signing `sig_hub` with the old key under the new certificate fails `hub_sig_invalid`; presenting the old certificate to a Worker that has seen the new serial fails `cert_serial_too_low`; presenting it to a Worker that has not is accepted only until the old `not_after`, which the test asserts and then asserts refusal one millisecond after); the new certificate does not extend the old key's validity; all sessions close at each issuance. `--validity` applies to one issuance only; the automatic renewal reuses `d` and its margin is `min(14 days, d / 3)`.

**Serials.** Two allocations in one millisecond differ; renewal raises the serial; `last_issued` is never lowered; a clock rollback with a durable database never repeats a serial; a floor above `now + 366 d` is refused without `--allow-far-floor`; property test over random operation sequences with injected crashes: within one database lineage no serial is allocated twice and none with two payloads. **Ordinary restore:** with the root, either every Worker connects after the restore reissue, or a Worker refuses with `cert_serial_too_low` or `cert_serial_conflict`, logs the documented command and the command repairs it (the test accepts both outcomes and asserts the second one fully); without the root the old certificate is kept, a Worker that saw a later serial refuses it with the logged command, and `renew --serial-floor N` (direct and `--prepare`) lets it connect. **Collision (separate):** a restored or forked issuance state (two Hubs started from one backup) allocates a serial equal to one a Worker holds with different bytes; the Worker refuses with `cert_serial_conflict` after a valid `sig_hub`, sends no `sig_worker` and no frame naming `max_serial`, logs the command, stores nothing; the command repairs it. A hostile client cannot change Hub serial state (there is no input).

**Activation, durability and recovery (all three flows).** One test each for C1 to C9 (section 9.4) asserting the durable facts after the crash, the recovery action at start, and that the flow completes afterwards: C1 removes P; C2 `--prepare` returns the same request; C3 survives restarts with R and P intact; C4 repeat install; C5 install-cert finds the key at N (key at the prepared name **and** at `op.key.next`, both accepted; both present and different refused); C6 confirms durability and completes the rename, a repeated `install-cert` reports "already active"; C7 to C9 as the table. Power loss: a database copy from before the commit combined with the files of after it (N matches R: kept; direct form: N orphaned; root rotation: RN removed, N orphaned) and the case that D2 forbids is shown unreachable by an injected failure of the directory sync (the transaction is not started). Error injection: `MakeDurable` fails or the request is canceled between COMMIT and the checkpoint (the Hub keeps its previous pair, serves nothing new, keeps N and RN, refuses other changes with `conflict`, and the same command or the next start confirms and completes; a start with a failing `MakeDurable` refuses and keeps N); a failed rename after a durable commit keeps serving, refuses the next change and succeeds after the retry; a key matching C or R is never deleted or orphaned in any of these runs. Also: install refuses a certificate differing from the reservation in any single byte, one for a payload never reserved, a superseded reservation, an expired window; `sign-cert` signed twice yields identical bytes and refuses a request for another root; `--prepare` twice returns the same request, a different prepare without `--replace` is refused, `--replace` (also with identical options) burns the serial, makes a fresh key and orphans a promoted key; a direct renew voids a lower reservation and deletes P; start never deletes P or N while R is outstanding; a `.next` that matches nothing becomes `op.key.orphan-*`. **Root rotation (compound):** X1 to X5 of section 9.4, each file decided independently; sessions are closed and the generation rises; a stalled handshake that took the old root-and-key set fails at registration.

**Bootstrap (9.7).** A pristine data directory (B0) creates root, row, first certificate and `op.key`, serves only after B2; two initializers race and adopt one root; a migrated 0.6 identity without a certificate (B1) keeps its fingerprint and `mnk_` invitations and gets a certificate with a key different from any stray `op.key`; one test for BS1 to BS6 asserting durable facts, recovery and completion (BS3 orphans N, BS4 refuses to start and keeps N when confirmation fails); **fail closed:** a row with a certificate and a missing `root.key` or `op.key`, and a row without a certificate and a missing root file, refuse to start, name the files, and no key is generated (the distinction of `identity.go` near line 120); a hardened database without a certificate is refused; no Worker connection is accepted while `current_cert` is NULL.

**S2 supersession (9.8).** S2 followed by direct `renew`: the reservation is voided with its serial burned (`last_issued` unchanged), the promoted key becomes `op.key.orphan-*`, a replacement is staged and activated, and `install-cert` for the old certificate is refused as superseded. S2 followed by root rotation: the same void and orphan, the new root and key are staged at RN and N, the transition and revoked invitations commit together, the old reservation cannot be installed. Crash tests SS1 to SS5 for both flows; injected failure of the void commit, of `MakeDurable` and of the orphan rename (previous pair kept, `conflict`, the repeated command completes); a key matching C or R is never deleted in any run; `--prepare --replace` over S2 behaves as 9.8 as well.

**Backup snapshot per state.** A backup taken in each of S0, S1, S2 (key at N), S3 and S4, and one taken while a state moves on between the database copy and the key read (retry path, bounded by five attempts, and the failure message when no key is found); **a root rotation between the database copy and the key read** (the root chosen by public key among `root.key` and `root.key.next`, the operational keys chosen independently, compound crash states X2 to X4 restore to a consistent set); restore of each yields S1 or S4, and from S1 the `install-cert` round trip completes; archive names are normalised.

**Activation fence.** Stalled handshake interleaving (takes its pair and generation, stalls, renew completes and closes sessions, the handshake resumes: registration fails, no session survives with the old signer); a handshake registered before the swap is closed by it; the generation rises on renew, install-cert and root rotation.

**Handshake and expiry.** Frame order and presence for the three modes, certificate bound into the transcript (one flipped certificate byte breaks every later step); the Worker never sends `sig_worker` before `sig_hub` verified, never reveals the invitation secret before step 7, never writes a pin before its commit point; each reason of the table, earliest wins (expired plus too low reports `cert_expired`; bad `sig_hub` plus too low reports `hub_sig_invalid`); window boundaries (`not_before - 1 h` accepted, one millisecond less refused, `now == not_after` accepted and admits nothing, one millisecond more refused, integer extremes); root transitions in the N2 order (chain, candidate, certificate under candidate, serial scope, `sig_hub`, atomic commit before any operation; a `hub_pub` different from `cert.root_pub` refused; a certificate under the old root with a new `hub_pub` refused; a rejected candidate leaves `max_serial` untouched); wrong root in each mode; a malicious Hub that never disconnects is cut at the deadline; wall time past `not_after` while the monotonic clock barely moved; wall clock backwards; monotonic equality counts as expired; an operation decoded before the deadline and admitted after it is not admitted; a revoke at or after the deadline is not applied and the pairing stays; a started write completes and is reported `unknown` while a read fails with `connection_lost`; `hub_cert_expired` before the upgrade; the Hub closes sessions at `not_after`.

**Pairing recovery.** Idempotency per (code, key): (1) lost welcome, then restart or retry by the same Worker with the same code within the lifetime and a fresh CPace, owner re-enters the code: the existing enrollment is returned, no second Worker, no new suffix, one `worker.pair_resume` audit event, the Worker commits; (2) crash after a valid welcome and before the commit, same result, and the `pair_resume` record (code, pairing id, expiry, root, serial, certificate hash) is durable before `sig_worker` was sent (error injection on the record write: no `sig_worker`); (3) Hub crash before its commit: no commit row, normal claim; (4) a different Worker key with the same code: today's refusal; (5) the same key with a different code and no repair window: `already_paired`; (6) a different user typing the code during resume: refused; (7) resume after code expiry: refused; (8) five wrong codes during resume close the resume; (9) a removed Worker during resume: `revoked`; (10) the Worker keeps a code only after the Hub's confirmation and `sig_hub` verified, and still retires it after an unauthenticated failure; (11) **identity substitution:** a resume answered by a Hub with a different root (even one that knows the code and passes CPace) is refused before `sig_worker`, the record and code are kept, and nothing is pinned; (12) a resume with a higher serial under the same root is accepted and the ratchet is raised at the commit, a lower serial or an equal serial with another hash is refused with the logged command; (13) `minion worker reset` and expiry delete the record and a new code is shown; (14) the `connect` mode ignores the record; the enrollment is uncommitted until `welcome`. **Owner re-issue:** only for a Worker with empty `first_connected_at` (not for a backfilled or connected one), only for its owner or an administrator, window of 10 minutes, completion with a new code keeps id, name and approval state, the window cannot be used by another key, expiry of the window restores `already_paired`; the Worker page shows the button only in the eligible state; **atomicity:** a first connect handshake racing the `repair_until` write and the repair completion is serialised by the conditional updates (exactly one of them wins, never both). Existing pairing tests keep passing.

**Backup and restore.** With the root, `--exclude-root`, hardened (a residual file of each of the four names never in the archive), with a prepared key and reservation, during an operational-key rotation; the manifest field `root_excluded` and reason set correctly in each case; **phase separation:** the archive and manifest are byte-identical after phase 1, the install plan carries the new hashes of transformed files (migrated database, hardened flag, reissue), a transformed file is installed although its hash differs from the manifest, a file damaged between phase 2 and the install is caught by the plan, SQLite is checkpointed and closed (no `-wal` or `-shm`); a **root-excluded archive restores successfully only with `--root-excluded`**, and the staged database is `hardened` before installation (checked on the staged copy and after start); without the flag it is rejected with the hint; the flag with an archive that is not root-excluded or contains a root file is refused; a missing root is rejected **only** for an archive not marked `root_excluded`; inconsistent combinations (root file with a `hardened` database) are refused whatever the flag says; a format-1 (0.6.x) archive restores and migrates; the OIDC secret round trip still decrypts; a restore with the root issues a fresh certificate and voids the reservation. **Disclosure:** a backup with a prepared key, disclosed; after `renew --prepare --replace` the old prepared key cannot be certified (the old request is refused as superseded and the old key matches nothing); the restored S1 state followed by `--prepare --replace` leaves a fresh key.

**Hardened mode.** The mode is read from the database; a start with each of `root.key`, `root.key.next`, `identity.key`, `identity.key.next` present refuses with the message; every row of the `MINION_ROOT_STORAGE` table of section 10 (including `simple` against a `hardened` database with and without a matching `root.key`, and an invalid value); the documented switch in both directions; `hub_identity_rotate` refused; `minion hub identity` works with no root file; removal of a Worker and its revoke work with no root.

**Migration.** A 0.6.2 data directory (with and without `identity.key.next`) starts, gets `root.key`, `op.key` and a first certificate, and keeps its fingerprint and `mnk_` invitations; both files with different bytes refuses to start; a crash at each step of section 14 is repaired by the next start; a 0.6.2 Worker state (pin, no `max_serial`) connects after both sides upgrade without re-enrollment; a 2.0 Worker is refused with `proto_incompatible` and keeps retrying; an existing-database fixture for both forward migrations; existing Workers have `first_connected_at` backfilled.

**Retirement ratchet (12.5).** `Add` stores `max_serial` and `cert_sha256` in the version-2 metadata before `Wipe` (injected failure of the write aborts before the wipe); **a stale operational certificate (serial below the carried `max_serial`) cannot answer `not_enrolled`**: the entry stays pending and the Worker logs the refusal; a certificate with an equal serial and another hash is refused; a valid higher serial advances the stored ratchet durably before the Worker acts (crash between the advance and the action keeps the advance); a failed advance keeps the entry; a version-1 file is seeded by its first validated connection, cannot be retired by `not_enrolled` on that connection, and is retired by a later one; the migration rewrites it as version 2; a root transition during retirement resets the scope as in section 6.

**Revoke.** Live revoke, tombstone revoke at reconnect and self-retirement across a renewal and an install; a revoke signed by the root, by the previous operational key or by another connection's key is ignored and deletes nothing; a hardened Hub removes a Worker and the revoke verifies; a removed Worker that was offline across a renewal is revoked at its next connect; an offline Worker passes a root rotation without re-pairing (its sessions were closed by the rotation); a pending Worker is revoked live and by tombstone.

## 18. Open questions

1. **Worker clock requirement.** A Worker with no real-time clock or a clock more than one hour behind `not_before` cannot connect. Is the one-hour tolerance right, and should the log line offer the exact clock difference (it does in this design) or an operator override?
2. **Default validity and margin.** 90 days with renewal at 14 days means one session recycle per Worker about every 76 days. Hardened operators renew by hand: should the hardened default be longer (for example 180 days), keeping the 400-day cap?
3. **Upgrade path from 0.6.2.** The 0.6.2 updater has an unwired protocol guard (section 14). Is a documentation-only caveat enough, or should a 0.6.x patch release wire the guard and teach `minion update` to accept a declared major step?
4. **Eligibility for Re-issue pairing.** The backfill makes every pre-0.7 Worker ineligible, including one that was genuinely stuck at upgrade. Acceptable, given such a Worker can be removed and enrolled again?
5. **Pairing review.** The independent cryptographic review of the CPace handshake is still pending; this design changes what CPace authenticates (the certificate is in its session id). Should the review be scheduled before the 0.7.0 tag?
