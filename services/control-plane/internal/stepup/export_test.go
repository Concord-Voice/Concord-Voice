package stepup

// Test seams for the external stepup_test package, which exists because mfa
// imports stepup and so cannot be imported by stepup's own tests.
var (
	SubjectTestDB   = subjectTestDB
	SubjectTestUser = subjectTestUser
	SubjectTestTOTP = subjectTestTOTP
	FactorsSetTOTP  = factorsSetTOTP
	FactorsAddKey   = factorsAddKey
)
