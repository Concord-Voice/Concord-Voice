package authoritycontract

import (
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// floodCeiling bounds what one refused flood may allocate. A flood is refused
// before EN5 step 3 builds a value tree, so the decode allocates the layout
// struct, its field table and the error, and nothing that grows with the
// input. Measured at under 10 KiB for every flood below, with and without
// -race; before prescan the smallest of them allocated over 60 MiB.
const floodCeiling = 64 << 10

type flood struct {
	name, tag string
	b         []byte
}

// fill writes head, then as many comma-separated copies of item as fit in
// limit bytes together with tail.
func fill(head, item, tail string, limit int) []byte {
	n := (limit - len(head) - len(tail) + 1) / (len(item) + 1)
	var sb strings.Builder
	sb.Grow(limit)
	sb.WriteString(head)
	for i := range n {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(item)
	}
	sb.WriteString(tail)
	return []byte(sb.String())
}

func capOfTag(tag string) int { return capOf(registry[tag]()) }

// floods are messages under their byte cap that the rest of EN5 refuses and
// that the decoder used to materialise in full first. The first five are the
// security review's (M1). The last three each isolate one prescan rule: the
// depth rule (an unbounded head position, where no item count applies), the
// top-level count rule, and the item count rule at a correctly placed array.
func floods() []flood {
	at := func(tag string) string { return encodedPrefix + `"` + tag + `",` }
	return []flood{
		{"trust items of \"\"", "trust", fill(at("trust")+`[`, `""`, `]]`, CapTrust)},
		{"trust items of []", "trust", fill(at("trust")+`[`, `[]`, `]]`, CapTrust)},
		{"history strings at account_id", "history", fill(at("history")+`[`, `"A"`, `]]`, capOfTag("history"))},
		{"chain-head strings at realm_id", "chain-head", fill(at("chain-head")+`[`, `"A"`, `]]`, HeadBlobMax)},
		{"chain-head [] at realm_id", "chain-head", fill(at("chain-head")+`[`, `[]`, `]]`, HeadBlobMax)},
		{"chain-head depth 3 in seq_index", "chain-head",
			fill(at("chain-head")+strings.Repeat(`"",`, 9)+`[[`, `""`, `]],[],[],[]]`, HeadBlobMax)},
		{"trust top-level values", "trust", fill(at("trust")+`[],`, `""`, `]`, CapTrust)},
		{"history bundles over HISTORY_PAGE", "history", fill(at("history")+`"",[`, `"A"`, `]]`, capOfTag("history"))},
	}
}

// allocDuring is the heap allocated while f runs. The package runs no
// t.Parallel, so nothing else allocates in the window.
func allocDuring(f func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestRefusedFloodsAllocateLittle is M1's regression test (CWE-789/400): a
// refused message must not cost the decoder a multiple of its own size.
func TestRefusedFloodsAllocateLittle(t *testing.T) {
	for _, f := range floods() {
		require.LessOrEqual(t, len(f.b), capOfTag(f.tag), "%s fits its cap, so step 1 does not refuse it", f.name)
		var err error
		got := allocDuring(func() { _, err = Decode(f.tag, f.b) })
		require.ErrorIs(t, err, ErrMalformed, f.name)
		require.LessOrEqual(t, got, uint64(floodCeiling),
			"%s: %d bytes allocated refusing %d bytes", f.name, got, len(f.b))
		t.Logf("%-36s %9d bytes in, %6d bytes allocated", f.name, len(f.b), got)
	}
}
