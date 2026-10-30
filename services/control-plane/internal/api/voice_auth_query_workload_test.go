package api

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	protocol "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voiceauthprotocol"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// Select a function and exact SQL literal/call identity. Concatenated call
// expressions expand named constants; dynamic queries select explicit source
// branch fragments. Ambiguous or missing identities fail closed.
func apiWorkloadQuery(t *testing.T, file, fn, identity string, branch int) string {
	t.Helper()
	// Only these package-owned sources supply workload SQL evidence.
	var raw []byte
	var err error
	switch file {
	case "voice_auth_runtime.go":
		raw, err = os.ReadFile("voice_auth_runtime.go")
	case "voice_auth_connections.go":
		raw, err = os.ReadFile("voice_auth_connections.go")
	case "voice_auth_closures.go":
		raw, err = os.ReadFile("voice_auth_closures.go")
	case "voice_auth_boots.go":
		raw, err = os.ReadFile("voice_auth_boots.go")
	case "dmblock_runtime.go":
		raw, err = os.ReadFile("dmblock_runtime.go")
	case "../dmblock/dmblock.go":
		raw, err = os.ReadFile("../dmblock/dmblock.go")
	default:
		t.Fatalf("unsupported workload SQL source %q", file)
	}
	require.NoError(t, err)
	tree, err := parser.ParseFile(token.NewFileSet(), file, raw, 0)
	require.NoError(t, err)
	constants := map[string]ast.Expr{}
	for _, decl := range tree.Decls {
		if g, ok := decl.(*ast.GenDecl); ok {
			for _, s := range g.Specs {
				if v, ok := s.(*ast.ValueSpec); ok && len(v.Names) == 1 && len(v.Values) == 1 {
					constants[v.Names[0].Name] = v.Values[0]
				}
			}
		}
	}
	var expand func(ast.Expr) (string, bool)
	expand = func(e ast.Expr) (string, bool) {
		switch v := e.(type) {
		case *ast.BasicLit:
			if v.Kind != token.STRING {
				return "", false
			}
			s, e := strconv.Unquote(v.Value)
			require.NoError(t, e)
			return s, true
		case *ast.BinaryExpr:
			if v.Op != token.ADD {
				return "", false
			}
			a, ok := expand(v.X)
			b, ok2 := expand(v.Y)
			return a + b, ok && ok2
		case *ast.Ident:
			if c, ok := constants[v.Name]; ok {
				return expand(c)
			}
		}
		return "", false
	}
	if fn == "" {
		e, ok := constants[identity]
		require.True(t, ok)
		query, ok := expand(e)
		require.True(t, ok)
		t.Logf("task8-query-source file=%s constant=%s sha256=%x", file, identity, sha256.Sum256(raw))
		return query
	}
	var function *ast.FuncDecl
	for _, decl := range tree.Decls {
		if f, ok := decl.(*ast.FuncDecl); ok && f.Name.Name == fn {
			require.Nil(t, function)
			function = f
		}
	}
	require.NotNil(t, function, "missing function %s", fn)
	var calls, literals []string
	ast.Inspect(function.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if s, ok := c.Fun.(*ast.SelectorExpr); ok && (s.Sel.Name == "QueryContext" || s.Sel.Name == "QueryRowContext") {
				if len(c.Args) > 1 {
					if q, ok := expand(c.Args[1]); ok && strings.HasPrefix(strings.TrimSpace(q), identity) {
						calls = append(calls, q)
					}
				}
			}
		}
		if l, ok := n.(*ast.BasicLit); ok && l.Kind == token.STRING {
			q, ok := expand(l)
			if ok && strings.HasPrefix(strings.TrimSpace(q), identity) {
				literals = append(literals, q)
			}
		}
		return true
	})
	var query string
	if len(calls) > 0 {
		require.Len(t, calls, 1)
		query = calls[0]
	} else {
		require.Len(t, literals, 1)
		query = literals[0]
	}
	if fn == "claimDueCredentialEpochEjections" {
		var suffix []string
		ast.Inspect(function.Body, func(n ast.Node) bool {
			if a, ok := n.(*ast.AssignStmt); ok && a.Tok == token.ADD_ASSIGN && len(a.Lhs) == 1 {
				if id, ok := a.Lhs[0].(*ast.Ident); ok && id.Name == "query" {
					s, ok := expand(a.Rhs[0])
					require.True(t, ok)
					suffix = append(suffix, s)
				}
			}
			return true
		})
		require.Len(t, suffix, 2)
		require.Contains(t, []int{0, 1}, branch)
		query += suffix[branch]
	}
	t.Logf("task8-query-source file=%s function=%s identity=%q branch=%d sha256=%x", file, fn, identity, branch, sha256.Sum256(raw))
	return query
}
func apiWorkloadPlan(t *testing.T, db *sql.DB, label, query string, args ...any) {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { err := tx.Rollback(); require.True(t, err == nil || err == sql.ErrTxDone) }()
	var plan json.RawMessage
	require.NoError(t, tx.QueryRowContext(t.Context(), `EXPLAIN (ANALYZE, BUFFERS, WAL, FORMAT JSON) `+query, args...).Scan(&plan))
	t.Logf("task8-query-plan label=%s query=%q bound=%v plan=%s mutation_rolled_back=true", label, query, args, plan)
	require.NoError(t, tx.Rollback())
}

