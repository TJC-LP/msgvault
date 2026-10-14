package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"go.kenn.io/kit/packstore"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

const (
	automaticAttachmentBytes = int64(256 << 20)
	// Kit's Pack and Repack cannot stop at a checkpoint on request, so the
	// scheduled jobs are not preemptible. This limit alone returns the gate.
	automaticAttachmentMaxRuntime = time.Minute
	attachmentMaintenanceJob      = "attachment-maintenance"
	attachmentMaintenanceCron     = "17 3 * * *"
	// attachmentPackJob packs loose blobs in bounded background passes. It
	// runs on its schedule and whenever a manual ingest sent to the daemon
	// succeeds, after that request has returned. Follow-ups drain a loose
	// backlog larger than one pass's byte budget.
	attachmentPackJob  = "attachment-pack"
	attachmentPackCron = "41 */6 * * *"
	importMboxCommand  = "import-mbox"
)

// attachmentMaintenance coordinates daemon-owned attachment maintenance. Its
// callers already hold the daemon operation gate; these methods must not try
// to acquire it again.
type attachmentMaintenance struct {
	store               *store.Store
	maintainer          *packstore.Maintainer
	coordinator         *packstore.Coordinator
	blob                *attachmentstore.Store
	logger              *slog.Logger
	packCreationEnabled bool
	attachmentsDir      string
	// packPending records that an ingest wrote loose blobs that no pack pass
	// has drained yet. Startup requests one scan of existing blobs.
	packPending atomic.Bool
	// requestPackJob asks the daemon scheduler for a background
	// attachment-pack run. registerAttachmentPackJob sets it before the
	// daemon serves requests; without it a pending pass waits for the job's
	// schedule.
	requestPackJob func() error
}

func newAttachmentMaintenance(
	s *store.Store,
	attachmentsDir string,
	logger *slog.Logger,
	packCreationEnabled bool,
) (*attachmentMaintenance, error) {
	layout, err := packstore.NewLayout(attachmentsDir, packstore.LayoutOptions{
		Staging: packstore.StagingSameDirectory,
	})
	if err != nil {
		return nil, fmt.Errorf("create attachment maintenance layout: %w", err)
	}
	coordinator := packstore.NewCoordinator()
	maintainer, err := packstore.NewMaintainer(store.NewMaintenancePackCatalog(s, layout.Root()), layout, packstore.MaintainerOptions{
		Coordinator: coordinator,
	})
	if err != nil {
		return nil, fmt.Errorf("create attachment maintainer: %w", err)
	}
	return &attachmentMaintenance{
		store:               s,
		maintainer:          maintainer,
		coordinator:         coordinator,
		blob:                attachmentstore.Wrap(maintainer.Store()),
		logger:              logger,
		packCreationEnabled: packCreationEnabled,
		attachmentsDir:      attachmentsDir,
	}, nil
}

func (m *attachmentMaintenance) close() error {
	if m == nil || m.maintainer == nil {
		return nil
	}
	if err := m.maintainer.Close(); err != nil {
		return fmt.Errorf("close attachment maintainer: %w", err)
	}
	return nil
}

// pack performs one packer pass with the requested soft raw-byte budget.
func (m *attachmentMaintenance) pack(ctx context.Context, maxBytes int64) (packstore.PackStats, error) {
	if !m.packCreationEnabled {
		return packstore.PackStats{}, errors.New(
			"attachment pack creation is disabled by [data].loose_attachments",
		)
	}
	stats, err := m.maintainer.Pack(ctx, packstore.PackOptions{MaxBytes: maxBytes})
	if err != nil {
		return stats, fmt.Errorf("pack attachments: %w", err)
	}
	return stats, nil
}

// repack performs one physical-GC pass through the daemon's shared blob-store
// cache with the requested soft live-raw-byte budget.
func (m *attachmentMaintenance) repack(ctx context.Context, maxBytes int64) (packstore.RepackStats, error) {
	if !m.packCreationEnabled {
		return packstore.RepackStats{}, errors.New(
			"attachment pack creation is disabled by [data].loose_attachments",
		)
	}
	stats, err := m.maintainer.Repack(ctx, packstore.RepackOptions{MaxBytes: maxBytes})
	if err != nil {
		return stats, fmt.Errorf("repack attachments: %w", err)
	}
	return stats, nil
}

