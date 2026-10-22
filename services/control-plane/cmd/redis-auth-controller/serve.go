package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/database"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedredis"
)

const (
	controllerRoot       = "/run/redis-auth"
	controlSocket        = controllerRoot + "/control.sock"
	controlSocketName    = "control.sock"
	aclFile              = controllerRoot + "/acl/users.acl"
	journalFile          = controllerRoot + "/state/rotation.json"
	activationMarkerFile = "/var/lib/concord/redis-auth/activated"
)

var errServe = errors.New("managed Redis controller unavailable")

func managedSetgid(mode os.FileMode) bool { return runtime.GOOS != "linux" || mode&os.ModeSetgid != 0 }

func requiredEnv(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", errServe
	}
	return value, nil
}

func requiredUID(name string) (int, error) {
	value, err := requiredEnv(name)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > 65535 {
		return 0, errServe
	}
	return n, nil
}

func requiredPeerUID(name string) (uint32, error) {
	value, err := requiredEnv(name)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(value, 10, 32)
	if err != nil || n < 1 || n > 65535 {
		return 0, errServe
	}
	return uint32(n), nil
}

func secretScope(project, environment, path, name string) managedredis.SecretScope {
	return managedredis.SecretScope{Project: project, Environment: environment, Path: path, Name: name}
}

func serve() (retErr error) {
	if os.Getenv("REDIS_CREDENTIAL_MODE") != "managed" {
		return errServe
	}
	input, err := readPrepareInputs()
	if err != nil {
		return errServe
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveConfigured(ctx, input, managedredis.ClientSecretFile, controllerRoot, activationMarkerFile)
}

func serveConfigured(ctx context.Context, input prepareInputs, clientSecretPath, root, markerPath string) (retErr error) {
	tlsMode := os.Getenv("REDIS_TLS")
	if tlsMode != "true" && tlsMode != "false" {
		return errServe
	}
	tlsConfig, err := database.ManagedRedisTLSConfig(input.endpoint, tlsMode == "true", os.Getenv("REDIS_TLS_CA_FILE"))
	if err != nil {
		return errServe
	}
	baseURL := os.Getenv("REDIS_INFISICAL_URL")
	if baseURL == "" {
		baseURL = "https://app.infisical.com"
	}
	provider, err := managedredis.NewInfisicalReader(baseURL, input.clientID, clientSecretPath, nil)
	if err != nil {
		return errServe
	}
	bootstrapScope := secretScope(input.project, input.environment, input.path, "REDIS_ADMIN_BOOTSTRAP")
	fetchCtx, cancel := context.WithTimeout(ctx, 16*time.Second)
	bootstrapRaw, name, err := provider.ReadExact(fetchCtx, bootstrapScope)
	cancel()
	if err != nil || name != bootstrapScope.Name {
		return errServe
	}
	bootstrap, err := managedredis.ParseAdminBootstrap(bootstrapRaw, bootstrapScope)
	if err != nil {
		return errServe
	}
	admin, err := newRedisAdmin(input.endpoint, tlsConfig, bootstrap)
	if err != nil {
		return errServe
	}
	defer func() {
		if err := admin.client.Close(); err != nil {
			retErr = errServe
		}
	}()
	return serveRuntime(ctx, input, provider, admin, bootstrap, servePaths{root: root, markerPath: markerPath})
}

type servePaths struct {
	root, markerPath string
}

func serveRuntime(ctx context.Context, input prepareInputs, provider exactSecretReader, admin managedredis.RedisAdmin, bootstrap managedredis.AdminBootstrap, paths servePaths) (retErr error) {
	aclPath := filepath.Join(paths.root, "acl/users.acl")
	journalPath := filepath.Join(paths.root, "state/rotation.json")
	aclDir, err := os.Stat(filepath.Dir(aclPath))
	if err != nil || aclDir.Mode().Perm() != 0o710 || !managedSetgid(aclDir.Mode()) {
		return errServe
	}
	controller, err := managedredis.NewController(managedredis.ControllerOptions{
		Admin: admin, ACLPath: aclPath, JournalPath: journalPath, MarkerPath: paths.markerPath, Bootstrap: bootstrap, Now: time.Now,
		ExpectedUIDs: map[string]uint32{"control-plane": input.controlUID, "media-plane": input.mediaUID},
		ACLUID:       os.Geteuid(), ACLGID: input.aclGID,
	})
	if err != nil || controller.Restore() != nil {
		return errServe
	}
	if controller.RevocationPending() {
		listener, err := listenSocket(filepath.Join(paths.root, controlSocketName))
		if err != nil {
			return errServe
		}
		defer func() {
			if err := listener.Close(); err != nil {
				retErr = errServe
			}
		}()
		go serveSocketWithHandler(ctx, listener, controller, "", peerUID, handleRecoveryControl)
		<-ctx.Done()
		return nil
	}
	if controller.ReconciliationPending() {
		if err := resumeReconciledJournal(ctx, controller, paths.root); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return errServe
		}
	}
	policyScope := secretScope(input.project, input.environment, input.path, "REDIS_AUTH_POLICY")
	startupCtx, cancel := context.WithTimeout(ctx, 16*time.Second)
	raw, name, readErr := provider.ReadExact(startupCtx, policyScope)
	startupErr := startupCtx.Err()
	cancel()
	if readErr != nil || startupErr != nil || name != policyScope.Name {
		log.Printf("managed Redis controller startup policy failed: provider")
		return errServe
	}
	policy, parseErr := managedredis.ParsePolicy(raw, policyScope, time.Now())
	if parseErr != nil {
		log.Printf("managed Redis controller startup policy failed: policy")
		return errServe
	}
	if controller.ValidateRestoredPolicy(policy) != nil {
		log.Printf("managed Redis controller startup policy failed: reconcile")
		return errServe
	}
	if err := waitForBootstrap(ctx, controller, paths.root); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return errServe
	}
	listeners, err := openListeners(ctx, controller, paths.root)
	if err != nil {
		return err
	}
	defer func() {
		for _, listener := range listeners {
			if err := listener.Close(); err != nil {
				retErr = errServe
			}
		}
	}()
	reconcileDone := make(chan struct{})
	go func() {
		defer close(reconcileDone)
		reconcileLoop(ctx, controller, provider, policyScope)
	}()
	<-ctx.Done()
	<-reconcileDone
	return nil
}

