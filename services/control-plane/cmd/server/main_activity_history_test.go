package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/activepresence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	controlwebsocket "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/gin-gonic/gin"
)

func TestBuildActivityHistoryDisclosureUsesOnlyDevelopmentAndTestForLoopbackHTTP(t *testing.T) {
	if disclosure := buildActivityHistoryDisclosure(nil); disclosure.Available {
		t.Fatal("nil config must produce unavailable disclosure")
	}
	tests := []struct {
		name      string
		cfg       *config.Config
		available bool
	}{
		{
			name: "saas ignores operator substitution",
			cfg: &config.Config{
				InstanceType:                    "saas",
				ActivityHistoryOperatorName:     "Untrusted Override",
				ActivityHistoryPrivacyPolicyURL: "http://example.test/privacy",
			},
			available: true,
		},
		{
			name: "self hosted https available",
			cfg: &config.Config{
				InstanceType:                    "self-hosted",
				ActivityHistoryOperatorName:     "Example Operator",
				ActivityHistoryPrivacyPolicyURL: "https://example.test/privacy",
			},
			available: true,
		},
		{
			name: "self hosted missing unavailable",
			cfg:  &config.Config{InstanceType: "self-hosted"},
		},
		{
			name: "development loopback http available",
			cfg: &config.Config{
				Environment:                     "development",
				InstanceType:                    "self-hosted",
				ActivityHistoryOperatorName:     "Local Operator",
				ActivityHistoryPrivacyPolicyURL: "http://127.0.0.1:8080/privacy",
			},
			available: true,
		},
		{
			name: "test loopback http available",
			cfg: &config.Config{
				Environment:                     "test",
				InstanceType:                    "self-hosted",
				ActivityHistoryOperatorName:     "Test Operator",
				ActivityHistoryPrivacyPolicyURL: "http://localhost:8080/privacy",
			},
			available: true,
		},
		{
			name: "staging loopback http unavailable",
			cfg: &config.Config{
				Environment:                     "staging",
				InstanceType:                    "self-hosted",
				ActivityHistoryOperatorName:     "Staging Operator",
				ActivityHistoryPrivacyPolicyURL: "http://localhost:8080/privacy",
			},
		},
		{
			name: "production loopback http unavailable",
			cfg: &config.Config{
				Environment:                     "production",
				InstanceType:                    "self-hosted",
				ActivityHistoryOperatorName:     "Production Operator",
				ActivityHistoryPrivacyPolicyURL: "http://localhost:8080/privacy",
			},
		},
		{
			name: "development arbitrary http unavailable",
			cfg: &config.Config{
				Environment:                     "development",
				InstanceType:                    "self-hosted",
				ActivityHistoryOperatorName:     "Dev Operator",
				ActivityHistoryPrivacyPolicyURL: "http://example.test/privacy",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			disclosure := buildActivityHistoryDisclosure(tc.cfg)
			if disclosure.Available != tc.available {
				t.Fatalf("available = %v, want %v", disclosure.Available, tc.available)
			}
			if tc.name == "saas ignores operator substitution" {
				canonical := presencehistory.BuildDisclosure(presencehistory.DisclosureOptions{InstanceType: "saas"})
				if !reflect.DeepEqual(canonical, disclosure) {
					t.Fatalf("SaaS disclosure was not canonical")
				}
			}
		})
	}
}

