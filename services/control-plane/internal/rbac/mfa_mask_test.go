package rbac_test

import (
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
)

// Unit tests U1–U4 of #3453 (spec §13). Every value below is a hand-derived
// literal, never a value computed through Apply, so a broken Apply cannot
// agree with its own expectation.

var (
	unenrolledMask = rbac.MFAMask{Enforcing: true, Enrolled: false}
	enrolledMask   = rbac.MFAMask{Enforcing: true, Enrolled: true}
	offMask        = rbac.MFAMask{}
)

// concreteMinusDangerous is 0x3FFFFFFF &^ 0x0200419B: what an unenrolled
// Administrator (or owner) keeps on an enforcing server.
const concreteMinusDangerous = rbac.Permission(0x3DFFBE64)

// U1 (table). Kills: EXPAND removed (the Administrator rows lose every
// concrete bit), Dangerous applied before EXPAND (EXPAND re-adds them), bit 62
// not cleared (the Administrator rows keep it), enrolled check inverted (the
// enrolled and unenrolled rows swap).
func TestMFAMaskApplyTable(t *testing.T) {
	cases := []struct {
		name string
		mask rbac.MFAMask
		raw  rbac.Permission
		want rbac.Permission
	}{
		{"not enforcing leaves an Administrator raw", offMask, rbac.PermAdministrator | rbac.PermKick, rbac.PermAdministrator | rbac.PermKick},
		{"not enforcing leaves a moderator raw", offMask, 0x3cfffe80, 0x3cfffe80},
		{"enrolled leaves an Administrator raw", enrolledMask, rbac.PermAdministrator, rbac.PermAdministrator},
		{"enrolled leaves a moderator raw", enrolledMask, 0x3cfffe80, 0x3cfffe80},
		{"unenrolled moderator loses Kick and ManageAllMessages", unenrolledMask, 0x3cfffe80, 0x3cffbe00},
		{"unenrolled plain member is unchanged", unenrolledMask, 0x1ce3be00, 0x1ce3be00},
		{"unenrolled Administrator is expanded, then masked", unenrolledMask, rbac.PermAdministrator, concreteMinusDangerous},
		{"unenrolled Administrator role over @all", unenrolledMask, 0x400000001ce3be00, concreteMinusDangerous},
		{"undefined bits pass through the expansion", unenrolledMask, rbac.PermAdministrator | 1<<40, concreteMinusDangerous | 1<<40},
		{"undefined bits pass through without bit 62", unenrolledMask, rbac.PermKick | 1<<33, 1 << 33},
		{"zero stays zero", unenrolledMask, 0, 0},
		{"every dangerous bit alone is removed", unenrolledMask, rbac.DangerousPermissions, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.mask.Apply(tc.raw)
			assert.Equal(t, tc.want, got, "got %#x want %#x", int64(got), int64(tc.want))
		})
	}
}

// derivedRaw returns the i-th bitfield of a fixed corpus, in the stored shape:
// bit 63 is never set (#2869's CHECKs). Derived rather than random, so a
// failure reproduces (the auth_useridcanon_soundness_test.go precedent).
func derivedRaw(domain string, i int) rbac.Permission {
	sum := sha256.Sum256([]byte(fmt.Sprintf("#3453 %s %d", domain, i)))
	var v int64
	for j, b := range sum[:8] {
		if j == 0 {
			b &= 0x7f
		}
		v = v<<8 | int64(b)
	}
	return rbac.Permission(v)
}

// concreteBits lists every single bit of ConcretePermissions.
func concreteBits() []rbac.Permission {
	var out []rbac.Permission
	for i := 0; i < 63; i++ {
		b := rbac.Permission(1) << i
		if rbac.ConcretePermissions&b != 0 {
			out = append(out, b)
		}
	}
	return out
}

// U1 (property). Invariant I1 over a fixed corpus of raw bitfields and every
// concrete bit.
func TestMFAMaskApplyPreservesEveryNonDangerousBit(t *testing.T) {
	bits := concreteBits()
	require.Len(t, bits, 30)
	for i := 0; i < 2000; i++ {
		raw := derivedRaw("I1", i)
		if i%2 == 0 {
			raw |= rbac.PermAdministrator
		}
		masked := unenrolledMask.Apply(raw)
		require.Zero(t, masked&rbac.PermAdministrator, "raw %#x: masked value carries bit 62", int64(raw))
		require.GreaterOrEqual(t, int64(masked), int64(0), "raw %#x: masked value sets bit 63", int64(raw))
		for _, b := range bits {
			if rbac.DangerousPermissions&b != 0 {
				require.False(t, masked.Has(b), "raw %#x: dangerous bit %#x survived", int64(raw), int64(b))
				continue
			}
			require.Equal(t, raw.Has(b), masked.Has(b), "raw %#x: bit %#x changed", int64(raw), int64(b))
		}
		require.Equal(t, raw, enrolledMask.Apply(raw), "an enrolled member is never masked")
		require.Equal(t, raw, offMask.Apply(raw), "a non-enforcing server never masks")
	}
}