func (m *attachmentMaintenance) unpack(ctx context.Context) (packstore.UnpackStats, error) {
	stats, err := m.maintainer.Unpack(ctx)
	if err != nil {
		return stats, fmt.Errorf("unpack attachments: %w", err)
	}
	return stats, nil
}

// runAutomaticPack performs one pack pass with the automatic byte budget.
// Scheduled callers bound its verification work and runtime; errors remain
// visible to them.
func (m *attachmentMaintenance) runAutomaticPack(ctx context.Context) error {
	if !m.packCreationEnabled {
		m.log().Debug("automatic attachment packing disabled")
		return nil
	}
	start := time.Now()
	stats, err := m.pack(ctx, automaticAttachmentBytes)
	// Each counter is a committed change a later pass need not repeat.
	// PacksQuarantined is not: a damaged pack stays in place and is counted
	// again on every pass.
	if stats.BlobsPacked > 0 || stats.PacksAdopted > 0 || stats.PacksRemoved > 0 ||
		stats.MappingsPruned > 0 || stats.RecordsDropped > 0 || stats.LooseSwept > 0 ||
		stats.LooseOrphansRemoved > 0 {
		jobctx.RecordProgress(ctx)
	}
	duration := time.Since(start)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			m.log().Info("automatic attachment maintenance canceled")
			return err
		}

		m.logAutomaticPackSummary("automatic attachment maintenance progress", stats, duration)
		m.log().Warn("automatic attachment maintenance failed",
			"error", err,
			"retry", "run `msgvault pack-attachments` to retry")
		return err
	}

	m.logAutomaticPackSummary("automatic attachment maintenance complete", stats, duration)
	if stats.BudgetExhausted {
		m.markPackPending()
	}
	return nil
}

func (m *attachmentMaintenance) logAutomaticPackSummary(message string, stats packstore.PackStats, duration time.Duration) {
	m.log().Info(message,
		"duration", duration.Round(time.Millisecond),
		"max_bytes", automaticAttachmentBytes,
		"packs_sealed", stats.PacksSealed,
		"blobs_packed", stats.BlobsPacked,
		"bytes_packed", stats.BytesPacked,
		"packs_adopted", stats.PacksAdopted,
		"packs_removed", stats.PacksRemoved,
		"packs_quarantined", stats.PacksQuarantined,
		"packs_unreadable", stats.PacksUnreadable,
		"blobs_deferred_oversized", stats.BlobsDeferredOversized,
		"packs_deferred_oversized", stats.PacksDeferredOversized,
		"records_dropped", stats.RecordsDropped,
		"mappings_pruned", stats.MappingsPruned,
		"blobs_missing", stats.BlobsMissing,
		"blobs_corrupt", stats.BlobsCorrupt,
		"loose_swept", stats.LooseSwept,
		"loose_orphans_removed", stats.LooseOrphansRemoved,
		"budget_exhausted", stats.BudgetExhausted)
}

func (m *attachmentMaintenance) runAutomaticRepack(ctx context.Context, emitWarning func(string) error) error {
	if !m.packCreationEnabled {
		m.log().Debug("automatic attachment repacking disabled")
		return nil
	}
	start := time.Now()
	stats, err := m.repack(ctx, automaticAttachmentBytes)
	if stats.PacksRewritten > 0 || stats.PacksRemoved > 0 || stats.MappingsPruned > 0 {
		jobctx.RecordProgress(ctx)
	}
	duration := time.Since(start)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			m.log().Info("automatic attachment repack canceled")
			return err
		}
		m.logAutomaticRepackSummary("automatic attachment repack progress", stats, duration)
		const retry = "run `msgvault repack-attachments` to retry"
		m.log().Warn("automatic attachment repack failed", "error", err, "retry", retry)
		if emitWarning != nil {
			warning := fmt.Sprintf("Automatic attachment repack failed: %v; %s.\n", err, retry)
			if emitErr := emitWarning(warning); emitErr != nil {
				m.log().Warn("failed to emit automatic attachment repack warning", "error", emitErr)
			}
		}
		return err
	}
	m.logAutomaticRepackSummary("automatic attachment repack complete", stats, duration)
	return nil
}

func (m *attachmentMaintenance) logAutomaticRepackSummary(message string, stats packstore.RepackStats, duration time.Duration) {
	m.log().Info(message,
		"duration", duration.Round(time.Millisecond),
		"max_bytes", automaticAttachmentBytes,
		"mappings_pruned", stats.MappingsPruned,
		"packs_selected", stats.PacksSelected,
		"packs_rewritten", stats.PacksRewritten,
		"packs_sealed", stats.PacksSealed,
		"packs_removed", stats.PacksRemoved,
		"packs_deferred_oversized", stats.PacksDeferredOversized,
		"blobs_repacked", stats.BlobsRepacked,
		"bytes_repacked", stats.BytesRepacked,
		"budget_exhausted", stats.BudgetExhausted)
}