func resumeReconciledJournal(ctx context.Context, controller *managedredis.Controller, root string) (retErr error) {
	listener, err := listenSocket(filepath.Join(root, controlSocketName))
	if err != nil {
		return errServe
	}
	defer func() {
		if err := listener.Close(); err != nil {
			retErr = errServe
		}
	}()
	go serveSocketWithHandler(ctx, listener, controller, "", peerUID, handleRecoveryControl)
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := controller.ResumeReconcile(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// waitForBootstrap keeps application sockets closed until Redis accepts the
// current fetched bootstrap identity. Redis itself starts after this process,
// not its health check, so this bounded status socket breaks that dependency
// cycle without admitting application readiness.
func waitForBootstrap(ctx context.Context, controller *managedredis.Controller, root string) (retErr error) {
	listener, err := listenSocket(filepath.Join(root, controlSocketName))
	if err != nil {
		return errServe
	}
	defer func() {
		if err := listener.Close(); err != nil && retErr == nil {
			retErr = errServe
		}
	}()
	go serveSocketWithHandler(ctx, listener, controller, "", peerUID, handleRecoveryControl)
	retry := time.NewTicker(time.Second)
	defer retry.Stop()
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := controller.ProbeConfigured(probeCtx)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-retry.C:
		}
	}
}

func openListeners(ctx context.Context, controller *managedredis.Controller, root string) ([]*net.UnixListener, error) {
	listeners := make([]*net.UnixListener, 0, 5)
	for _, socket := range []struct {
		path string
		slot string
	}{
		{filepath.Join(root, controlSocketName), ""}, {filepath.Join(root, "acks/control-plane/ack-control.sock"), "control-plane"},
		{filepath.Join(root, "acks/media-plane/ack-media.sock"), "media-plane"},
		{filepath.Join(root, "acks/media-plane-blue/ack-media.sock"), "media-plane-blue"},
		{filepath.Join(root, "acks/media-plane-green/ack-media.sock"), "media-plane-green"},
	} {
		listener, err := listenSocket(socket.path)
		if err != nil {
			for _, prior := range listeners {
				if prior.Close() != nil {
					log.Printf("managed Redis controller listener cleanup failed")
				}
			}
			return nil, errServe
		}
		listeners = append(listeners, listener)
		go serveSocket(ctx, listener, controller, socket.slot)
	}
	return listeners, nil
}

