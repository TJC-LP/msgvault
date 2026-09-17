package importer

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const olmTestMessage = `<?xml version="1.0" encoding="UTF-8"?>
<emails><email>
<OPFMessageCopySubject>Subject %s</OPFMessageCopySubject>
<OPFMessageCopyMessageID>&lt;%s@example.com&gt;</OPFMessageCopyMessageID>
<OPFMessageCopySentTime>2023-03-04T05:06:07</OPFMessageCopySentTime>
<OPFMessageCopyFromAddresses><emailAddress OPFContactEmailAddressName="Sender" OPFContactEmailAddressAddress="sender@example.com"/></OPFMessageCopyFromAddresses>
<OPFMessageCopyToAddresses><emailAddress OPFContactEmailAddressName="Me" OPFContactEmailAddressAddress="user@example.com"/></OPFMessageCopyToAddresses>
<OPFMessageCopyBody>Body %s</OPFMessageCopyBody>
<OPFMessageCopyHTMLBody>&lt;p&gt;Body %s&lt;/p&gt;</OPFMessageCopyHTMLBody>
%ATT%
</email></emails>`

const olmTestAttachment = `<OPFMessageCopyAttachmentList><messageAttachment OPFAttachmentContentType="text/plain" OPFAttachmentName="note.txt" OPFAttachmentURL="Accounts/acct/com.microsoft.__Attachments/1/note.txt"/></OPFMessageCopyAttachmentList>`

func olmTestXML(id, attachmentXML string) string {
	s := strings.ReplaceAll(olmTestMessage, "%s", id)
	return strings.ReplaceAll(s, "%ATT%", attachmentXML)
}

// writeTestOlm builds a synthetic archive with three mail folders, one
// attachment, and one non-mail item.
func writeTestOlm(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	testutil.CreateZip(t, p, []testutil.ArchiveEntry{
		{Name: "Accounts/acct/com.microsoft.__Messages/Inbox/message_00001.xml", Content: olmTestXML("m1", olmTestAttachment)},
		{Name: "Accounts/acct/com.microsoft.__Messages/Inbox/message_00002.xml", Content: olmTestXML("m2", "")},
		{Name: "Accounts/acct/com.microsoft.__Messages/Sent Items/message_00003.xml", Content: olmTestXML("m3", "")},
		{Name: "Accounts/acct/com.microsoft.__Messages/Junk Email/message_00004.xml", Content: olmTestXML("m4", "")},
		{Name: "Accounts/acct/com.microsoft.__Attachments/1/note.txt", Content: "hello"},
		{Name: "Accounts/acct/com.microsoft.__Calendar/event_00001.xml", Content: "<appointments/>"},
	})
	return p
}

func TestImportOlm_MissingFile(t *testing.T) {
	st := openTestStorePst(t)
	mock := &mockIngestFunc{}
	_, err := ImportOlm(context.Background(), st, filepath.Join(t.TempDir(), "missing.olm"), OlmImportOptions{
		Identifier: "user@example.com",
		IngestFunc: mock.fn,
	})
	require.Error(t, err)
	assert.Empty(t, mock.calls)
}

func TestImportOlm_RequiresIdentifier(t *testing.T) {
	st := openTestStorePst(t)
	_, err := ImportOlm(context.Background(), st, writeTestOlm(t, "a.olm"), OlmImportOptions{})
	require.ErrorContains(t, err, "identifier is required")
}

func TestImportOlm_ImportsFoldersAsLabelsAndSkipsFolders(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := openTestStorePst(t)
	mock := &mockIngestFunc{}
	p := writeTestOlm(t, "mail.olm")

	summary, err := ImportOlm(context.Background(), st, p, OlmImportOptions{
		Identifier:  "user@example.com",
		SkipFolders: []string{"junk email"},
		IngestFunc:  mock.fn,
	})
	require.NoError(err)

	assert.Equal(2, summary.FoldersTotal, "Junk Email skipped")
	assert.Equal(2, summary.FoldersImported)
	assert.Equal(int64(3), summary.MessagesProcessed)
	assert.Equal(int64(3), summary.MessagesAdded)
	assert.Equal(int64(0), summary.Errors)
	assert.False(summary.HardErrors)
	require.Len(mock.calls, 3)

	labels := map[int64]bool{}
	for _, c := range mock.calls {
		assert.Equal(summary.SourceID, c.SourceID)
		assert.Equal("user@example.com", c.Identifier)
		assert.True(strings.HasPrefix(c.SourceMsgID, "olm-o"), "source id %q", c.SourceMsgID)
		assert.Equal(time.Date(2023, 3, 4, 5, 6, 7, 0, time.UTC), c.FallbackDate)
		require.Len(c.LabelIDs, 1)
		labels[c.LabelIDs[0]] = true
		assert.Positive(c.RawLen)
	}
	assert.Len(labels, 2, "Inbox and Sent Items labels")

	src, err := st.GetSourceByID(summary.SourceID)
	require.NoError(err)
	assert.Equal("olm", src.SourceType)
	assert.Equal("mail.olm", src.DisplayName.String)
}