func TestInitializeActivityHistoryRuntimeOrdersStartupAndFailsClosed(t *testing.T) {
	if _, err := initializeActivityHistoryRuntime(context.Background(), activityHistoryStartupSteps{}); err == nil {
		t.Fatal("missing startup dependencies must fail closed")
	}
	events := make([]string, 0, 4)
	steps := activityHistoryStartupSteps{
		reconcileDisclosure: func(ctx context.Context) (int64, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("startup reconciliation context is not bounded")
			}
			events = append(events, "disclosure")
			return 2, nil
		},
		bindRuntime: func() error {
			events = append(events, "bind")
			return nil
		},
		reconcilePending: func(ctx context.Context, limit int) (presencehistory.ReconcileStats, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("pending startup reconciliation context is not bounded")
			}
			if limit != activityHistoryStartupBatch {
				t.Fatalf("pending startup limit = %d, want %d", limit, activityHistoryStartupBatch)
			}
			events = append(events, "pending")
			return presencehistory.ReconcileStats{}, nil
		},
		reconcileActivePlans: func(ctx context.Context, limit int) (activepresence.PassStats, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("active-plan startup reconciliation context is not bounded")
			}
			if limit != activityHistoryStartupBatch {
				t.Fatalf("active-plan startup limit = %d, want %d", limit, activityHistoryStartupBatch)
			}
			events = append(events, "active_plans")
			return activepresence.PassStats{}, nil
		},
		startWorkers: func() { events = append(events, "workers") },
	}

	paused, err := initializeActivityHistoryRuntime(context.Background(), steps)
	if err != nil {
		t.Fatalf("initialize runtime: %v", err)
	}
	if paused != 2 {
		t.Fatalf("paused = %d, want 2", paused)
	}
	want := []string{"disclosure", "bind", "pending", "active_plans", "workers"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("startup order = %v, want %v", events, want)
	}

	for _, failure := range []string{"disclosure", "bind", "pending", "active_plans"} {
		t.Run("failure at "+failure, func(t *testing.T) {
			calls := make([]string, 0, 4)
			fail := errors.New("classified startup failure")
			steps := activityHistoryStartupSteps{
				reconcileDisclosure: func(context.Context) (int64, error) {
					calls = append(calls, "disclosure")
					if failure == "disclosure" {
						return 0, fail
					}
					return 0, nil
				},
				bindRuntime: func() error {
					calls = append(calls, "bind")
					if failure == "bind" {
						return fail
					}
					return nil
				},
				reconcilePending: func(context.Context, int) (presencehistory.ReconcileStats, error) {
					calls = append(calls, "pending")
					if failure == "pending" {
						return presencehistory.ReconcileStats{}, fail
					}
					return presencehistory.ReconcileStats{}, nil
				},
				reconcileActivePlans: func(context.Context, int) (activepresence.PassStats, error) {
					calls = append(calls, "active_plans")
					if failure == "active_plans" {
						return activepresence.PassStats{}, fail
					}
					return activepresence.PassStats{}, nil
				},
				startWorkers: func() { calls = append(calls, "workers") },
			}
			_, err := initializeActivityHistoryRuntime(context.Background(), steps)
			if !errors.Is(err, fail) {
				t.Fatalf("error = %v, want classified failure", err)
			}
			for _, call := range calls {
				if call == "workers" {
					t.Fatalf("workers started after %s failure", failure)
				}
			}
		})
	}
}

func TestStartActivityHistoryRuntimeReturnsBoundDependenciesAndWorkers(t *testing.T) {
	if _, err := startActivityHistoryRuntime(activityHistoryRuntimeDependencies{}); err == nil {
		t.Fatal("missing runtime dependencies must fail closed")
	}

	router := &gin.Engine{}
	hub := &controlwebsocket.Hub{}
	natsClient := &natsclient.Client{}
	started := make(chan string, 3)
	runtime, err := startActivityHistoryRuntime(activityHistoryRuntimeDependencies{
		startupContext: context.Background(),
		workerContext:  context.Background(),
		reconcileDisclosure: func(context.Context) (int64, error) {
			return 3, nil
		},
		bindRouter: func() (*gin.Engine, *controlwebsocket.Hub, *natsclient.Client, error) {
			return router, hub, natsClient, nil
		},
		reconcilePending: func(context.Context, int) (presencehistory.ReconcileStats, error) {
			return presencehistory.ReconcileStats{}, nil
		},
		reconcileActivePlans: func(context.Context, int) (activepresence.PassStats, error) {
			return activepresence.PassStats{}, nil
		},
		pendingWorker:    func(context.Context) { started <- "pending" },
		activePlanWorker: func(context.Context) { started <- "active_plans" },
		retentionWorker:  func(context.Context) { started <- "retention" },
	})
	if err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	runtime.waitWorkers()
	if runtime.router != router || runtime.hub != hub || runtime.natsClient != natsClient {
		t.Fatal("runtime did not preserve bound router dependencies")
	}
	if runtime.paused != 3 {
		t.Fatalf("paused = %d, want 3", runtime.paused)
	}
	seen := startedWorkerSet(t, started, 3)
	if !seen["pending"] || !seen["active_plans"] || !seen["retention"] {
		t.Fatalf("started workers = %v, want pending, active_plans and retention", seen)
	}
}