var apiWorkloadRelations = []string{"login_families", "login_family_sids", "login_family_revocations", "login_family_revocation_targets", "refresh_tokens", "voice_auth_connections", "voice_auth_room_bindings", "voice_auth_connection_closures", "voice_auth_room_closures", "voice_auth_boot_fences", "voice_enforcement_sessions", "credential_epoch_voice_ejections", "dm_block_voice_ejections"}

type apiWorkloadSample struct {
	lsn   string
	stats runtime.MemStats
	at    time.Time
}

func apiWorkloadSnapshot(t *testing.T, db *sql.DB, phase string) apiWorkloadSample {
	t.Helper()
	sample := apiWorkloadSample{at: time.Now()}
	runtime.ReadMemStats(&sample.stats)
	require.NoError(t, db.QueryRow(`SELECT pg_current_wal_insert_lsn()::text`).Scan(&sample.lsn))
	for _, relation := range apiWorkloadRelations {
		var rows, heap, indexes, total int64
		require.NoError(t, db.QueryRow(`SELECT count(*),pg_relation_size($1::regclass),pg_indexes_size($1::regclass),pg_total_relation_size($1::regclass) FROM `+pq.QuoteIdentifier(relation), relation).Scan(&rows, &heap, &indexes, &total))
		t.Logf("task8-storage phase=%s relation=%s rows=%d heap_bytes=%d index_bytes=%d total_bytes=%d", phase, relation, rows, heap, indexes, total)
	}
	t.Logf("task8-memory phase=%s heap_alloc=%d heap_inuse=%d total_alloc=%d mallocs=%d", phase, sample.stats.HeapAlloc, sample.stats.HeapInuse, sample.stats.TotalAlloc, sample.stats.Mallocs)
	return sample
}
func apiWorkloadDelta(t *testing.T, db *sql.DB, label string, before, after apiWorkloadSample) {
	t.Helper()
	var wal string
	require.NoError(t, db.QueryRow(`SELECT pg_wal_lsn_diff($1::pg_lsn,$2::pg_lsn)::text`, after.lsn, before.lsn).Scan(&wal))
	t.Logf("task8-phase label=%s elapsed=%s allocated_bytes=%d mallocs=%d wal_bytes=%s caveat=cluster-wide-WAL-process-wide-memory-includes-snapshot-overhead", label, after.at.Sub(before.at), after.stats.TotalAlloc-before.stats.TotalAlloc, after.stats.Mallocs-before.stats.Mallocs, wal)
}
func apiWorkloadAnalyze(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, relation := range apiWorkloadRelations {
		_, err := db.Exec(`ANALYZE ` + pq.QuoteIdentifier(relation))
		require.NoError(t, err)
	}
	rows, err := db.Query(`SELECT tablename,indexname,indexdef FROM pg_indexes WHERE schemaname=current_schema() AND tablename=ANY($1) ORDER BY tablename,indexname`, pq.Array(apiWorkloadRelations))
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	for rows.Next() {
		var table, name, definition string
		require.NoError(t, rows.Scan(&table, &name, &definition))
		t.Logf("task8-query-index relation=%s index=%s definition=%s", table, name, definition)
	}
	require.NoError(t, rows.Err())
}