// runScheduledPack performs one pass that packs new loose blobs within the
// byte budget and verifies the next window of existing packed blobs, so its
// cost tracks new content rather than archive size. A pass that stops at its
// byte budget leaves packPending set. verifyMore reports that the durable
// verification cursor has further windows.
func (m *attachmentMaintenance) runScheduledPack(ctx context.Context) (verifyMore bool, err error) {
	if !m.packCreationEnabled {
		return false, nil
	}
	packCtx, pass, err := m.store.BeginPackVerification(ctx, 128, 32<<20)
	if err != nil {
		return false, err
	}
	if err := m.runAutomaticPack(packCtx); err != nil {
		return false, err
	}
	more, err := m.store.FinishPackVerification(ctx, pass)
	if err != nil {
		return false, err
	}
	if more {
		// A completed verification cycle restarts on the next request. Only an
		// unfinished window provides a resume point for a budgeted follow-up.
		jobctx.RecordProgress(ctx)
	}
	return more, nil
}

// daily runs the two bounded phases in order. Follow-ups finish the loose
// backlog and the whole verification cycle before the repack phase. A failed
// pack phase stops the job so the scheduler records the failure instead of
// obscuring it with a second maintenance result.
func (m *attachmentMaintenance) daily(ctx context.Context) error {
	m.packPending.Store(false)
	verifyMore, err := m.runScheduledPack(ctx)
	if err != nil {
		m.markPackPending()
		return err
	}
	if verifyMore || m.packPending.Load() {
		return scheduler.ErrReschedule
	}
	return m.runAutomaticRepack(ctx, nil)
}

// markPackPending records that loose blobs await the next pack pass.
func (m *attachmentMaintenance) markPackPending() {
	if m != nil {
		m.packPending.Store(true)
	}
}

// runPendingPack runs one automatic pack pass when an ingest left new loose
// blobs since the last pass. A failed pass leaves the request pending; an
// exhausted byte budget queues another pass behind waiting work. Each pass
// advances verification by one window but does not chain a whole
// verification cycle: on a large archive that is hundreds of passes, and the
// daily attachment-maintenance job completes cycles.
func (m *attachmentMaintenance) runPendingPack(ctx context.Context) error {
	if m == nil || !m.packPending.Swap(false) {
		return nil
	}
	if _, err := m.runScheduledPack(ctx); err != nil {
		m.packPending.Store(true)
		return err
	}
	if m.packPending.Load() {
		return scheduler.ErrReschedule
	}
	return nil
}

// requestPack records that a successful manual ingest may have left loose
// blobs and asks the attachment-pack job to pack them. It never waits: the
// job takes the operation gate after the ingest's request releases it, runs
// under the job's runtime limit, and counts as daemon activity while it
// waits and runs. Without a registered job the request stays pending.
func (m *attachmentMaintenance) requestPack() {
	if m == nil || !m.packCreationEnabled {
		return
	}
	m.markPackPending()
	if m.requestPackJob == nil {
		return
	}
	if err := m.requestPackJob(); err != nil {
		m.log().Info("background attachment packing not started; new blobs stay pending",
			"job", attachmentPackJob, "error", err)
	}
}

// looseBlobWrites reports the in-process count of loose blobs created under
// this maintenance's attachments directory.
func (m *attachmentMaintenance) looseBlobWrites() int64 {
	if m == nil || m.attachmentsDir == "" {
		return 0
	}
	return export.LooseBlobWrites(m.attachmentsDir)
}

func (m *attachmentMaintenance) log() *slog.Logger {
	if m != nil && m.logger != nil {
		return m.logger
	}
	return slog.Default()
}

// runAfterSuccessfulAttachmentIngest requests background packing only after
// a successful ingest. The caller's request returns as soon as the ingest
// does; packing can neither delay nor replace the ingest result.
func runAfterSuccessfulAttachmentIngest(
	ctx context.Context,
	maintenance *attachmentMaintenance,
	ingest func(context.Context) error,
) error {
	if err := runWithAttachmentMutation(ctx, maintenance, ingest); err != nil {
		return err
	}
	maintenance.requestPack()
	return nil
}