// startedWorkerSet collects exactly count worker names, or fails by NAME.
//
// A bare <-started would block forever on a worker that never launched, so the
// suite would report a 10-minute panic rather than the missing worker — which is
// precisely the regression a variadic launcher can introduce.
func startedWorkerSet(t *testing.T, started <-chan string, count int) map[string]bool {
	t.Helper()
	seen := make(map[string]bool, count)
	for i := 0; i < count; i++ {
		select {
		case name := <-started:
			seen[name] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d background workers started: %v", i, count, seen)
		}
	}
	return seen
}

// A failed startup pass must FAIL BOOT rather than degrade. Reconciler.Run
// discards every pass error on purpose, so a process that came up with the rail
// unable to reach its database would accumulate plans and deliver nothing, with
// no error anywhere. This is the surface that says so.
func TestStartActivityHistoryRuntimeFailsBootWhenActivePlanReconciliationFails(t *testing.T) {
	unreachable := errors.New("classified active-plan startup failure")
	started := make(chan string, 3)
	_, err := startActivityHistoryRuntime(activityHistoryRuntimeDependencies{
		startupContext:      context.Background(),
		workerContext:       context.Background(),
		reconcileDisclosure: func(context.Context) (int64, error) { return 0, nil },
		bindRouter: func() (*gin.Engine, *controlwebsocket.Hub, *natsclient.Client, error) {
			return &gin.Engine{}, &controlwebsocket.Hub{}, &natsclient.Client{}, nil
		},
		reconcilePending: func(context.Context, int) (presencehistory.ReconcileStats, error) {
			return presencehistory.ReconcileStats{}, nil
		},
		reconcileActivePlans: func(context.Context, int) (activepresence.PassStats, error) {
			return activepresence.PassStats{}, unreachable
		},
		pendingWorker:    func(context.Context) { started <- "pending" },
		activePlanWorker: func(context.Context) { started <- "active_plans" },
		retentionWorker:  func(context.Context) { started <- "retention" },
	})
	if !errors.Is(err, unreachable) {
		t.Fatalf("error = %v, want the classified startup failure", err)
	}
	select {
	case name := <-started:
		t.Fatalf("worker %q started after a failed startup pass", name)
	default:
	}
}

// Three workers, not two: startActivityHistoryWorkers is variadic so a fourth
// never again requires editing a hardcoded Add(2), and a loop that dropped its
// last worker is exactly what a two-worker table would miss.
func TestStartActivityHistoryWorkersStartsEverySuppliedWorkerAndWaitsForCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan string, 3)
	var mu sync.Mutex
	counts := map[string]int{}
	worker := func(name string) func(context.Context) {
		return func(ctx context.Context) {
			mu.Lock()
			counts[name]++
			mu.Unlock()
			started <- name
			<-ctx.Done()
		}
	}
	wait := startActivityHistoryWorkers(ctx,
		worker("pending"), worker("active_plans"), worker("retention"))
	seen := startedWorkerSet(t, started, 3)
	if !seen["pending"] || !seen["active_plans"] || !seen["retention"] {
		t.Fatalf("started workers = %v", seen)
	}

	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done:
		t.Fatal("wait returned before cancellation")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker wait did not return after cancellation")
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["pending"] != 1 || counts["active_plans"] != 1 || counts["retention"] != 1 {
		t.Fatalf("worker counts = %v, want one each", counts)
	}
}

