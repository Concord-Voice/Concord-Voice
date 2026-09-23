package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/database"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
)

// runVoiceEnforcementRollout is deliberately a local, direct-DB operator
// command. There is no HTTP mutation surface: possession of deployment DB
// credentials is the authority to activate or deactivate the protocol.
func runVoiceEnforcementRollout(args []string) int {
	if len(args) == 0 ||
		(args[0] != "activate" && args[0] != "deactivate" && args[0] != "status") ||
		(args[0] == "activate" && (len(args) != 2 || args[1] != "--confirm-drained")) ||
		(args[0] != "activate" && len(args) != 1) {
		fmt.Fprintln(os.Stderr, "usage: control-plane voice-enforcement-rollout activate --confirm-drained|deactivate|status")
		return 64
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load configuration: %v\n", err)
		return 1
	}
	db, err := database.New(cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open database: %v\n", err)
		return 1
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "close voice enforcement database: %v\n", closeErr)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	switch args[0] {
	case "activate":
		if _, err := db.ExecContext(ctx, `
			UPDATE voice_enforcement_rollout
			SET activated_at = clock_timestamp(), updated_at = clock_timestamp()
			WHERE id = TRUE AND activated_at IS NULL`); err != nil {
			fmt.Fprintf(os.Stderr, "activate voice enforcement rollout: %v\n", err)
			return 1
		}
	case "deactivate":
		if _, err := db.ExecContext(ctx, `
			UPDATE voice_enforcement_rollout
			SET activated_at = NULL, updated_at = clock_timestamp()
			WHERE id = TRUE`); err != nil {
			fmt.Fprintf(os.Stderr, "deactivate voice enforcement rollout: %v\n", err)
			return 1
		}
	}
	var (
		activatedAt          *time.Time
		registryRows         int64
		dmParentRows         int64
		credentialParentRows int64
		dmReconciliationRows int64
	)
	if err := db.QueryRowContext(ctx, `
		SELECT activated_at,
		       (SELECT count(*) FROM voice_enforcement_sessions),
		       (SELECT count(*) FROM dm_block_voice_ejections),
		       (SELECT count(*) FROM credential_epoch_voice_ejections),
		       (SELECT count(*) FROM dm_block_reconciliations)
		FROM voice_enforcement_rollout
		WHERE id = TRUE`).Scan(&activatedAt, &registryRows, &dmParentRows, &credentialParentRows, &dmReconciliationRows); err != nil {
		fmt.Fprintf(os.Stderr, "read voice enforcement rollout: %v\n", err)
		return 1
	}
	if activatedAt == nil {
		fmt.Println("voice-enforcement-rollout: inactive")
	} else {
		fmt.Printf("voice-enforcement-rollout: active since %s\n", activatedAt.UTC().Format(time.RFC3339Nano))
	}
	fmt.Printf("voice-enforcement-rollout: registryRows=%d\n", registryRows)
	fmt.Printf("voice-enforcement-rollout: dmParentRows=%d\n", dmParentRows)
	fmt.Printf("voice-enforcement-rollout: credentialParentRows=%d\n", credentialParentRows)
	fmt.Printf("voice-enforcement-rollout: dmReconciliationRows=%d\n", dmReconciliationRows)
	return 0
}
