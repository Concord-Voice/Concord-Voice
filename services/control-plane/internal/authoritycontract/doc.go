// Package authoritycontract is the account-key authority's frozen protocol v1
// contract (DoR #2553 §2): the EN1–EN6 codec for every Class F and Class I
// layout and the two persisted heads, the §2.4 digests and signature
// primitives, and the frozen canonical texts.
//
// It is pure: no clock reads, no I/O, no database. It imports the standard
// library and github.com/google/uuid only, and never database/sql or
// internal/age (imports_test.go). The verification API (§2.12) is PR-A2's and
// lives in this package too.
package authoritycontract