func TestRunControlPlaneInitializesActivityHistoryBeforeTrafficAndLogsMetadataOnly(t *testing.T) {
	source, err := os.ReadFile("main.go") // #nosec G304 -- fixed test-only source path
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, "main.go", source, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var initializePosition, listenPosition token.Pos
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "runControlPlane" || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch activityHistoryCallName(call.Fun) {
			case "startActivityHistoryRuntime":
				initializePosition = call.Pos()
			case "ListenAndServe":
				listenPosition = call.Pos()
			case "Info", "Fatal":
				if !activityHistoryLogCall(call) {
					return true
				}
				for _, argument := range call.Args[1:] {
					ast.Inspect(argument, func(value ast.Node) bool {
						identifier, ok := value.(*ast.Ident)
						if !ok {
							return true
						}
						normalized := strings.ToLower(identifier.Name)
						for _, forbidden := range []string{
							"err", "disclosure", "hash", "consent", "payload", "cursor",
							"user", "sender", "operation", "timestamp",
						} {
							if strings.Contains(normalized, forbidden) {
								t.Errorf("Activity History startup log contains forbidden value %q", identifier.Name)
							}
						}
						return true
					})
				}
			}
			return true
		})
	}
	if initializePosition == token.NoPos || listenPosition == token.NoPos {
		t.Fatalf("startup/listen positions missing: initialize=%v listen=%v", initializePosition, listenPosition)
	}
	if initializePosition >= listenPosition {
		t.Fatalf("Activity History startup at %s must precede ListenAndServe at %s",
			files.Position(initializePosition), files.Position(listenPosition))
	}
}

func TestRunControlPlaneActivatesAdminReaderOnlyAfterSafeStartupBoundary(t *testing.T) {
	source, err := os.ReadFile("main.go") // #nosec G304 -- fixed test-only source path
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, "main.go", source, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var routerPosition, signalPosition, readerPosition, listenPosition token.Pos
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "runControlPlane" || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch activityHistoryCallName(call.Fun) {
			case "startActivityHistoryRuntime":
				routerPosition = call.Pos()
			case "Notify":
				signalPosition = call.Pos()
			case "configureAdminOpsMetricsReader":
				readerPosition = call.Pos()
			case "ListenAndServe":
				listenPosition = call.Pos()
			case "Fatal", "Fatalf", "Exit":
				t.Errorf("runControlPlane contains defer-skipping call %s at %s",
					activityHistoryCallName(call.Fun), files.Position(call.Pos()))
			}
			return true
		})
	}

	for name, position := range map[string]token.Pos{
		"router": routerPosition,
		"signal": signalPosition,
		"reader": readerPosition,
		"listen": listenPosition,
	} {
		if position == token.NoPos {
			t.Fatalf("%s startup position is missing", name)
		}
	}
	if routerPosition >= readerPosition || signalPosition >= readerPosition || readerPosition >= listenPosition {
		t.Fatalf(
			"unsafe startup order: router=%s signal=%s reader=%s listen=%s",
			files.Position(routerPosition),
			files.Position(signalPosition),
			files.Position(readerPosition),
			files.Position(listenPosition),
		)
	}
}

func activityHistoryCallName(function ast.Expr) string {
	switch value := function.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		return value.Sel.Name
	default:
		return ""
	}
}

func activityHistoryLogCall(call *ast.CallExpr) bool {
	if len(call.Args) == 0 {
		return false
	}
	literal, ok := call.Args[0].(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return false
	}
	message, err := strconv.Unquote(literal.Value)
	return err == nil && strings.Contains(message, "Activity History")
}
