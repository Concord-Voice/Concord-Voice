package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voicecommand"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
)

const voiceCommandOwnerRecoveryBudget = 10 * time.Second
const voiceCommandOwnerRecoveryUsage = "usage: control-plane voice-command-owner recover --expected-revision N --expected-boot-id UUID --confirm-predecessor-stopped"

type voiceCommandOwnerRecoveryOptions struct {
	ExpectedRevision          uint64
	ExpectedBootID            uuid.UUID
	ConfirmPredecessorStopped bool
}

// parseVoiceCommandOwnerRecovery admits only an exact owner and explicit proof.
// No configuration, database, or network operation occurs before this boundary.
func parseVoiceCommandOwnerRecovery(args []string) (voiceCommandOwnerRecoveryOptions, error) {
	var options voiceCommandOwnerRecoveryOptions
	if len(args) == 0 || args[0] != "recover" {
		return options, errors.New("voice command owner recovery requires recover")
	}
	flags := flag.NewFlagSet("voice-command-owner recover", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var revision, boot string
	flags.StringVar(&revision, "expected-revision", "", "exact current KV revision")
	flags.StringVar(&boot, "expected-boot-id", "", "exact predecessor boot identity")
	flags.BoolVar(&options.ConfirmPredecessorStopped, "confirm-predecessor-stopped", false, "old process and media workers are stopped and cannot restart")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || !options.ConfirmPredecessorStopped {
		return voiceCommandOwnerRecoveryOptions{}, errors.New("voice command owner recovery requires exact options and predecessor-stop confirmation")
	}
	parsedRevision, err := strconv.ParseUint(revision, 10, 64)
	if err != nil || parsedRevision == 0 || strconv.FormatUint(parsedRevision, 10) != revision {
		return voiceCommandOwnerRecoveryOptions{}, errors.New("voice command owner recovery revision is not a positive canonical decimal")
	}
	parsedBoot, err := uuid.Parse(boot)
	if err != nil || parsedBoot.String() != boot || parsedBoot.Variant() != uuid.RFC4122 ||
		parsedBoot.Version() < 1 || parsedBoot.Version() > 8 {
		return voiceCommandOwnerRecoveryOptions{}, errors.New("voice command owner recovery boot identity is not a canonical UUID")
	}
	options.ExpectedRevision, options.ExpectedBootID = parsedRevision, parsedBoot
	return options, nil
}

// runVoiceCommandOwnerRecovery is an operator-only local command. The exact held
// record and receipt remain authoritative; elapsed time never supplies stop proof.
func runVoiceCommandOwnerRecovery(args []string) int {
	options, err := parseVoiceCommandOwnerRecovery(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, voiceCommandOwnerRecoveryUsage)
		return 64
	}
	ctx, cancel := context.WithTimeout(context.Background(), voiceCommandOwnerRecoveryBudget)
	defer cancel()
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "voice-command-owner: configuration unavailable")
		return 1
	}
	db, err := openVoiceCommandRecoveryDB(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "voice-command-owner: database unavailable")
		return 1
	}
	defer func() {
		if err := db.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "voice-command-owner: database close failed")
		}
	}()
	if err := recoverVoiceCommandOwner(ctx, db, cfg.NATSUrl, options); err != nil {
		// Connection/configuration errors can contain URL credentials. The
		// command emits a fixed refusal rather than writing that error to stderr.
		fmt.Fprintln(os.Stderr, "voice-command-owner: recovery refused; verify transport continuity and exact stopped predecessor")
		return 1
	}
	fmt.Println("voice-command-owner: ownership released")
	return 0
}

func openVoiceCommandRecoveryDB(ctx context.Context, databaseURL string) (*sql.DB, error) {
	configuration, err := pq.NewConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse voice command recovery database configuration: %w", err)
	}
	configuration.ConnectTimeout = voiceCommandOwnerRecoveryBudget
	if deadline, bounded := ctx.Deadline(); bounded {
		configuration.ConnectTimeout = time.Until(deadline)
	}
	if configuration.ConnectTimeout <= 0 {
		return nil, context.DeadlineExceeded
	}
	connector, err := pq.NewConnectorConfig(configuration)
	if err != nil {
		return nil, fmt.Errorf("construct voice command recovery database connector: %w", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("connect voice command recovery database: %w", err), db.Close())
	}
	return db, nil
}

func recoverVoiceCommandOwner(ctx context.Context, db *sql.DB, url string, options voiceCommandOwnerRecoveryOptions) (returnErr error) {
	if !options.ConfirmPredecessorStopped {
		return errors.New("voice command owner recovery lacks physical-stop proof")
	}
	receipt, err := voicecommand.LoadReceipt(ctx, db)
	if err != nil {
		return err
	}
	client, err := natsclient.NewVoiceCommandClient(ctx, url)
	if err != nil {
		return err
	}
	defer func() {
		if err := client.Close(); err != nil && !errors.Is(err, natsclient.ErrDrainNothingToDo) {
			returnErr = errors.Join(returnErr, errors.New("close voice command recovery broker failed"))
		}
	}()
	if err := client.Verify(ctx, receipt); err != nil {
		return err
	}
	return client.RecoverVoiceCommandOwner(ctx, options.ExpectedRevision, options.ExpectedBootID, receipt)
}
