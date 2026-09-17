package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/store"
)

var (
	importOlmSourceType         string
	importOlmSkipFolders        []string
	importOlmNoResume           bool
	importOlmCheckpointInterval int
	importOlmNoAttachments      bool
	importOlmNoDefaultIdentity  bool
)

var importOlmCmd = &cobra.Command{
	Use:   "import-olm <identifier> <olm-file>",
	Short: "Import an Outlook for Mac (.olm) archive into msgvault",
	Long: `Import an Outlook for Mac .olm export into msgvault.

All mail messages are imported. Calendar items, contacts, notes, and tasks
are skipped. The Outlook folder structure is preserved as labels (e.g. the
Inbox folder becomes the "Inbox" label). OLM exports do not carry the
original transport headers, so From, To, Date, Subject, and Message-ID are
rebuilt from the exported fields.

The import is resumable: if interrupted with Ctrl+C, rerunning with the same
arguments continues from where it left off. Re-importing the same file skips
messages that are already archived. Use --no-resume to start fresh.

Examples:
  msgvault import-olm you@company.com ~/Desktop/export.olm
  msgvault import-olm you@company.com export.olm --skip-folder "Deleted Items" --skip-folder "Junk Email"
  msgvault import-olm you@company.com export.olm --no-resume
`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, args, nil)
		}

		identifier := args[0]
		olmPath := args[1]

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		st, cleanup, err := openWritableStoreAndInitForIngest()
		if err != nil {
			return err
		}
		defer cleanup()

		attachmentsDir := cfg.AttachmentsDir()
		if importOlmNoAttachments {
			attachmentsDir = ""
		}
		dbPath := cfg.DatabaseDSN()

		summary, importErr := importer.ImportOlm(ctx, st, olmPath, importer.OlmImportOptions{
			SourceType:         importOlmSourceType,
			Identifier:         identifier,
			SkipFolders:        importOlmSkipFolders,
			NoResume:           importOlmNoResume,
			CheckpointInterval: importOlmCheckpointInterval,
			AttachmentsDir:     attachmentsDir,
			RemoteImages:       configuredRemoteImageFetcher(),
			Logger:             logger,
		})
		if importErr != nil {
			return errors.Join(importErr, rebuildCacheAfterWrite(dbPath))
		}
		if err := runOlmPostImportMigrations(cmd.OutOrStdout(), st, summary, importOlmSourceType, identifier, importOlmNoDefaultIdentity); err != nil {
			return errors.Join(err, rebuildCacheAfterWrite(dbPath))
		}

		out := cmd.OutOrStdout()
		switch {
		case ctx.Err() != nil:
			_, _ = fmt.Fprintln(out, "Import interrupted. Run again to resume.")
		case summary.HardErrors:
			_, _ = fmt.Fprintln(out, "Import complete (with errors).")
		default:
			_, _ = fmt.Fprintln(out, "Import complete.")
		}
		if summary.WasResumed {
			_, _ = fmt.Fprintln(out, "  Resumed from checkpoint.")
		}
		_, _ = fmt.Fprintf(out, "  File:           %s\n", olmPath)
		_, _ = fmt.Fprintf(out, "  Folders:        %d / %d\n", summary.FoldersImported, summary.FoldersTotal)
		_, _ = fmt.Fprintf(out, "  Processed:      %d messages\n", summary.MessagesProcessed)
		_, _ = fmt.Fprintf(out, "  Added:          %d messages\n", summary.MessagesAdded)
		_, _ = fmt.Fprintf(out, "  Updated:        %d messages\n", summary.MessagesUpdated)
		_, _ = fmt.Fprintf(out, "  Skipped:        %d messages\n", summary.MessagesSkipped)
		_, _ = fmt.Fprintf(out, "  Errors:         %d\n", summary.Errors)
		_, _ = fmt.Fprintf(out, "  Duration:       %s\n", summary.Duration.Round(1e9))

		resultErr := ctx.Err()
		if resultErr != nil {
			resultErr = context.Canceled
		} else if summary.HardErrors {
			resultErr = fmt.Errorf("import completed with %d errors", summary.Errors)
		}
		return errors.Join(resultErr, rebuildCacheAfterWrite(dbPath))
	},
}

func init() {
	rootCmd.AddCommand(importOlmCmd)

	importOlmCmd.Flags().StringVar(&importOlmSourceType, "source-type", "olm", "Source type recorded in the database")
	importOlmCmd.Flags().StringArrayVar(&importOlmSkipFolders, "skip-folder", nil, "Folder name to skip (repeatable, case-insensitive)")
	importOlmCmd.Flags().BoolVar(&importOlmNoResume, "no-resume", false, "Do not resume from an interrupted import")
	importOlmCmd.Flags().IntVar(&importOlmCheckpointInterval, "checkpoint-interval", 200, "Save progress every N messages")
	importOlmCmd.Flags().BoolVar(&importOlmNoAttachments, "no-attachments", false, "Do not store attachments to disk (messages are still imported)")
	importOlmCmd.Flags().BoolVar(&importOlmNoDefaultIdentity, "no-default-identity", false, noDefaultIdentityHelp)
}

func runOlmPostImportMigrations(
	out io.Writer,
	st *store.Store,
	summary *importer.OlmImportSummary,
	sourceType string,
	identifier string,
	noDefaultIdentity bool,
) error {
	if summary == nil || summary.SourceID == 0 {
		return nil
	}
	// Auto-default-identity runs before the legacy migration retry so
	// migrated legacy [identity] rows cannot suppress the source's own
	// account identifier on a later resume (same ordering as import-pst).
	if !noDefaultIdentity && store.SourceTypeUsesEmailIdentity(sourceType) {
		confirmDefaultIdentity(out, st, summary.SourceID, identifier, identifier, "account-identifier")
	}
	if err := runPostSourceCreateMigrations(st); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}
	return nil
}