// U2. Apply is idempotent under every mask.
func TestMFAMaskApplyIsIdempotent(t *testing.T) {
	for i := 0; i < 2000; i++ {
		raw := derivedRaw("U2", i)
		for _, m := range []rbac.MFAMask{offMask, enrolledMask, unenrolledMask} {
			once := m.Apply(raw)
			require.Equal(t, once, m.Apply(once), "mask %+v, raw %#x", m, int64(raw))
		}
	}
}

// U3. Pins both constants and their bit lists.
func TestMFAMaskConstantsArePinned(t *testing.T) {
	assert.Equal(t, rbac.Permission(0x0200419B), rbac.DangerousPermissions)
	assert.Equal(t, rbac.Permission(0x3FFFFFFF), rbac.ConcretePermissions)
	var dangerousBits []int
	for i := 0; i < 64; i++ {
		if rbac.DangerousPermissions&(rbac.Permission(1)<<i) != 0 {
			dangerousBits = append(dangerousBits, i)
		}
	}
	assert.Equal(t, []int{0, 1, 3, 4, 7, 8, 14, 25}, dangerousBits)
	assert.Zero(t, rbac.ConcretePermissions&rbac.PermAdministrator, "bit 62 is never concrete")
	assert.Equal(t, rbac.DangerousPermissions, rbac.DangerousPermissions&rbac.ConcretePermissions)
	assert.Zero(t, rbac.DangerousPermissions&rbac.PermManageRolesAssign, "D6 keeps ManageRolesAssign out")

	var named rbac.Permission
	for p := range rbac.PermissionNames {
		named |= p
	}
	assert.Equal(t, rbac.ConcretePermissions, named&^rbac.PermAdministrator)
}

// U3 (AST). Every Perm* const declared in types.go is a PermissionNames key,
// and ConcretePermissions is exactly the OR of them minus bit 62. Kills: a new
// Perm* added without registering it, or without adding it to Concrete.
//
// The const declarations are type-checked on their own (they need nothing but
// the Permission type), so types.go's imports do not matter here.
func TestEveryPermConstIsNamedAndConcrete(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "types.go", nil, 0)
	require.NoError(t, err)

	var decls []ast.Decl
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		switch gen.Tok {
		case token.CONST:
			decls = append(decls, gen)
		case token.TYPE:
			for _, spec := range gen.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.Name == "Permission" {
					decls = append(decls, &ast.GenDecl{Tok: token.TYPE, Specs: []ast.Spec{ts}})
				}
			}
		}
	}
	pkg, err := (&types.Config{}).Check("rbac", fset,
		[]*ast.File{{Name: ast.NewIdent("rbac"), Decls: decls}}, nil)
	require.NoError(t, err)

	var concrete rbac.Permission
	declared := 0
	for _, name := range pkg.Scope().Names() {
		c, ok := pkg.Scope().Lookup(name).(*types.Const)
		if !ok || !strings.HasPrefix(name, "Perm") {
			continue
		}
		v, exact := constant.Int64Val(c.Val())
		require.True(t, exact, name)
		p := rbac.Permission(v)
		_, registered := rbac.PermissionNames[p]
		assert.True(t, registered, "%s (%#x) is not a PermissionNames key", name, v)
		if p != rbac.PermAdministrator {
			concrete |= p
		}
		declared++
	}
	assert.Equal(t, len(rbac.PermissionNames), declared, "every PermissionNames key is a Perm* const")
	assert.Equal(t, rbac.ConcretePermissions, concrete,
		"ConcretePermissions must be the OR of every Perm* const except PermAdministrator")
}

// U4. An unenrolled owner keeps everything but the dangerous set, including
// PermMentionEveryone (RS9).
func TestMFAMaskUnenrolledOwner(t *testing.T) {
	got := unenrolledMask.Apply(rbac.OwnerPermissions)
	assert.Equal(t, rbac.OwnerPermissions&^rbac.DangerousPermissions, got)
	assert.Equal(t, concreteMinusDangerous, got)
	assert.True(t, got.Has(rbac.PermMentionEveryone))
	assert.False(t, got.Has(rbac.PermManageServer))
}
