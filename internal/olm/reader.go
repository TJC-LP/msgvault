// Package olm reads Outlook for Mac archive files (.olm).
//
// An OLM archive is a zip container. Each mail message is a small XML
// document stored under
//
//	Accounts/<account>/com.microsoft.__Messages/<Folder>[/<Subfolder>...]/message_NNNNN.xml
//
// so the folder hierarchy is expressed by the entry path. Attachments are
// separate zip entries referenced from the message XML by OPFAttachmentURL.
// Other item types (calendar, contacts, notes, tasks) live under sibling
// com.microsoft.__* directories and are ignored.
package olm

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"sort"
	"strings"
)

// messagesSegment is the path segment that marks the mail-item subtree.
const messagesSegment = "com.microsoft.__Messages"

// ErrNoMessages is returned by Open when the archive contains no mail items.
var ErrNoMessages = errors.New("olm archive contains no mail messages")

// Archive is an open OLM file.
type Archive struct {
	zr      *zip.ReadCloser
	path    string
	folders []Folder
	entries map[string]*zip.File

	// Fingerprint is a short stable identifier derived from the archive's
	// central directory (message entry names, sizes, CRCs) and file size.
	// It changes whenever Outlook re-exports different content and is used
	// to namespace source_message_id and to validate resume checkpoints.
	Fingerprint string
}

// Folder is one mail folder in the archive.
type Folder struct {
	// Path is the slash-separated folder path relative to the account's
	// message root, e.g. "Inbox/Projects".
	Path string
	// Name is the last path element.
	Name string
	// Messages lists the message entries in this folder, sorted by entry name.
	Messages []MessageRef
}

// MessageRef identifies one message XML entry inside the archive.
type MessageRef struct {
	// EntryPath is the full zip entry name; it is unique within the archive
	// and stable across re-exports that keep the same numbering.
	EntryPath string
	// Size is the uncompressed size of the XML document.
	Size int64
	file *zip.File
}

// Open opens an OLM archive and indexes its mail folders.
func Open(olmPath string) (*Archive, error) {
	st, err := os.Stat(olmPath)
	if err != nil {
		return nil, fmt.Errorf("stat olm: %w", err)
	}
	zr, err := zip.OpenReader(olmPath)
	if err != nil {
		return nil, fmt.Errorf("open olm (expected a zip container): %w", err)
	}

	a := &Archive{zr: zr, path: olmPath, entries: make(map[string]*zip.File, len(zr.File))}

	type fpEntry struct {
		Name string
		Size uint64
		CRC  uint32
	}
	var fp []fpEntry
	byFolder := make(map[string]*Folder)

	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		clean, ok := cleanEntryName(zf.Name)
		if !ok {
			// Entry names with traversal or absolute paths never come from
			// Outlook; ignore rather than fail the whole archive.
			continue
		}
		a.entries[clean] = zf

		folderPath, isMessage := messageFolder(clean)
		if !isMessage {
			continue
		}
		fp = append(fp, fpEntry{Name: clean, Size: zf.UncompressedSize64, CRC: zf.CRC32})

		f, exists := byFolder[folderPath]
		if !exists {
			f = &Folder{Path: folderPath, Name: path.Base(folderPath)}
			byFolder[folderPath] = f
		}
		f.Messages = append(f.Messages, MessageRef{
			EntryPath: clean,
			Size:      entrySize(zf),
			file:      zf,
		})
	}

	if len(fp) == 0 {
		_ = zr.Close()
		return nil, ErrNoMessages
	}

	sort.Slice(fp, func(i, j int) bool { return fp[i].Name < fp[j].Name })
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "olm:%x\n", st.Size())
	for _, e := range fp {
		_, _ = fmt.Fprintf(h, "%s\x00%x\x00%x\n", e.Name, e.Size, e.CRC)
	}
	a.Fingerprint = "o" + hex.EncodeToString(h.Sum(nil))[:12]

	a.folders = make([]Folder, 0, len(byFolder))
	for _, f := range byFolder {
		sort.Slice(f.Messages, func(i, j int) bool { return f.Messages[i].EntryPath < f.Messages[j].EntryPath })
		a.folders = append(a.folders, *f)
	}
	sort.Slice(a.folders, func(i, j int) bool { return a.folders[i].Path < a.folders[j].Path })

	return a, nil
}