// runAfterSuccessfulAttachmentRemoval runs bounded physical GC only after a
// successful removal. Repack and warning-stream failures never replace the
// already committed removal result.
func runAfterSuccessfulAttachmentRemoval(
	ctx context.Context,
	maintenance *attachmentMaintenance,
	remove func(context.Context) error,
	emitWarning func(string) error,
) error {
	if err := runWithAttachmentMutation(ctx, maintenance, remove); err != nil {
		return err
	}
	if maintenance != nil {
		_ = maintenance.runAutomaticRepack(ctx, emitWarning)
	}
	return nil
}

func runWithAttachmentMutation(
	ctx context.Context,
	maintenance *attachmentMaintenance,
	run func(context.Context) error,
) error {
	if maintenance == nil || maintenance.coordinator == nil {
		return run(ctx)
	}
	lease, err := maintenance.coordinator.AcquireMutation(ctx)
	if err != nil {
		return fmt.Errorf("acquire attachment mutation lease: %w", err)
	}
	return errors.Join(run(ctx), lease.Release())
}

// runScheduledSource distinguishes attachment-producing provider/SyncTech
// sources from calendar-only sources while preserving one shared wrapper.
// Packing never runs inline: a pack pass re-reads the whole pack catalog and
// excludes every ingest while it runs, so a scheduled sync only records that
// it wrote new loose blobs and the attachment-pack job packs them later.
func runScheduledSource(
	ctx context.Context,
	maintenance *attachmentMaintenance,
	attachmentProducing bool,
	run func(context.Context) error,
) error {
	if !attachmentProducing {
		return run(ctx)
	}
	before := maintenance.looseBlobWrites()
	err := runWithAttachmentMutation(ctx, maintenance, run)
	if maintenance != nil && maintenance.looseBlobWrites() != before {
		maintenance.markPackPending()
	}
	return err
}

func registerAttachmentMaintenanceJob(sched *scheduler.Scheduler, maintenance *attachmentMaintenance) error {
	return sched.AddJob(scheduler.Job{
		Name:       attachmentMaintenanceJob,
		MaxRuntime: automaticAttachmentMaxRuntime,
		Schedule:   attachmentMaintenanceCron,
		Run: func(ctx context.Context) error {
			return maintenance.daily(ctx)
		},
	})
}

func registerAttachmentPackJob(sched *scheduler.Scheduler, maintenance *attachmentMaintenance) error {
	// Recheck the catalog on the first pass after startup. In-memory write
	// counts cannot tell us what a previous daemon left loose.
	maintenance.markPackPending()
	if err := sched.AddJob(scheduler.Job{
		Name:       attachmentPackJob,
		MaxRuntime: automaticAttachmentMaxRuntime,
		Schedule:   attachmentPackCron,
		Run:        maintenance.runPendingPack,
	}); err != nil {
		return err
	}
	// A daemon started on demand may idle out long before the next cron
	// tick, so ingests start the job themselves.
	maintenance.requestPackJob = func() error { return sched.RequestJob(attachmentPackJob) }
	return nil
}

func registerScheduledBeeperJob(
	sched *scheduler.Scheduler,
	schedule string,
	maintenance *attachmentMaintenance,
	run func(context.Context) error,
) error {
	// Every beeper store source (one per beeper AccountID) maps to this
	// singleton job name via api.SchedulerJobNameForSource.
	return sched.AddJob(scheduler.Job{
		Name:        api.BeeperJobName,
		Schedule:    schedule,
		Preemptible: true,
		Run: func(ctx context.Context) error {
			return runScheduledSource(ctx, maintenance, true, run)
		},
	})
}

// attachmentProducingCommand reports whether the first command word names a
// generic daemon CLI operation that can create loose attachments.
func attachmentProducingCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "archive-remote-images",
		"backfill-beeper-media",
		"backfill-discord-media",
		"backfill-slack-media",
		"backfill-teams-media",
		"import",
		"import-maildir",
		"import-eml",
		"import-emlx",
		"import-gvoice",
		"import-imazing-csv",
		"import-imessage",
		importMboxCommand,
		"import-messenger",
		"import-pst",
		"import-slackdump",
		"import-synctech-sms",
		"import-whatsapp",
		"sync-beeper",
		"sync-discord",
		"sync-slack",
		"sync-synctech-sms",
		"sync-teams":
		return true
	default:
		return false
	}
}