func reconcileLoop(ctx context.Context, controller *managedredis.Controller, provider exactSecretReader, scope managedredis.SecretScope) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		code := reconcileOnce(ctx, controller, provider, scope)
		if code != "" {
			log.Printf("managed Redis controller refresh failed: %s", code)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func reconcileOnce(ctx context.Context, controller *managedredis.Controller, provider exactSecretReader, scope managedredis.SecretScope) string {
	attemptCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	raw, name, err := provider.ReadExact(attemptCtx, scope)
	code := ""
	if err != nil || name != scope.Name {
		code = "provider"
	} else {
		policy, parseErr := managedredis.ParsePolicy(raw, scope, time.Now())
		if parseErr != nil {
			code = "policy"
		} else if reconcileErr := controller.Reconcile(attemptCtx, policy); reconcileErr != nil {
			code = "reconcile"
			if errors.Is(reconcileErr, managedredis.ErrPolicyStale) {
				code = "policy-stale"
			}
		}
	}
	if code != "" {
		controller.ReportFailure(code)
	}
	return code
}

func listenSocket(path string) (*net.UnixListener, error) {
	dir, err := os.Lstat(filepath.Dir(path))
	if err != nil || !dir.IsDir() || dir.Mode()&os.ModeSymlink != 0 || dir.Mode().Perm() != 0o710 || !managedSetgid(dir.Mode()) {
		return nil, errServe
	}
	stat, ok := dir.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return nil, errServe
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errServe
		}
		if err := os.Remove(path); err != nil {
			return nil, errServe
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errServe
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, errServe
	}
	if err := os.Chmod(path, 0o660); err != nil { // #nosec G302 -- isolated setgid socket; peer UID authorizes every request.
		return nil, closeInvalidSocket(listener)
	}
	bound, err := os.Lstat(path)
	if err != nil || bound.Mode()&os.ModeSocket == 0 || bound.Mode().Perm() != 0o660 {
		return nil, closeInvalidSocket(listener)
	}
	boundStat, ok := bound.Sys().(*syscall.Stat_t)
	if !ok || int(boundStat.Uid) != os.Geteuid() || boundStat.Gid != stat.Gid {
		return nil, closeInvalidSocket(listener)
	}
	return listener, nil
}

func closeInvalidSocket(listener *net.UnixListener) error {
	if listener.Close() != nil {
		log.Printf("managed Redis controller listener cleanup failed")
	}
	return errServe
}

func serveSocket(ctx context.Context, listener *net.UnixListener, controller *managedredis.Controller, slot string) {
	serveSocketWithPeerUID(ctx, listener, controller, slot, peerUID)
}

func serveSocketWithPeerUID(ctx context.Context, listener *net.UnixListener, controller *managedredis.Controller, slot string, peerIdentity func(*net.UnixConn) (uint32, error)) {
	serveSocketWithHandler(ctx, listener, controller, slot, peerIdentity, handleControl)
}

func serveSocketWithHandler(ctx context.Context, listener *net.UnixListener, controller *managedredis.Controller, slot string, peerIdentity func(*net.UnixConn) (uint32, error), controlHandler func(context.Context, *managedredis.Controller, uint32, []byte) []byte) {
	sem := make(chan struct{}, 32)
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		select {
		case sem <- struct{}{}:
		default:
			if conn.Close() != nil {
				log.Printf("managed Redis controller connection cleanup failed")
			}
			continue
		}
		go func() {
			defer func() { <-sem }()
			defer func() {
				if err := conn.Close(); err != nil {
					log.Printf("managed Redis controller connection cleanup failed")
				}
			}()
			deadline := time.Now().Add(5 * time.Second)
			if err := conn.SetDeadline(deadline); err != nil {
				return
			}
			requestCtx, cancel := context.WithDeadline(ctx, deadline)
			defer cancel()
			uid, err := peerIdentity(conn)
			if err != nil {
				return
			}
			line, err := bufio.NewReader(io.LimitReader(conn, maxControlLine+2)).ReadBytes('\n')
			if err != nil || len(line) > maxControlLine {
				return
			}
			var reply []byte
			if slot == "" {
				reply = controlHandler(requestCtx, controller, uid, line)
			} else {
				reply = handleAck(requestCtx, controller, slot, uid, line)
			}
			if _, err := conn.Write(reply); err != nil {
				log.Printf("managed Redis controller reply delivery failed")
			}
		}()
	}
}

func request(input io.Reader, output io.Writer, socketPath string) (retErr error) {
	line, err := bufio.NewReader(io.LimitReader(input, maxControlLine+2)).ReadBytes('\n')
	if err != nil || len(line) > maxControlLine {
		return errServe
	}
	var command controlRequest
	if decodeControlLine(line, &command) != nil {
		return errServe
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).Dial("unix", socketPath)
	if err != nil {
		return errServe
	}
	defer func() {
		if err := conn.Close(); err != nil {
			retErr = errServe
		}
	}()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return errServe
	}
	if _, err := conn.Write(line); err != nil {
		return errServe
	}
	reply, err := bufio.NewReader(io.LimitReader(conn, maxControlReplyLine+2)).ReadBytes('\n')
	if err != nil || len(reply) > maxControlReplyLine {
		return errServe
	}
	var result map[string]any
	if decodeControlLine(reply, &result) != nil || result["error"] != nil {
		return errServe
	}
	if _, err := output.Write(reply); err != nil {
		return errServe
	}
	return nil
}