// Close releases the underlying zip reader.
func (a *Archive) Close() error {
	if err := a.zr.Close(); err != nil {
		return fmt.Errorf("close olm: %w", err)
	}
	return nil
}

// Path returns the archive file path.
func (a *Archive) Path() string { return a.path }

// Folders returns the mail folders in deterministic (path-sorted) order.
func (a *Archive) Folders() []Folder { return a.folders }

// OpenMessage returns a reader for one message XML document.
func (a *Archive) OpenMessage(ref MessageRef) (io.ReadCloser, error) {
	if ref.file == nil {
		zf, ok := a.entries[ref.EntryPath]
		if !ok {
			return nil, fmt.Errorf("message entry %q not found", ref.EntryPath)
		}
		ref.file = zf
	}
	rc, err := ref.file.Open()
	if err != nil {
		return nil, fmt.Errorf("open message %q: %w", ref.EntryPath, err)
	}
	return rc, nil
}

// ReadEntry reads a zip entry (typically an attachment referenced by
// OPFAttachmentURL) into memory, refusing entries larger than maxBytes.
// The URL is resolved relative to the archive root; a leading slash and
// Windows separators are tolerated.
func (a *Archive) ReadEntry(name string, maxBytes int64) ([]byte, error) {
	clean, ok := cleanEntryName(name)
	if !ok {
		return nil, fmt.Errorf("invalid entry name %q", name)
	}
	zf, ok := a.entries[clean]
	if !ok {
		return nil, fmt.Errorf("entry %q not found", clean)
	}
	size := entrySize(zf)
	if maxBytes > 0 && size > maxBytes {
		return nil, fmt.Errorf("entry %q is %d bytes, exceeds limit %d", clean, size, maxBytes)
	}
	rc, err := zf.Open()
	if err != nil {
		return nil, fmt.Errorf("open entry %q: %w", clean, err)
	}
	defer func() { _ = rc.Close() }()

	limit := maxBytes
	if limit <= 0 {
		limit = size
	}
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read entry %q: %w", clean, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("entry %q exceeds limit %d while reading", clean, limit)
	}
	return data, nil
}

// cleanEntryName normalizes a zip entry name and rejects names that escape
// the archive root.
func cleanEntryName(name string) (string, bool) {
	name = strings.ReplaceAll(name, `\`, "/")
	name = strings.TrimPrefix(name, "/")
	clean := path.Clean(name)
	if clean == "." || clean == "" || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", false
	}
	return clean, true
}

// messageFolder reports whether a cleaned entry is a mail message XML
// document and, if so, returns its folder path relative to the account's
// message root. Messages directly under the message root get folder path
// equal to the messages segment's parent account name so they still receive
// a label.
func messageFolder(clean string) (string, bool) {
	if !strings.EqualFold(path.Ext(clean), ".xml") {
		return "", false
	}
	segs := strings.Split(clean, "/")
	idx := -1
	for i, s := range segs {
		if s == messagesSegment {
			idx = i
			break
		}
	}
	if idx < 0 || idx == len(segs)-1 {
		return "", false
	}
	folderSegs := segs[idx+1 : len(segs)-1]
	if len(folderSegs) == 0 {
		// A message with no folder: label it with the account name when
		// present, otherwise a fixed root name.
		if idx > 0 {
			return segs[idx-1], true
		}
		return "Messages", true
	}
	return strings.Join(folderSegs, "/"), true
}

// entrySize returns an entry's uncompressed size as int64, saturating on
// the (never seen in practice) values that do not fit.
func entrySize(zf *zip.File) int64 {
	if zf.UncompressedSize64 > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(zf.UncompressedSize64)
}
