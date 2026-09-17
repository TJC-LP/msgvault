package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/olm"
	"go.kenn.io/msgvault/internal/remoteimage"
	"go.kenn.io/msgvault/internal/store"
)

// OlmImportOptions configures an Outlook for Mac (.olm) import.
type OlmImportOptions struct {
	// SourceType is the sources.source_type value. Defaults to "olm".
	SourceType string

	// Identifier is the email address for this source (required).
	Identifier string

	// SkipFolders lists folder names to skip (case-insensitive), matched
	// against the last path element, e.g. "Deleted Items" or "Junk Email".
	SkipFolders []string

	// NoResume forces a fresh import even if a checkpointed run exists.
	NoResume bool

	// CheckpointInterval controls how often (in messages) progress is saved.
	// Defaults to 200.
	CheckpointInterval int

	// AttachmentsDir controls where attachment files are written. Empty
	// disables disk storage (messages are still imported).
	AttachmentsDir string
	// RemoteImages is nil unless remote image archiving was explicitly enabled.
	RemoteImages *remoteimage.Fetcher

	// MaxMessageBytes limits the synthesized message size (body plus
	// attachments). Defaults to 128 MiB. Attachments larger than the limit
	// are dropped with a warning; the message itself is still imported.
	MaxMessageBytes int64

	// IngestFunc allows tests to override message ingestion.
	IngestFunc func(
		ctx context.Context, st *store.Store,
		sourceID int64, identifier, attachmentsDir string,
		labelIDs []int64, sourceMsgID, rawHash string,
		raw []byte, fallbackDate time.Time,
		log *slog.Logger,
	) error

	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// OlmImportSummary reports the results of an OLM import.
type OlmImportSummary struct {
	SourceID   int64
	WasResumed bool
	Duration   time.Duration

	FoldersTotal    int
	FoldersImported int

	MessagesProcessed int64
	MessagesAdded     int64
	MessagesUpdated   int64
	MessagesSkipped   int64
	Errors            int64
	HardErrors        bool
}

// olmCheckpoint tracks resume state. ArchiveID is the central-directory
// fingerprint from olm.Archive and is the authoritative identity check on
// resume: Outlook re-exports to the same path routinely, and a replaced file
// must restart from the beginning rather than resume at a stale offset.
type olmCheckpoint struct {
	File        string `json:"file"`
	ArchiveID   string `json:"archive_id,omitempty"`
	FolderIndex int    `json:"folder_index"`
	FolderPath  string `json:"folder_path"`
	MsgIndex    int64  `json:"msg_index"`
}

const (
	olmSyncType                     = "import-olm"
	defaultMaxOlmMessageBytes int64 = 128 << 20 // 128 MiB
	olmSourceMessageIDPrefix        = "olm"
)

// ImportOlm imports all mail messages from an Outlook for Mac .olm archive.
//
// Folder structure is preserved as labels. Calendar, contact, note, and task
// items are not present in the mail subtree and are never read. The import
// is resumable: rerunning with the same arguments continues from the last
// checkpoint, and re-importing a fresh export of the same mailbox skips
// messages already archived from an identical archive.
func ImportOlm(
	ctx context.Context, st *store.Store, olmPath string, opts OlmImportOptions,
) (retSummary *OlmImportSummary, retErr error) {
	if opts.SourceType == "" {
		opts.SourceType = "olm"
	}
	if opts.Identifier == "" {
		return nil, errors.New("identifier is required")
	}
	if opts.CheckpointInterval <= 0 {
		opts.CheckpointInterval = 200
	}
	if opts.MaxMessageBytes <= 0 {
		opts.MaxMessageBytes = defaultMaxOlmMessageBytes
	}

	ingestFn := opts.IngestFunc
	if ingestFn == nil {
		ingestFn = rawMessageIngester(opts.RemoteImages)
	}

	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	start := time.Now()
	summary := &OlmImportSummary{}

	absPath, err := filepath.Abs(olmPath)
	if err != nil {
		return nil, fmt.Errorf("abs path: %w", err)
	}
	cpFile := absPath
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		cpFile = resolved
	}

	// Open the archive first: the fingerprint is needed before any sync
	// bookkeeping so a corrupt or empty file never leaves a sync run behind.
	archive, err := olm.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("open olm: %w", err)
	}
	defer func() { _ = archive.Close() }()
	archiveID := archive.Fingerprint

	skipFolders := make(map[string]bool, len(opts.SkipFolders))
	for _, f := range opts.SkipFolders {
		skipFolders[strings.ToLower(f)] = true
	}

	src, err := st.GetOrCreateSource(opts.SourceType, opts.Identifier)
	if err != nil {
		return nil, fmt.Errorf("get/create source: %w", err)
	}
	summary.SourceID = src.ID
	ownershipCtx := context.WithoutCancel(ctx)
	execution, err := st.AcquireSyncExecutionContext(ownershipCtx, src.ID)
	if err != nil {
		return nil, fmt.Errorf("acquire sync execution: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, execution.Release())
	}()

	olmBase := filepath.Base(absPath)
	if !src.DisplayName.Valid || src.DisplayName.String == "" {
		if err := st.UpdateSourceDisplayName(src.ID, olmBase); err != nil {
			log.Warn("failed to set source display name", "error", err)
		}
	}

	var (
		cp     store.Checkpoint
		resume olmCheckpoint
	)

	if !opts.NoResume {
		active, err := st.GetLatestCheckpointedSyncByType(src.ID, olmSyncType)
		if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
			return nil, fmt.Errorf("check resumable sync: %w", err)
		}
		if active != nil {
			cp.MessagesProcessed = active.MessagesProcessed
			cp.MessagesAdded = active.MessagesAdded
			cp.MessagesUpdated = active.MessagesUpdated
			cp.ErrorsCount = active.ErrorsCount
			if active.CursorBefore.Valid && active.CursorBefore.String != "" {
				var saved olmCheckpoint
				if err := json.Unmarshal([]byte(active.CursorBefore.String), &saved); err == nil {
					sameFile := saved.File == absPath || saved.File == cpFile
					if !sameFile && saved.File != "" {
						if curInfo, err := os.Stat(absPath); err == nil {
							if cpInfo, err := os.Stat(saved.File); err == nil && os.SameFile(curInfo, cpInfo) {
								sameFile = true
							}
						}
					}
					archiveChanged := saved.ArchiveID != "" && saved.ArchiveID != archiveID
					switch {
					case sameFile && archiveChanged:
						log.Warn("olm archive fingerprint changed since last checkpoint; restarting from the beginning",
							"file", absPath,
							"saved_archive_id", saved.ArchiveID,
							"current_archive_id", archiveID,
						)
					case sameFile:
						resume = saved
						summary.WasResumed = true
						log.Info("resuming olm import",
							"file", absPath,
							"folder_index", resume.FolderIndex,
							"msg_index", resume.MsgIndex,
						)
					case saved.File != "":
						return nil, fmt.Errorf("active olm import is for %q, not %q; rerun with --no-resume", saved.File, absPath)
					}
				}
			}
		}
	}

	syncID, err := execution.StartSyncContext(ownershipCtx, olmSyncType, "")
	if err != nil {
		return nil, fmt.Errorf("start sync: %w", err)
	}
	st = st.ScopedToSync(src.ID, syncID)

	if !summary.WasResumed {
		if err := saveOlmCheckpoint(st, syncID, cpFile, archiveID, 0, "", 0, &cp); err != nil {
			log.Warn("failed to save initial checkpoint", "error", err)
		}
	}

	// Collect folders, honoring skips. Folders are already path-sorted.
	var folders []olm.Folder
	for _, f := range archive.Folders() {
		if skipFolders[strings.ToLower(f.Name)] {
			log.Debug("skipping folder", "path", f.Path)
			continue
		}
		if len(f.Messages) > 0 {
			folders = append(folders, f)
		}
	}
	summary.FoldersTotal = len(folders)

	if summary.WasResumed && resume.FolderPath != "" {
		if resume.FolderIndex >= len(folders) {
			log.Warn("resume folder index out of range; restarting from beginning",
				"saved_index", resume.FolderIndex,
				"folder_count", len(folders),
			)
			resume.FolderIndex = 0
			resume.MsgIndex = 0
		} else if folders[resume.FolderIndex].Path != resume.FolderPath {
			log.Warn("resume folder path mismatch; restarting from beginning",
				"saved_path", resume.FolderPath,
				"actual_path", folders[resume.FolderIndex].Path,
			)
			resume.FolderIndex = 0
			resume.MsgIndex = 0
		}
	}

	const (
		batchSize  = 200
		batchBytes = 32 << 20 // 32 MiB
	)

	type pendingOlmMessage struct {
		Raw          []byte
		RawHash      string
		SourceMsgID  string
		FallbackDate time.Time
		LabelID      int64
		FolderIndex  int
		FolderPath   string
		MsgIndex     int64
	}

	var (
		pending           []pendingOlmMessage
		pendingBytes      int64
		checkpointBlocked bool
		hardErrors        bool
		// lastDone is the position of the most recently processed message.
		// A cancellation checkpoint must point here, not at the message
		// about to be processed, or resume would skip that message.
		lastDone    *pendingOlmMessage
		haveResumed = summary.WasResumed
	)

	saveCp := func(fi int, fp string, mi int64) {
		if err := saveOlmCheckpoint(st, syncID, cpFile, archiveID, fi, fp, mi, &cp); err != nil {
			cp.ErrorsCount++
			summary.Errors++
			log.Warn("failed to save checkpoint", "error", err)
		}
	}

	flushPending := func() (stop bool) {
		if len(pending) == 0 {
			return false
		}

		ids := make([]string, len(pending))
		for i, p := range pending {
			ids[i] = p.SourceMsgID
		}

		existingWithRaw, errWithRaw := st.MessageExistsWithRawBatch(src.ID, ids)
		if errWithRaw != nil {
			cp.ErrorsCount++
			summary.Errors++
			log.Warn("existence check failed", "error", errWithRaw)
		}
		existingAny, errAny := st.MessageExistsBatch(src.ID, ids)
		if errAny != nil {
			cp.ErrorsCount++
			summary.Errors++
			log.Warn("existence check (any) failed", "error", errAny)
		}

		for i := range pending {
			p := pending[i]
			if ctx.Err() != nil {
				switch {
				case lastDone != nil:
					saveCp(lastDone.FolderIndex, lastDone.FolderPath, lastDone.MsgIndex)
				case haveResumed:
					saveCp(resume.FolderIndex, resume.FolderPath, resume.MsgIndex)
				default:
					saveCp(0, "", 0)
				}
				summary.Duration = time.Since(start)
				return true
			}

			cp.MessagesProcessed++
			summary.MessagesProcessed++
			lastDone = &pending[i]

			if errWithRaw == nil {
				if msgID, exists := existingWithRaw[p.SourceMsgID]; exists {
					summary.MessagesSkipped++
					if p.LabelID != 0 {
						if err := st.AddMessageLabels(msgID, []int64{p.LabelID}); err != nil {
							log.Warn("add labels to existing message", "error", err)
						}
					}
					if !checkpointBlocked && cp.MessagesProcessed%int64(opts.CheckpointInterval) == 0 {
						saveCp(p.FolderIndex, p.FolderPath, p.MsgIndex)
					}
					continue
				}
			} else {
				one, err := st.MessageExistsWithRawBatch(src.ID, []string{p.SourceMsgID})
				if err == nil {
					if msgID, exists := one[p.SourceMsgID]; exists {
						summary.MessagesSkipped++
						if p.LabelID != 0 {
							_ = st.AddMessageLabels(msgID, []int64{p.LabelID})
						}
						continue
					}
				}
			}

			alreadyExists := false
			if errAny == nil {
				_, alreadyExists = existingAny[p.SourceMsgID]
			}

			lblIDs := []int64{}
			if p.LabelID != 0 {
				lblIDs = []int64{p.LabelID}
			}

			if err := ingestFn(ctx, st, src.ID, opts.Identifier, opts.AttachmentsDir,
				lblIDs, p.SourceMsgID, p.RawHash, p.Raw, p.FallbackDate, log,
			); err != nil {
				cp.ErrorsCount++
				summary.Errors++
				log.Warn("failed to ingest message", "source_msg_id", p.SourceMsgID, "error", err)
				checkpointBlocked = true
				hardErrors = true
				continue
			}

			if alreadyExists {
				cp.MessagesUpdated++
				summary.MessagesUpdated++
			} else {
				cp.MessagesAdded++
				summary.MessagesAdded++
			}

			if !checkpointBlocked && cp.MessagesProcessed%int64(opts.CheckpointInterval) == 0 {
				saveCp(p.FolderIndex, p.FolderPath, p.MsgIndex)
			}
		}

		clear(pending)
		pending = pending[:0]
		pendingBytes = 0
		checkpointBlocked = false
		return false
	}

	labelCache := make(map[string]int64)

	for fi, folder := range folders {
		if ctx.Err() != nil {
			break
		}
		if summary.WasResumed && fi < resume.FolderIndex {
			continue
		}

		log.Debug("processing folder", "path", folder.Path, "count", len(folder.Messages))

		labelID, ok := labelCache[folder.Path]
		if !ok {
			lid, err := st.EnsureLabel(src.ID, folder.Path, folder.Name, "user")
			if err != nil {
				cp.ErrorsCount++
				summary.Errors++
				log.Warn("ensure label failed", "path", folder.Path, "error", err)
				lid = 0
			}
			labelCache[folder.Path] = lid
			labelID = lid
		}
		summary.FoldersImported++

		for mi, ref := range folder.Messages {
			if ctx.Err() != nil {
				break
			}
			msgIndex := int64(mi + 1)
			if summary.WasResumed && fi == resume.FolderIndex && msgIndex <= resume.MsgIndex {
				continue
			}

			raw, fallbackDate, err := buildOlmMessage(archive, ref, opts.MaxMessageBytes, log)
			if err != nil {
				cp.ErrorsCount++
				summary.Errors++
				log.Warn("failed to read olm message", "entry", ref.EntryPath, "error", err)
				continue
			}
			if int64(len(raw)) > opts.MaxMessageBytes {
				cp.ErrorsCount++
				summary.Errors++
				log.Warn("message exceeds size limit; skipping",
					"entry", ref.EntryPath, "size", len(raw), "limit", opts.MaxMessageBytes)
				continue
			}

			sum := sha256.Sum256(raw)
			rawHash := hex.EncodeToString(sum[:])
			// Entry paths are unique within one archive but restart at
			// message_00001 in every export, so namespace them by the
			// archive fingerprint. Re-importing the identical file is
			// idempotent; a fresh export of the same mailbox is a new
			// archive and relies on content-level dedup downstream.
			sourceMsgID := fmt.Sprintf("%s-%s-%s", olmSourceMessageIDPrefix, archiveID, ref.EntryPath)

			pending = append(pending, pendingOlmMessage{
				Raw:          raw,
				RawHash:      rawHash,
				SourceMsgID:  sourceMsgID,
				FallbackDate: fallbackDate,
				LabelID:      labelID,
				FolderIndex:  fi,
				FolderPath:   folder.Path,
				MsgIndex:     msgIndex,
			})
			pendingBytes += int64(len(raw))

			if len(pending) >= batchSize || pendingBytes >= batchBytes {
				if stop := flushPending(); stop {
					summary.HardErrors = hardErrors
					return summary, nil
				}
			}
		}
	}

	if stop := flushPending(); stop {
		summary.HardErrors = hardErrors
		return summary, nil
	}

	summary.Duration = time.Since(start)
	summary.HardErrors = hardErrors

	if hardErrors {
		if err := st.FailSync(syncID, fmt.Sprintf("completed with %d errors", cp.ErrorsCount)); err != nil {
			return summary, fmt.Errorf("fail sync: %w", err)
		}
		return summary, nil
	}

	finalMsg := fmt.Sprintf("folders:%d messages:%d", summary.FoldersImported, summary.MessagesProcessed)
	if err := st.CompleteSync(syncID, finalMsg); err != nil {
		return summary, fmt.Errorf("complete sync: %w", err)
	}
	return summary, nil
}

