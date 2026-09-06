package api_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"
)

// The readiness wiring in NewRouter is invisible to every other test: they all
// construct a health.Prober directly and call Start themselves, so none of
// them exercises NewRouter, and NewRouter itself cannot be driven without a
// live database (it returns early at bindPresenceHistoryRuntime).
//
// This walks the AST rather than grepping the source, which the first version
// of this file did. A text pin caught the accident that actually happened -- a
// deleted `go readinessProber.Start(...)` -- and missed three edits with the
// same production effect, because strings.Contains matches inside COMMENTS and
// never looks at call ARGUMENTS:
//
//   - commenting the call out instead of deleting it (the likelier shape of a
//     "temporarily disable this" edit) read as green;
//   - passing a FRESH health.NewReadiness() instead of dependencies.Readiness
//     read as green -- verbatim the two-instance hazard that RouterDependencies
//     and cmd/server warn about three separate times, where SIGTERM latches a
//     flag nothing reads and /readyz answers 200 through the entire drain;
//   - deleting the route registration read as green.
//
// The AST sees none of the comment forms and does see the arguments, so all
// four are covered here. Precedent: internal/presencehook/invariant_guards_test.go.
//
// WHAT THIS COSTS, stated up front so the next person to redden it knows
// whether they broke production or merely moved code. Three assertions match
// on SYNTAX and will fail on refactors that preserve behaviour exactly:
//
//   - the prober argument must be the selector `dependencies.Readiness`, so
//     hoisting it to a local (`ready := dependencies.Readiness`) reddens this;
//   - `go readinessProber.Start(context.Background())` must be a DIRECT child
//     of NewRouter's body, so moving it into a helper or an if-block reddens
//     this even though the goroutine still starts;
//   - the handler argument must be the call `ReadyzHandler(readinessProber)`,
//     so extracting it to a variable first reddens this.
//
// That is the deliberate trade: this file is the only witness to wiring no
// other test can reach, and the failure it guards is silent in production
// (a /readyz that answers 200 through an entire drain). A false red costs one
// reading of this comment; a false green costs the feature. If you are here
// after a benign refactor, update the assertion to match the new shape -- do
// not delete it, and do not relax it to "some call named Start exists", which
// is the text-pin weakness this file was written to replace.
func newRouterBody(t *testing.T) *ast.FuncDecl {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "router.go", nil, 0)
	if err != nil {
		t.Fatalf("parse router.go: %v", err)
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "NewRouter" && fn.Body != nil {
			return fn
		}
	}
	t.Fatal("NewRouter not found in router.go")
	return nil
}

// selectorIs reports whether e is exactly `x.sel`.
func selectorIs(e ast.Expr, x, sel string) bool {
	s, ok := e.(*ast.SelectorExpr)
	if !ok || s.Sel.Name != sel {
		return false
	}
	id, ok := s.X.(*ast.Ident)
	return ok && id.Name == x
}

// selectorIsIdent reports whether e is the bare identifier `name`.
func selectorIsIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// servesReadyzProber reports whether a router.GET/HEAD("/readyz", X) call's
// handler argument is the prober-backed handler rather than some other one.
// isReadyzHandlerCall reports whether e is literally
// `ReadyzHandler(readinessProber)`.
func isReadyzHandlerCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok || !selectorIsIdent(call.Fun, "ReadyzHandler") || len(call.Args) != 1 {
		return false
	}
	return selectorIsIdent(call.Args[0], "readinessProber")
}

// handlerBinding is one assignment to a local, and whether that assignment made
// it a correct readiness handler.
type handlerBinding struct {
	pos       token.Pos
	qualifies bool
}

