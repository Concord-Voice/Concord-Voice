// Package decide is the account guard's pure decision core (#3680, child spec §7): Decide
// maps a snapshot of an account's credentials, primaries, reservations and requests to the
// row of the closed T table (parent §6.2) that applies, and to the effects the runner must
// write.
//
// It is pure: no clock reads, no I/O, no database and no package state. Non-test files
// import only time, sort, errors and bytes (purity_test.go), and Input.Now is the only
// clock. ClassOf and LoginFloor are exported so the runner (PR3) and the routes (PR7a)
// compute class the same way.
//
// # Runner obligations (PR3)
//
// Decide is only as sound as the snapshot it is given and the way its outcome is applied.
//
//   - Snapshot. Input.Creds is the pre-action snapshot of the account's LIVE credentials only:
//     verified addresses, confirmed TOTP, registered WebAuthn, and the password, SSO links,
//     backup-code sets and anchors. The credential an Enrol adds is not in it; an Enrol whose
//     slot is already present is refused (architect ruling 7).
//   - Now. Input.Now is the database clock. A zero Now is refused with RowNone and no effects
//     (red-team F3): at a zero instant nothing counts, so every mature credential would read
//     as immature and could be contested at equal class.
//   - Proof. Every Proof.Used ref was verified by the runner at its current material
//     version. A WebAuthn ref is included only when THIS assertion carried user verification
//     (protocol.VerificationRequired); a non-UV assertion contributes no ref.
//   - Device-bound WebAuthn (red-team F1). A WebAuthn ref is in Proof.DeviceBound only when
//     THIS assertion's authenticator data has BE=0. Decide reads backup eligibility from
//     nowhere else, so an empty DeviceBound costs WebAuthn its C3 pairing with an anchor and
//     never grants it. PR3 verifies every assertion through go-webauthn's ValidateLogin (or
//     ValidatePasskeyLogin), never a hand-rolled signature check, and sets DeviceBound from
//     !parsed.Response.AuthenticatorData.Flags.HasBackupEligible() (equivalently, from the BE
//     flag of the credential ValidateLogin returns, which it has already required to equal
//     the stored record's). That is one shared helper, which PR7a reuses.
//   - C7 and BE storage. Storing BE/BS at registration and loading them into
//     webauthn.Credential.Flags needs no Decide change: Decide never reads a stored BE bit.
//   - SSOLogin (plan D2, H1) states whether the account signs in with SSO rather than a
//     password. It cannot be derived from a password credential being present, because SSO
//     accounts store a real hash. Its zero value is a password account, so a dropped load
//     denies an SSO-only owner rather than letting an SSO assertion alone meet a password
//     account's floor.
//   - MatureNoticesSettled and GuessableBudgetClear (H1) are true only when the runner read
//     them: every notice to a mature address accepted or permanently failed, and every
//     guessable-proof budget read and unlocked. Their zero values hold the request. Until PR4
//     builds the outbox and the budget, PR3 sets both to true explicitly, with a comment: a
//     visible stub, not a silent zero.
//   - EnrolledAt and EnrolClass (D20) come from the 000189 enrolled_at and enrol_class
//     columns; a NULL enrol_class is ClassNone. A zero EnrolledAt or a zero MaturesAt makes a
//     credential uncontestable, and a zero MaturesAt makes it count for nothing.
//   - Enrol class of a written credential (red-team F2). Write Outcome.EnrolClass, together
//     with enrolled_at, in the statement that makes the credential live: the password UPDATE,
//     the TOTP confirm UPDATE (an upsert fires the BEFORE UPDATE trigger), the WebAuthn
//     INSERT. It is ClassOf the proof for T1 and T4, C3 for T6c, and the origin class for a
//     T11 completion of a break-glass or an address_change; every other outcome writes no
//     credential and carries ClassNone. A NULL written here lets a lone mailbox contest the
//     owner's fresh password at the rank floor.
//   - Fields whose zero is legitimate must be loaded, because Decide cannot tell a missed
//     load from a real zero (round-2 Q6): a dropped HeldSince completes a request early, a
//     dropped ContestedAt revives a contested credential, and a dropped DisplacedAt makes a
//     second current primary. Three more cannot be inverted (H1): an empty RecentCancels is
//     legitimate, and a dropped one lets a reopen after a proof cancel open freely (PR3
//     AC-int: that reopen is refused or disputed); a dropped ResetFactors spares the mature
//     factors the owner asked a break-glass to reset, and it mirrors a NOT NULL DEFAULT false
//     column, so inverting it would make an unspecified break-glass reset by default; and a
//     dropped Paper lets a credential that proves like a backup code count as a non-paper P.
//   - Primaries carries exactly one current primary, the row with a zero DisplacedAt
//     (round-2 Q3). With none, or with two, a break-glass completion errs toward restoring
//     the newest reserved primary that was mature when it opened.
//   - Restored primaries (C5 hand-off). A restored primary must be written back with its
//     original enrolled_at and enrol_class as well as its maturity, so Primary or the
//     reservation must carry them by then.
//   - UV coverage (D21). Hold (b) treats every mature, uncontested WebAuthn credential as
//     UV-capable, because Cred carries no UV bit, and none as device-bound, because nobody
//     asserted it (F1): there a WebAuthn never pairs with an anchor into C3, so hold (b) errs
//     only toward holding.
//   - Bound recovery keys (C6, round-2 Q5). covered() builds a proof from credentials the
//     runner never verified, and counts every anchor. Under C4 every recovery key is unbound,
//     so that equals parent §6.2's rule. When C6 binds keys at the authority, Cred must gain
//     an authority-bound bit and covered() must exclude such keys: a bound key cannot cancel
//     at the CP, so counting it makes hold (b) fail open.
//   - Email-MFA self-veto (F4, a C12 obligation). In C4 every email-MFA code is delivered to
//     the primary address (child spec H2), so a T7 cancel drops an email-MFA ref exactly when
//     the request targets the primary. Once email_mfa_target_id may name a backup address,
//     that filter is wrong in both directions: it drops a code that does not prove the
//     primary, and it keeps a code that proves the targeted backup. Cred must then carry the
//     email-MFA delivery slot, and selfVetoFiltered must test that slot against the targets.
//   - A Complete refused with RowNone wedges its request silently; PR3 should count those
//     refusals, and may add CHECK (held_ms >= 0) to its own migration. A row whose CompletesAt is before
//     OpenedAt + Δ_cp is one of them (H3), so PR5's dev fast-forward verb must shift
//     opened_at and completes_at together.
//   - Requests. Every Req has a unique, non-empty ID, or Decide refuses the action.
//     Input.Reqs holds the pending and disputed rows; Input.RecentCancels the proof and
//     dispute_resolved cancels younger than NCancelCP, fingerprints intact. A recent cancel
//     with a zero CancelledAt counts as inside the reopen window (H1).
//   - Origins. Opening.Origins (like Targets and ReservedTo) is in one canonical order:
//     Kind by number, then ID byte-wise, then Version, with no duplicate (architect ruling
//     6). PR3 hashes origin_fingerprint over the slice exactly as given.
//   - New IDs (D12). An EffectOpen carries no RequestID; the runner assigns it. Group is
//     read only on an EffectDispute and on an EffectOpen whose Status is disputed, and ""
//     there means one new dispute group shared by every such effect of the outcome. A notice
//     with an empty RequestID inside an outcome that holds an EffectOpen belongs to the
//     opened request; anywhere else it belongs to none.
//   - Disputes (ruling 12). An EffectDispute on a row that is already disputed writes only
//     dispute_group_id: the row keeps its status and its disputed_at. A merged group may hold
//     several rows with one origin set (D13, A-20), so PR3 creates no unique index on
//     (dispute_group_id, origin_fingerprint).
//   - Holds. EffectHold with a non-zero HeldSince opens a hold (held_since = HeldSince,
//     held_ms = HeldMs); with a zero HeldSince it folds one (held_ms = HeldMs, held_since =
//     NULL). Held time is in milliseconds.
//   - Removals. An EffectRemove names the credential at the version to delete. Its link to a
//     request travels on the credential_removed notice, never on the effect (ruling 11).
//   - Flags. RotateEpoch begins a fence op (child spec §4 input 6); PermGenBump runs the
//     #3453 factor hook after commit; EffectResyncMFAFlags rewrites users.mfa_methods in the
//     transaction. SetsPassword writes the request's pending password hash.
//   - Actions are passed by value. A pointer to an action, like any type outside the closed
//     sum, is refused.
package decide