// buildOlmMessage parses one message entry, resolves its attachments within
// the byte budget, and synthesizes RFC 5322 bytes. Oversized or missing
// attachments are logged and dropped so the message itself still imports.
func buildOlmMessage(archive *olm.Archive, ref olm.MessageRef, maxBytes int64, log *slog.Logger) ([]byte, time.Time, error) {
	rc, err := archive.OpenMessage(ref)
	if err != nil {
		return nil, time.Time{}, err
	}
	msg, err := olm.ParseMessage(rc)
	_ = rc.Close()
	if err != nil {
		return nil, time.Time{}, err
	}

	var (
		attachments []olm.Attachment
		budget      = maxBytes
	)
	for _, a := range msg.Attachments {
		if a.URL == "" {
			continue
		}
		content, err := archive.ReadEntry(a.URL, budget)
		if err != nil {
			log.Warn("skipping attachment", "entry", ref.EntryPath, "attachment", a.Name, "error", err)
			continue
		}
		budget -= int64(len(content))
		attachments = append(attachments, olm.Attachment{Ref: a, Content: content})
	}

	raw, err := olm.BuildRFC5322(msg, attachments)
	if err != nil {
		return nil, time.Time{}, err
	}

	fallback := msg.SentAt
	if fallback.IsZero() {
		fallback = msg.ReceivedAt
	}
	return raw, fallback, nil
}

func saveOlmCheckpoint(st *store.Store, syncID int64, file, archiveID string, folderIndex int, folderPath string, msgIndex int64, cp *store.Checkpoint) error {
	b, err := json.Marshal(olmCheckpoint{
		File:        file,
		ArchiveID:   archiveID,
		FolderIndex: folderIndex,
		FolderPath:  folderPath,
		MsgIndex:    msgIndex,
	}, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("marshal checkpoint: %w", err)
	}
	cp.PageToken = string(b)
	return st.UpdateSyncCheckpoint(syncID, cp)
}