// A canonical mint/bind/sign sequence with unique socket IDs. No rig reset is
// performed inside population loops and no terminal authority is manufactured.
func apiWorkloadMint(t *testing.T, r *voiceAuthRig, boot string, n int) protocol.Binding {
	t.Helper()
	sid := uuid.New()
	prepared, err := auth.PrepareAccessToken(auth.AccessTokenInput{UserID: r.userID, SID: sid, CredentialEpoch: r.epoch, EmailVerified: true}, time.Now())
	require.NoError(t, err)
	tx, err := r.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { err := tx.Rollback(); require.True(t, err == nil || err == sql.ErrTxDone) }()
	barrier, err := loginfamily.AcquireWriterBarrierTx(t.Context(), tx)
	require.NoError(t, err)
	users, err := loginfamily.LockUsersTx(t.Context(), tx, barrier, []uuid.UUID{r.userID}, loginfamily.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := users.ForUser(r.userID)
	require.NoError(t, err)
	family, err := loginfamily.CreateFamilyTx(t.Context(), tx, user, r.epoch)
	require.NoError(t, err)
	require.NoError(t, loginfamily.BindSIDTx(t.Context(), tx, family, sid, prepared.ExpiresAt()))
	require.NoError(t, tx.Commit())
	r.familyID = family.Binding().FamilyID
	r.token, err = prepared.Sign(r.secret)
	require.NoError(t, err)
	r.target = protocol.Target{ConnectionGeneration: uuid.NewString(), NodeBootID: boot, SocketID: fmt.Sprintf("query-workload-%d-%s", n, sid), CredentialEpoch: r.epoch}
	return r.register(t)
}
func apiWorkloadSubject(t *testing.T, r *voiceAuthRig) *voiceAuthRig {
	t.Helper()
	subject := *r
	subject.userID = testdb.CreateUser(t, r.db)
	var err error
	subject.epoch, err = credepoch.NewEpoch()
	require.NoError(t, err)
	_, err = r.db.Exec(`UPDATE users SET credential_epoch=$2 WHERE id=$1`, subject.userID, subject.epoch)
	require.NoError(t, err)
	return &subject
}
func apiWorkloadBoot(t *testing.T, r *voiceAuthRig) string {
	t.Helper()
	boot := uuid.NewString()
	_, err := r.db.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id) VALUES($1)`, boot)
	require.NoError(t, err)
	return boot
}

func apiWorkloadRegistryPlans(t *testing.T, r *voiceAuthRig, o voiceAuthOwner, b protocol.Binding, p protocol.RoomRegister, state string) {
	t.Helper()
	apiWorkloadAnalyze(t, r.db)
	plan := func(file, fn, id string, args ...any) {
		apiWorkloadPlan(t, r.db, state+"/"+fn, apiWorkloadQuery(t, file, fn, id, 0), args...)
	}
	plan("voice_auth_runtime.go", "AuthoritySnapshot", "SELECT activated_at IS NOT NULL,", b.Target.NodeBootID)
	var otherBoot string
	require.NoError(t, r.db.QueryRow(`SELECT node_boot_id FROM voice_auth_connections WHERE node_boot_id<>$1 ORDER BY node_boot_id LIMIT 1`, b.Target.NodeBootID).Scan(&otherBoot))
	plan("voice_auth_runtime.go", "AuthoritySnapshot", "SELECT activated_at IS NOT NULL,", otherBoot)
	for _, attempt := range []int{1, 103} {
		for _, offset := range []int{0, 100} {
			plan("voice_auth_runtime.go", "loadOwnerFamilyPage", "WITH families AS (", o.generation, o.userID, o.auth, attempt, offset, 100)
		}
		plan("voice_auth_runtime.go", "loadOwnerConnections", "WITH candidates AS (", o.generation, o.userID, o.auth, attempt, 100)
	}
	plan("voice_auth_runtime.go", "ownerEvidenceRemains", "SELECT EXISTS(SELECT 1 FROM login_families f", o.generation, o.userID, o.auth)
	if o.auth {
		plan("voice_auth_runtime.go", "deliverAuth", "SELECT scope,reason,attempts", o.generation, o.userID)
	} else {
		var epoch string
		require.NoError(t, r.db.QueryRow(`SELECT credential_epoch FROM credential_epoch_voice_ejections WHERE generation=$1`, o.generation).Scan(&epoch))
		plan("voice_auth_runtime.go", "deliverEpoch", "SELECT attempts", o.generation, o.userID, epoch, o.oldEpoch)
	}
	for _, lock := range []string{"FOR KEY SHARE", "FOR UPDATE"} {
		plan("voice_auth_connections.go", "voiceAuthReadConnection", "SELECT user_id,family_id,connection_generation,node_boot_id,socket_id,credential_epoch,registration_token_sha256,registration_body_sha256 FROM voice_auth_connections WHERE connection_generation=$1 "+lock, b.Target.ConnectionGeneration)
	}
	plan("voice_auth_connections.go", "voiceAuthNoAssociations", "SELECT EXISTS", b.Target.ConnectionGeneration)
	plan("voice_auth_connections.go", "voiceAuthActiveBoot", "SELECT retired_at", b.Target.NodeBootID)
	plan("voice_auth_closures.go", "voiceAuthReadAssociation", "SELECT connection_generation", p.SessionGeneration)
	plan("voice_auth_closures.go", "voiceAuthReadRoom", "SELECT session_generation", p.SessionGeneration)
	apiWorkloadPlan(t, r.db, state+"/boot-zero-live", apiWorkloadQuery(t, "voice_auth_boots.go", "", "voiceAuthBootRetirementClearSQL", 0), b.Target.NodeBootID)
	for _, spec := range []struct {
		fn   string
		args []any
	}{{"loadDMBlockVoiceEnforcementSessions", []any{p.RoomID, r.userID}}, {"loadCredentialEpochVoiceEnforcementSessions", []any{r.userID, o.oldEpoch}}} {
		plan("dmblock_runtime.go", spec.fn, "SELECT count(*)", spec.args...)
		for _, offset := range []int{0, 100} {
			args := append(append([]any{}, spec.args...), offset, 100)
			plan("dmblock_runtime.go", spec.fn, "SELECT session_generation, node_boot_id", args...)
		}
	}
}

func TestVoiceAuthQueryWorkloadRegistryLifecycle(t *testing.T) {
	for _, kind := range []string{"auth", "epoch"} {
		t.Run(kind, func(t *testing.T) {
			r := newVoiceAuthRig(t)
			base := apiWorkloadSnapshot(t, r.db, "baseline")
			// Small separate-user live and opposite-terminal cohorts populate both UNION
			// arms without falsely treating excluded boot rows as target evidence.
			unrelated := apiWorkloadSubject(t, r)
			apiWorkloadMint(t, unrelated, apiWorkloadBoot(t, r), -1)
			opposite := apiWorkloadSubject(t, r)
			apiWorkloadMint(t, opposite, apiWorkloadBoot(t, r), -2)
			if kind == "auth" {
				runtimeRotateEpoch(t, opposite)
			} else {
				runtimeRevoke(t, opposite)
			}
			for round := 0; round < 2; round++ {
				r = apiWorkloadSubject(t, r) // fresh authority; prior pending owners remain retained
				boots := []string{apiWorkloadBoot(t, r), apiWorkloadBoot(t, r)}
				var bindings []protocol.Binding
				var rooms []protocol.RoomRegister
				var dm protocol.RoomRegister
				for n := 0; n < 102; n++ {
					boot := boots[0]
					if n == 101 {
						boot = boots[1]
					}
					b := apiWorkloadMint(t, r, boot, round*102+n)
					bindings = append(bindings, b)
					if n == 0 {
						dm = roomBindingFixture(t, r, "dm")
					}
					p := dm
					p.ConnectionGeneration = b.Target.ConnectionGeneration
					p.NodeBootID = boot
					p.SocketID = b.Target.SocketID
					p.CredentialEpoch = r.epoch
					p.SessionGeneration = uuid.NewString()
					roomBindingAttach(t, r, b, p)
					rooms = append(rooms, p)
				}
				// A separate channel connection shares the last canonically minted family.
				r.target.ConnectionGeneration = uuid.NewString()
				r.target.SocketID = "query-channel-" + uuid.NewString()
				channelBinding := r.register(t)
				channel := roomBindingFixture(t, r, "channel")
				roomBindingAttach(t, r, channelBinding, channel)
				live := apiWorkloadSnapshot(t, r.db, fmt.Sprintf("%s-round%d-live102-DM-plus-channel", kind, round))
				apiWorkloadDelta(t, r.db, "populate", base, live)
				oldEpoch := r.epoch
				o := voiceAuthOwner{userID: r.userID, auth: kind == "auth", oldEpoch: oldEpoch, attempts: 1}
				if o.auth {
					owner := runtimeRevoke(t, r)
					o.generation = owner.OperationID
				} else {
					owner := runtimeRotateEpoch(t, r)
					o.generation = owner.Generation
				}
				var familyCount int
				column := "auth_operation_id"
				if !o.auth {
					column = "epoch_generation"
				}
				require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM login_families WHERE `+column+`=$1`, o.generation).Scan(&familyCount))
				require.Equal(t, 102, familyCount)
				terminal := apiWorkloadSnapshot(t, r.db, "terminal-unreleased")
				apiWorkloadDelta(t, r.db, "terminalize", live, terminal)
				apiWorkloadRegistryPlans(t, r, o, bindings[0], rooms[0], "bound")
				measured := &voiceAuthRuntime{db: r.db}
				var memoryBefore, memoryAfter runtime.MemStats
				runtime.ReadMemStats(&memoryBefore)
				pageStarted := time.Now()
				for _, attempt := range []int{1, 103} {
					candidate := o
					candidate.attempts = attempt
					var all []uuid.UUID
					for _, offset := range []int{0, 100} {
						page, err := measured.loadOwnerFamilyPage(t.Context(), candidate, offset)
						require.NoError(t, err)
						if offset == 0 {
							require.Len(t, page, 100)
						} else {
							require.Len(t, page, 2)
						}
						all = append(all, page...)
					}
					require.Len(t, all, 102)
					seen := map[uuid.UUID]bool{}
					for _, family := range all {
						require.False(t, seen[family])
						seen[family] = true
					}
					for _, binding := range bindings {
						require.True(t, seen[uuid.MustParse(binding.FamilyID)])
					}
					page, err := measured.loadOwnerConnections(t.Context(), candidate)
					require.NoError(t, err)
					require.Len(t, page, 100)
					for _, binding := range page {
						require.Equal(t, r.userID.String(), binding.UserID)
						require.True(t, seen[uuid.MustParse(binding.FamilyID)])
					}
				}
				runtime.ReadMemStats(&memoryAfter)
				t.Logf("task8-page-loading elapsed=%s allocated_bytes=%d mallocs=%d scope=process-wide", time.Since(pageStarted), memoryAfter.TotalAlloc-memoryBefore.TotalAlloc, memoryAfter.Mallocs-memoryBefore.Mallocs)

				before := apiWorkloadSnapshot(t, r.db, "release-rooms-start")
				for n, p := range rooms {
					w, req := r.request(t, protocol.EndpointRoomRelease, roomBindingRelease(bindings[n], p), "")
					require.Equal(t, 204, w.Code, w.Body.String())
					r.verify(t, protocol.EndpointRoomRelease, w, req, protocol.ExpectedResponse{Binding: &bindings[n]})
				}
				w, req := r.request(t, protocol.EndpointRoomRelease, roomBindingRelease(channelBinding, channel), "")
				require.Equal(t, 204, w.Code, w.Body.String())
				r.verify(t, protocol.EndpointRoomRelease, w, req, protocol.ExpectedResponse{Binding: &channelBinding})
				after := apiWorkloadSnapshot(t, r.db, "rooms-released-parents-live")
				apiWorkloadDelta(t, r.db, "room-release", before, after)
				before = after
				for _, b := range append(bindings, channelBinding) {
					runtimeRelease(t, r, b, nil)
				}
				after = apiWorkloadSnapshot(t, r.db, "connections-released-active-boot-closures")
				apiWorkloadDelta(t, r.db, "parent-release", before, after)
				// Analyze/EXPLAIN are outside the measured mutation windows.
				apiWorkloadRegistryPlans(t, r, o, bindings[0], rooms[0], "released")
				before = apiWorkloadSnapshot(t, r.db, "retirement-GC-start")
				require.Zero(t, retentionReceiptGC(t, r).DeletedConnectionClosures)
				for _, boot := range boots {
					r.target.NodeBootID = boot
					retentionReceiptRetire(t, r)
				}
				var collectedConnections, collectedRooms int64
				for n := 0; n < 4; n++ {
					got, err := loginfamily.SweepRetiredEvidence(t.Context(), r.db, 100)
					require.NoError(t, err)
					collectedConnections += got.DeletedConnectionClosures
					collectedRooms += got.DeletedRoomClosures
				}
				require.Zero(t, collectedConnections, "unsettled owner retains closures after boot retirement")
				require.Zero(t, collectedRooms, "unsettled owner retains room closures")
				var retainedConnections, retainedRooms int
				require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM voice_auth_connection_closures WHERE node_boot_id=ANY($1::uuid[])`, pq.Array(boots)).Scan(&retainedConnections))
				require.Equal(t, 103, retainedConnections)
				require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM voice_auth_room_closures WHERE node_boot_id=ANY($1::uuid[])`, pq.Array(boots)).Scan(&retainedRooms))
				require.Equal(t, 103, retainedRooms)

				after = apiWorkloadSnapshot(t, r.db, "retired-pending-owner-closures-and-usable-families-retained")
				apiWorkloadDelta(t, r.db, "retirement-GC", before, after)
				if kind == "epoch" {
					require.NoError(t, r.db.QueryRow(`SELECT credential_epoch FROM users WHERE id=$1`, r.userID).Scan(&r.epoch))
				}
				base = apiWorkloadSnapshot(t, r.db, "next-round-baseline")
			}
			// Unregistered provisional close is closure evidence, never a live table.
			r.target.NodeBootID = apiWorkloadBoot(t, r)
			before := apiWorkloadSnapshot(t, r.db, "provisional-start")
			for n := 0; n < 12; n++ {
				r.target.ConnectionGeneration = uuid.NewString()
				r.target.SocketID = fmt.Sprintf("provisional-%d", n)
				wire, _ := r.signed(t, protocol.EndpointConnectionRegister, r.registration(), r.token)
				payload := closureRequest(r, wire)
				w, request := r.request(t, protocol.EndpointProvisionalClose, payload, "")
				require.Equal(t, 200, w.Code, w.Body.String())
				r.verify(t, protocol.EndpointProvisionalClose, w, request, protocol.ExpectedResponse{})
			}
			after := apiWorkloadSnapshot(t, r.db, "unregistered-provisional12-closures")
			apiWorkloadDelta(t, r.db, "provisional-close", before, after)
			before = after
			retentionReceiptRetire(t, r)
			got, err := loginfamily.SweepRetiredEvidence(t.Context(), r.db, 100)
			require.NoError(t, err)
			require.EqualValues(t, 12, got.DeletedConnectionClosures)
			after = apiWorkloadSnapshot(t, r.db, "provisional-retired-collected-permanent-fences-retained")
			apiWorkloadDelta(t, r.db, "provisional-GC", before, after)
		})
	}
}

