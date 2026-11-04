// Package accountguard is the C4 account guard's runner side (#3680): the key
// derivation and key-id registry here, with the runner, the transaction seams and the
// sweeper arriving in later tasks. The pure decision core is the sub-package decide.
//
// This package is the only writer of C4 rows (the account_pending_* tables,
// user_credential_history, account_address_reservations and accountguard_hmac_keys).
// Runner.Run will be its only transaction opener, with one exception:
// EnrolAtRegistrationTx, which takes the caller's transaction instead of opening its own.
//
// The obligations the runner owes the decision core (snapshot, clock, proof, enrol class,
// and the fields whose zero value is legitimate) live in decide/doc.go.
package accountguard