func TestImportOlm_RealIngestIsIdempotent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := openTestStorePst(t)
	p := writeTestOlm(t, "mail.olm")
	opts := OlmImportOptions{Identifier: "user@example.com", AttachmentsDir: t.TempDir()}

	first, err := ImportOlm(context.Background(), st, p, opts)
	require.NoError(err)
	assert.Equal(int64(4), first.MessagesAdded)
	assert.Equal(int64(0), first.Errors)
	assert.False(first.HardErrors)

	second, err := ImportOlm(context.Background(), st, p, opts)
	require.NoError(err)
	assert.Equal(int64(0), second.MessagesAdded)
	assert.Equal(int64(4), second.MessagesSkipped)

	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_id = ?`, first.SourceID).Scan(&count))
	assert.Equal(4, count)

	var withMID int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND rfc822_message_id = ?`, first.SourceID, "m1@example.com").Scan(&withMID))
	assert.Equal(1, withMID, "Message-ID from OLM preserved")

	var atts int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments a JOIN messages m ON m.id = a.message_id WHERE m.source_id = ?`, first.SourceID).Scan(&atts))
	assert.Equal(1, atts, "attachment referenced by OPFAttachmentURL imported")
}

func TestImportOlm_ResumesFromCheckpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := openTestStorePst(t)
	p := writeTestOlm(t, "mail.olm")

	// First run: cancel after the first successful ingest. Checkpoints are
	// saved every message, so the run stops with one message recorded.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstRun := &mockIngestFunc{}
	cancelAfterFirst := func(
		c context.Context, s *store.Store,
		sourceID int64, identifier, attachmentsDir string,
		labelIDs []int64, sourceMsgID, rawHash string,
		raw []byte, fallbackDate time.Time,
		log *slog.Logger,
	) error {
		err := firstRun.fn(c, s, sourceID, identifier, attachmentsDir, labelIDs, sourceMsgID, rawHash, raw, fallbackDate, log)
		cancel()
		return err
	}
	interrupted, err := ImportOlm(ctx, st, p, OlmImportOptions{
		Identifier:         "user@example.com",
		CheckpointInterval: 1,
		IngestFunc:         cancelAfterFirst,
	})
	require.NoError(err)
	assert.Equal(int64(1), interrupted.MessagesAdded)
	require.Len(firstRun.calls, 1)

	// Second run resumes and imports only the remaining three.
	secondRun := &mockIngestFunc{}
	resumed, err := ImportOlm(context.Background(), st, p, OlmImportOptions{
		Identifier: "user@example.com",
		IngestFunc: secondRun.fn,
	})
	require.NoError(err)
	assert.True(resumed.WasResumed)
	assert.Equal(int64(3), resumed.MessagesAdded)
	require.Len(secondRun.calls, 3)
	assert.NotEqual(firstRun.calls[0].SourceMsgID, secondRun.calls[0].SourceMsgID)
}

func TestImportOlm_ChangedArchiveRestartsInsteadOfResuming(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := openTestStorePst(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "mail.olm")
	testutil.CreateZip(t, p, []testutil.ArchiveEntry{
		{Name: "Accounts/acct/com.microsoft.__Messages/Inbox/message_00001.xml", Content: olmTestXML("m1", "")},
		{Name: "Accounts/acct/com.microsoft.__Messages/Inbox/message_00002.xml", Content: olmTestXML("m2", "")},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelAfterFirst := func(
		c context.Context, s *store.Store,
		sourceID int64, identifier, attachmentsDir string,
		labelIDs []int64, sourceMsgID, rawHash string,
		raw []byte, fallbackDate time.Time,
		log *slog.Logger,
	) error {
		cancel()
		return nil
	}
	_, err := ImportOlm(ctx, st, p, OlmImportOptions{
		Identifier: "user@example.com", CheckpointInterval: 1, IngestFunc: cancelAfterFirst,
	})
	require.NoError(err)

	// Re-export to the same path with different content.
	testutil.CreateZip(t, p, []testutil.ArchiveEntry{
		{Name: "Accounts/acct/com.microsoft.__Messages/Inbox/message_00001.xml", Content: olmTestXML("n1", "")},
		{Name: "Accounts/acct/com.microsoft.__Messages/Inbox/message_00002.xml", Content: olmTestXML("n2", "")},
		{Name: "Accounts/acct/com.microsoft.__Messages/Inbox/message_00003.xml", Content: olmTestXML("n3", "")},
	})

	mock := &mockIngestFunc{}
	summary, err := ImportOlm(context.Background(), st, p, OlmImportOptions{
		Identifier: "user@example.com", IngestFunc: mock.fn,
	})
	require.NoError(err)
	assert.False(summary.WasResumed, "fingerprint change forces a restart")
	assert.Len(mock.calls, 3)
}

func TestImportOlm_IngestErrorMarksHardErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := openTestStorePst(t)
	p := writeTestOlm(t, "mail.olm")
	mock := &mockIngestFunc{err: errors.New("ingest failed")}

	summary, err := ImportOlm(context.Background(), st, p, OlmImportOptions{
		Identifier: "user@example.com", IngestFunc: mock.fn,
	})
	require.NoError(err)
	assert.True(summary.HardErrors)
	assert.Equal(int64(4), summary.Errors)
	assert.Equal(int64(0), summary.MessagesAdded)
}

func TestImportOlm_AttachesMeetingInviteKeyedByMessageID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := openTestStorePst(t)
	p := filepath.Join(t.TempDir(), "invite.olm")
	testutil.CreateZip(t, p, []testutil.ArchiveEntry{
		{Name: "Accounts/acct/com.microsoft.__Messages/Sent Items/message_00001.xml", Content: olmTestXML("meet1", "")},
		{Name: "Accounts/acct/com.microsoft.__Messages/Sent Items/com.microsoft.__Attachments/meet1@example.com.ics", Content: "BEGIN:VCALENDAR\nEND:VCALENDAR\n"},
	})

	summary, err := ImportOlm(context.Background(), st, p, OlmImportOptions{
		Identifier: "user@example.com", AttachmentsDir: t.TempDir(),
	})
	require.NoError(err)
	assert.Equal(int64(1), summary.MessagesAdded)

	var name, ctype string
	require.NoError(st.DB().QueryRow(`SELECT a.filename, a.mime_type FROM attachments a JOIN messages m ON m.id = a.message_id WHERE m.source_id = ?`, summary.SourceID).Scan(&name, &ctype))
	assert.Equal("invite.ics", name)
	assert.Equal("text/calendar", ctype)
}

func TestImportOlm_ResolvesNameOnlyRecipients(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := openTestStorePst(t)
	// m1 carries a real To address for "Me"; m2 lists the same name only in
	// DisplayTo. After import both should have a "to" recipient row.
	withAddr := olmTestXML("m1", "")
	nameOnly := strings.Replace(olmTestXML("m2", ""),
		`<OPFMessageCopyToAddresses><emailAddress OPFContactEmailAddressName="Me" OPFContactEmailAddressAddress="user@example.com"/></OPFMessageCopyToAddresses>`,
		`<OPFMessageCopyDisplayTo>Me</OPFMessageCopyDisplayTo>`, 1)
	require.NotEqual(withAddr, nameOnly)
	p := filepath.Join(t.TempDir(), "names.olm")
	testutil.CreateZip(t, p, []testutil.ArchiveEntry{
		{Name: "Accounts/acct/com.microsoft.__Messages/Inbox/message_00001.xml", Content: withAddr},
		{Name: "Accounts/acct/com.microsoft.__Messages/Inbox/message_00002.xml", Content: nameOnly},
	})

	summary, err := ImportOlm(context.Background(), st, p, OlmImportOptions{Identifier: "user@example.com"})
	require.NoError(err)
	assert.Equal(int64(2), summary.MessagesAdded)
	assert.Equal(2, summary.RecipientNamesLearned, "Sender and Me")

	var toRows int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM message_recipients r JOIN messages m ON m.id = r.message_id WHERE m.source_id = ? AND r.recipient_type = 'to'`, summary.SourceID).Scan(&toRows))
	assert.Equal(2, toRows, "name-only recipient resolved to an address")

	// With resolution off, the name-only message has no addressable recipient.
	st2 := openTestStorePst(t)
	summary2, err := ImportOlm(context.Background(), st2, p, OlmImportOptions{Identifier: "user@example.com", NoResolveRecipients: true})
	require.NoError(err)
	assert.Equal(0, summary2.RecipientNamesLearned)
	require.NoError(st2.DB().QueryRow(`SELECT COUNT(*) FROM message_recipients r JOIN messages m ON m.id = r.message_id WHERE m.source_id = ? AND r.recipient_type = 'to'`, summary2.SourceID).Scan(&toRows))
	assert.Equal(1, toRows)
}
