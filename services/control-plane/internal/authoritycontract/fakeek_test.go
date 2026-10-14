package authoritycontract

import (
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/require"
)

// gen.mjs builds EK SPKIs from a valid SPKI's header and trailer around a
// random odd 4096-bit modulus (fakeEk). §2.4's three steps must accept it.
func TestValidateEKSPKIAcceptsATemplateModulus(t *testing.T) {
	spki := testEKSPKI(t)
	m := make([]byte, 512)
	_, err := rand.Read(m)
	require.NoError(t, err)
	m[0] |= 0x80
	m[511] |= 1
	fake := append(append(append([]byte(nil), spki[:33]...), m...), spki[545:]...)
	require.Len(t, fake, 550)
	_, err = ValidateEKSPKI(fake)
	require.NoError(t, err)
}