func TestVoiceAuthQueryWorkloadDueParentPlans(t *testing.T) {
	r := newVoiceAuthRig(t)
	boot := apiWorkloadBoot(t, r)
	var authOwners []dmblock.AuthRevocation
	for n := 0; n < 102; n++ {
		subject := apiWorkloadSubject(t, r)
		apiWorkloadMint(t, subject, boot, n)
		owner := runtimeRevoke(t, subject)
		authOwners = append(authOwners, owner)
		// Fixture scheduling only. Completion follows the existing completion fixture
		// convention; no new delivery completion claim is made by this measurement.
		// Due timestamps must preserve the durable reconcile_after >= created_at
		// constraint; canonical creation time is already due without backdating.
		query := `UPDATE login_family_revocations SET reconcile_after=created_at WHERE operation_id=$1`
		if n == 100 {
			query = `UPDATE login_family_revocations SET reconcile_after=clock_timestamp()+INTERVAL '1 hour' WHERE operation_id=$1`
		}
		if n == 101 {
			query = `UPDATE login_family_revocations SET completed_at=clock_timestamp(),updated_at=clock_timestamp(),reconcile_after=created_at WHERE operation_id=$1`
		}
		_, err := r.db.Exec(query, owner.OperationID)
		require.NoError(t, err)
	}
	var epochs []loginfamily.EpochOwner
	for n := 0; n < 12; n++ {
		subject := apiWorkloadSubject(t, r)
		apiWorkloadMint(t, subject, boot, 200+n)
		owner := runtimeRotateEpoch(t, subject)
		epochs = append(epochs, owner)
		due := "created_at"
		if n >= 10 {
			due = "clock_timestamp()+INTERVAL '1 hour'"
		}
		_, err := r.db.Exec(`UPDATE credential_epoch_voice_ejections SET reconcile_after=`+due+` WHERE generation=$1`, owner.Generation)
		require.NoError(t, err)
	}
	for n := 0; n < 12; n++ {
		p := roomBindingFixture(t, r, "dm")
		tx, err := r.db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		_, err = dmblock.PrepareConversationTx(t.Context(), tx, p.RoomID, []uuid.UUID{r.userID}, dmblock.LockNoKeyUpdate, dmblock.LockUpdate)
		require.NoError(t, err)
		_, err = tx.Exec(`DELETE FROM dm_participants WHERE conversation_id=$1 AND user_id=$2`, p.RoomID, r.userID)
		require.NoError(t, err)
		require.NoError(t, dmblock.EnqueueVoiceEjectionsTx(t.Context(), tx, p.RoomID, []uuid.UUID{r.userID}))
		require.NoError(t, tx.Commit())
		interval := "-INTERVAL '1 hour'"
		if n >= 10 {
			interval = "+INTERVAL '1 hour'"
		}
		_, err = r.db.Exec(`UPDATE dm_block_voice_ejections SET reconcile_after=clock_timestamp()`+interval+` WHERE conversation_id=$1 AND user_id=$2`, p.RoomID, r.userID)
		require.NoError(t, err)
	}
	apiWorkloadSnapshot(t, r.db, "due-parent-populated102-auth12-epoch12-DM")
	apiWorkloadAnalyze(t, r.db)
	var due, future, completed int
	require.NoError(t, r.db.QueryRow(`SELECT count(*) FILTER(WHERE completed_at IS NULL AND reconcile_after<=clock_timestamp()),count(*) FILTER(WHERE completed_at IS NULL AND reconcile_after>clock_timestamp()),count(*) FILTER(WHERE completed_at IS NOT NULL) FROM login_family_revocations`).Scan(&due, &future, &completed))
	require.Equal(t, 100, due)
	require.Equal(t, 1, future)
	require.Equal(t, 1, completed)
	for _, table := range []string{"credential_epoch_voice_ejections", "dm_block_voice_ejections"} {
		var due, future int
		require.NoError(t, r.db.QueryRow(`SELECT count(*) FILTER(WHERE reconcile_after<=clock_timestamp()),count(*) FILTER(WHERE reconcile_after>clock_timestamp()) FROM `+table).Scan(&due, &future))
		require.Equal(t, 10, due)
		require.Equal(t, 2, future)
	}
	t.Log("task8-due-classification auth=100due/1future/1fixture-completed epoch=10due/2future DM=10due/2future; timestamp-adjustments=fixture-only; epoch-and-DM12=diagnostic-not-index-scalability-proof")
	file := "../dmblock/dmblock.go"
	plan := func(fn, id string, branch int, args ...any) {
		apiWorkloadPlan(t, r.db, "due/"+fn, apiWorkloadQuery(t, file, fn, id, branch), args...)
	}
	plan("listDueAuthRevocations", "SELECT operation_id", 0, 100)
	var originalAttempts int
	var originalDue time.Time
	require.NoError(t, r.db.QueryRow(`SELECT attempts,reconcile_after FROM login_family_revocations WHERE operation_id=$1`, authOwners[0].OperationID).Scan(&originalAttempts, &originalDue))
	plan("claimAuthRevocation", "WITH due AS (", 0, authOwners[0].OperationID)
	var afterAttempts int
	var afterDue time.Time
	require.NoError(t, r.db.QueryRow(`SELECT attempts,reconcile_after FROM login_family_revocations WHERE operation_id=$1`, authOwners[0].OperationID).Scan(&afterAttempts, &afterDue))
	require.Equal(t, originalAttempts, afterAttempts)
	require.True(t, originalDue.Equal(afterDue), "EXPLAIN executes UPDATE; disposable transaction must restore due time")
	plan("claimDueCredentialEpochEjections", "SELECT user_id, credential_epoch", 1, 100)
	plan("claimDueCredentialEpochEjections", "SELECT user_id, credential_epoch", 0, epochs[0].UserID, epochs[0].CredentialEpoch, epochs[0].SupersededCredentialEpoch, 100)
	plan("claimDueVoiceEjections", "SELECT conversation_id, user_id", 0, 100)
}