// handlerBindings records EVERY assignment to each local in body, so a later or
// nested one can override an earlier one.
//
// Resolution is by nearest PRECEDING assignment rather than by true lexical
// scope. That is an approximation, and it errs conservative in both directions
// that matter: an assignment after the route registration cannot validate it,
// and an inner-block assignment shadows an outer one for any registration that
// follows it textually. A sibling block's assignment is also visible, which
// real Go scoping would not allow -- that direction only ever REJECTS, never
// admits, so it costs a false red on code nobody writes rather than a false
// green on code somebody might.
func handlerBindings(body *ast.BlockStmt) map[string][]handlerBinding {
	bindings := map[string][]handlerBinding{}
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range assign.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || i >= len(assign.Rhs) {
				continue
			}
			bindings[id.Name] = append(bindings[id.Name], handlerBinding{
				pos:       assign.Pos(),
				qualifies: isReadyzHandlerCall(assign.Rhs[i]),
			})
		}
		return true
	})
	for name := range bindings {
		sort.Slice(bindings[name], func(a, b int) bool {
			return bindings[name][a].pos < bindings[name][b].pos
		})
	}
	return bindings
}

// qualifiesAt reports whether the nearest assignment to name BEFORE use made it
// a correct readiness handler.
func qualifiesAt(bindings map[string][]handlerBinding, name string, use token.Pos) bool {
	qualifies := false
	found := false
	for _, b := range bindings[name] {
		if b.pos >= use {
			break
		}
		qualifies, found = b.qualifies, true
	}
	return found && qualifies
}

func servesReadyzProber(args []ast.Expr, bindings map[string][]handlerBinding) bool {
	if len(args) < 2 {
		return false
	}
	if isReadyzHandlerCall(args[1]) {
		return true
	}
	id, ok := args[1].(*ast.Ident)
	return ok && qualifiesAt(bindings, id.Name, id.Pos())
}

// readinessWiring is everything a walk of NewRouter's body can observe about
// the readiness wiring. Collecting it is kept separate from asserting on it so
// the nesting the AST forces -- a type switch over nodes, then a ladder of type
// assertions to reach a selector's receiver -- stays out of the test, which is
// then a flat list of independent failures. Same split readyz.go makes between
// parseReadinessLog and readinessLogLevel, and for the same reason: a reader
// debugging a red here wants the assertion, not the walk.
type readinessWiring struct {
	proberArg            ast.Expr
	proberArgs           []ast.Expr
	started              bool
	stopped              bool
	getRoute             bool
	headRoute            bool
	getServesProber      bool
	headServesProber     bool
	startsWithBackground bool
	wiresSeverityAdapter bool
}

func (w *readinessWiring) noteProberConstruction(call *ast.CallExpr) {
	if !selectorIs(call.Fun, "health", "NewProber") || len(call.Args) == 0 {
		return
	}
	w.proberArg = call.Args[0]
	if len(call.Args) >= 4 {
		w.proberArgs = call.Args
	}
}

func (w *readinessWiring) noteSeverityAdapter(call *ast.CallExpr) {
	if !selectorIs(call.Fun, "readinessProber", "SetLogger") {
		return
	}
	ast.Inspect(call, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && selectorIsIdent(c.Fun, "routeReadinessLine") {
			w.wiresSeverityAdapter = true
		}
		return true
	})
}

// noteRouteRegistration matches `router.GET("/readyz", ...)` and its HEAD twin.
func (w *readinessWiring) noteRouteRegistration(call *ast.CallExpr, bindings map[string][]handlerBinding) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || len(call.Args) == 0 {
		return
	}
	if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "router" {
		return
	}
	if lit, ok := call.Args[0].(*ast.BasicLit); !ok || lit.Value != `"/readyz"` {
		return
	}
	switch sel.Sel.Name {
	case "GET":
		w.getRoute = true
		w.getServesProber = servesReadyzProber(call.Args, bindings)
	case "HEAD":
		w.headRoute = true
		w.headServesProber = servesReadyzProber(call.Args, bindings)
	}
}

// noteStart walks the body's TOP-LEVEL statements only: a `go Start(...)`
// nested in a FuncLit that is never invoked, or guarded by an if, is present to
// the AST and absent at runtime.
func (w *readinessWiring) noteStart(body *ast.BlockStmt) {
	for _, stmt := range body.List {
		g, ok := stmt.(*ast.GoStmt)
		if !ok || !selectorIs(g.Call.Fun, "readinessProber", "Start") {
			continue
		}
		w.started = true
		if len(g.Call.Args) != 1 {
			continue
		}
		if c, ok := g.Call.Args[0].(*ast.CallExpr); ok && selectorIs(c.Fun, "context", "Background") {
			w.startsWithBackground = true
		}
	}
}

