package tlspin

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// AC14: no tls.Config field that skips peer verification anywhere under
// internal/authority*: this tree, the contract, and #2627's
// internal/authorityclient when it lands. The field's name is assembled from
// two halves, and never written whole in this tree, so the walk does not
// match this file.
func TestNoSkippedPeerVerificationUnderTheAuthority(t *testing.T) {
	needle := []byte("Insecure" + "SkipVerify")
	var scanned []string
	err := walkAuthorityGo(os.DirFS(filepath.Join("..", "..")), func(p string, b []byte) { // services/control-plane/internal
		scanned = append(scanned, p)
		if bytes.Contains(b, needle) {
			t.Errorf("%s names it", p)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	// Positive controls: the walk read a named file in each tree it guards
	// (authoritycontract alone holds more than 50 files, so a count cannot
	// tell whether authority/ was reached), and it read the authority tree
	// broadly rather than one file of each.
	for _, want := range []string{"authority/tlspin/tlspin.go", "authoritycontract/crypto.go"} {
		if !slices.Contains(scanned, want) {
			t.Errorf("the walk never read %s", want)
		}
	}
	if len(scanned) < 50 {
		t.Fatalf("scanned only %d files", len(scanned))
	}
}

// walkAuthorityGo calls visit with the path and contents of every .go file
// under a top-level internal/authority* directory of internal.
func walkAuthorityGo(internal fs.FS, visit func(p string, b []byte)) error {
	return fs.WalkDir(internal, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && !authorityDir(p) {
			return fs.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		b, err := fs.ReadFile(internal, p)
		if err != nil {
			return err
		}
		visit(p, b)
		return nil
	})
}

// authorityDir reports whether the walk descends into directory p: the root,
// and anything under a top-level internal/authority* directory.
func authorityDir(p string) bool {
	return p == "." || strings.HasPrefix(strings.SplitN(p, "/", 2)[0], "authority")
}