func collectReadinessWiring(fn *ast.FuncDecl) *readinessWiring {
	w := &readinessWiring{}
	// Collect handler assignments BEFORE the main walk, so a registration can be
	// resolved against the assignment that precedes it.
	bindings := handlerBindings(fn.Body)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		w.noteProberConstruction(call)
		w.noteSeverityAdapter(call)
		if selectorIs(call.Fun, "readinessProber", "Stop") {
			w.stopped = true
		}
		w.noteRouteRegistration(call, bindings)
		return true
	})
	w.noteStart(fn.Body)
	return w
}

func TestNewRouterWiresTheReadinessProberToTheSharedDrainFlag(t *testing.T) {
	w := collectReadinessWiring(newRouterBody(t))
	proberArg, proberArgs := w.proberArg, w.proberArgs
	started, stopped := w.started, w.stopped
	getRoute, headRoute := w.getRoute, w.headRoute
	getServesProber, headServesProber := w.getServesProber, w.headServesProber
	startsWithBackground, wiresSeverityAdapter := w.startsWithBackground, w.wiresSeverityAdapter

	if proberArg == nil {
		t.Fatal("NewRouter no longer constructs a health.NewProber")
	}
	// THE two-instance hazard. A fresh flag here divorces the prober from the
	// one cmd/server marks draining on SIGTERM: /readyz then answers 200 for
	// the whole shutdown and the deploy stays green.
	if !selectorIs(proberArg, "dependencies", "Readiness") {
		t.Fatalf("health.NewProber's first argument is %T, not dependencies.Readiness. "+
			"A second *health.Readiness silently divorces the drain flag from the "+
			"probe: SIGTERM latches a flag nothing reads and /readyz reports ready "+
			"through the entire drain.", proberArg)
	}
	if !started {
		t.Fatal("the readiness prober is constructed but never started (no `go readinessProber.Start(...)` " +
			"statement). /readyz would answer probe_stale 503 for the life of the " +
			"process, failing every deploy at wait-healthy. A commented-out call " +
			"does not count -- that is why this is an AST walk.")
	}
	if !stopped {
		t.Fatal("the readiness prober is started but never stopped; it outlives shutdown " +
			"and leaks a ticker per NewRouter call in tests")
	}
	// M7: the route may exist and serve the WRONG handler. Registering
	// healthHandler here -- an unconditional 200 three lines away -- left the
	// path assertion green while /readyz answered 200 through every drain and
	// every dead-Postgres window: VULN-001's outcome by a different door.
	if !getServesProber || !headServesProber {
		t.Fatalf("/readyz must be served by ReadyzHandler(readinessProber); "+
			"GET serves it: %v, HEAD serves it: %v. A route registered with any "+
			"other handler (healthHandler is an unconditional 200) reports ready "+
			"through the entire drain.", getServesProber, headServesProber)
	}
	// M6/M4: the AST sees a GoStmt whose callee is Start; it cannot see
	// reachability or context lifetime. `proberCtx, cancel := ...; defer
	// cancel(); go Start(proberCtx)` makes Start publish one verdict and
	// return, so /readyz answers probe_stale forever -- and router.go's own
	// comment about the CancelFunc lint invites exactly that edit.
	if !startsWithBackground {
		t.Fatal("`go readinessProber.Start(...)` must be passed context.Background() " +
			"and sit directly in NewRouter's body. A cancellable context, or a call " +
			"nested in a closure that is never invoked, publishes at most one verdict " +
			"and then /readyz answers probe_stale for the process lifetime.")
	}
	// M8: the constants are pinned by value elsewhere, but not the ORDER they
	// are passed in. Transposed, interval becomes 20s and staleAfter 5s, which
	// violates the documented staleAfter > interval + ProbeTimeout MUST and
	// makes a healthy prober trip its own fence permanently.
	if proberArgs != nil && (!selectorIsIdent(proberArgs[2], "readyzProbeInterval") ||
		!selectorIsIdent(proberArgs[3], "readyzStaleAfter")) {
		t.Fatal("health.NewProber's 3rd/4th arguments must be readyzProbeInterval " +
			"then readyzStaleAfter, in that order. Transposed, the staleness fence " +
			"is shorter than the probe interval and trips on every healthy cycle.")
	}
	// R1: the severity adapter is the entire production effect of the
	// three-level split. `SetLogger(log.Info)` reverted it with a green suite,
	// because readinessLogLevel stayed referenced from its own table test.
	if !wiresSeverityAdapter {
		t.Fatal("SetLogger must be wired to routeReadinessLine; a bare log.Info " +
			"sends readiness LOSS to Info, level-indistinguishable from the " +
			"per-request line emitted for every HTTP call")
	}
	if !getRoute || !headRoute {
		t.Fatalf("/readyz must be registered for both GET (%v) and HEAD (%v); "+
			"an unregistered route 404s and fails every deploy at wait-healthy", getRoute, headRoute)
	}
}

// serves parses one synthetic NewRouter body and asks the resolver whether its
// router.GET("/readyz", ...) registration is served by the readiness prober.
func serves(t *testing.T, body string) bool {
	t.Helper()
	src := "package api\nfunc f() {\n" + body + "\n}\n"
	file, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", src, 0)
	if err != nil {
		t.Fatalf("parse synthetic body: %v\n%s", err, src)
	}
	fn := file.Decls[0].(*ast.FuncDecl)
	bindings := handlerBindings(fn.Body)

	var got, found bool
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "GET" {
			return true
		}
		found = true
		got = servesReadyzProber(call.Args, bindings)
		return true
	})
	if !found {
		t.Fatalf("synthetic body registers no router.GET:\n%s", src)
	}
	return got
}

// TestReadyzHandlerResolutionRespectsOrderAndShadowing drives the resolver
// directly, because these shapes cannot be committed to router.go -- the file
// must keep exactly one, correct wiring.
//
// The first version of this resolver matched a route argument against the SET
// of names ever assigned ReadyzHandler(readinessProber), ignoring where and
// when. Two of the rejection cases below passed under it: a handler reassigned
// correctly AFTER the registration retroactively blessed it, and an inner-block
// shadow was invisible. Both put an unconditional 200 on /readyz while this
// file reported the wiring intact.
//
// The acceptance cases matter as much: a resolver tightened into "must be a
// local spelled readyzHandler" would reject an inline registration and a
// renamed local, turning every benign refactor red and training the next
// reader to delete the check.
func TestReadyzHandlerResolutionRespectsOrderAndShadowing(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"hoisted local, the production shape", `
	readyzHandler := ReadyzHandler(readinessProber)
	router.GET("/readyz", readyzHandler)`, true},

		{"inline call, no local", `
	router.GET("/readyz", ReadyzHandler(readinessProber))`, true},

		{"local under a different name", `
	probeHandler := ReadyzHandler(readinessProber)
	router.GET("/readyz", probeHandler)`, true},

		{"reassigned correctly only AFTER the registration", `
	readyzHandler := gin.HandlerFunc(healthHandler)
	router.GET("/readyz", readyzHandler)
	readyzHandler = ReadyzHandler(readinessProber)`, false},

		{"inner block shadows the good handler", `
	readyzHandler := ReadyzHandler(readinessProber)
	{
		readyzHandler := gin.HandlerFunc(healthHandler)
		router.GET("/readyz", readyzHandler)
	}`, false},

		{"conditional reassignment before the registration", `
	readyzHandler := ReadyzHandler(readinessProber)
	if cond {
		readyzHandler = gin.HandlerFunc(healthHandler)
	}
	router.GET("/readyz", readyzHandler)`, false},

		{"a DIFFERENT name holds the good handler", `
	otherHandler := ReadyzHandler(readinessProber)
	readyzHandler := gin.HandlerFunc(healthHandler)
	router.GET("/readyz", readyzHandler)`, false},

		{"right function, wrong prober", `
	readyzHandler := ReadyzHandler(someOtherProber)
	router.GET("/readyz", readyzHandler)`, false},

		{"never assigned at all", `
	router.GET("/readyz", readyzHandler)`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := serves(t, tc.body); got != tc.want {
				t.Fatalf("servesReadyzProber = %v, want %v. A false positive here puts an "+
					"unconditional 200 on /readyz with this file reporting the wiring "+
					"intact; a false negative reddens a benign refactor.", got, tc.want)
			}
		})
	}
}
