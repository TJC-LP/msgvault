package olm

import (
	"bytes"
	"mime"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

const sampleMessageXML = `<?xml version="1.0" encoding="UTF-8"?>
<emails>
<email>
<OPFMessageCopySubject>Quarterly numbers — draft</OPFMessageCopySubject>
<OPFMessageCopyMessageID>&lt;abc123@example.com&gt;</OPFMessageCopyMessageID>
<OPFMessageCopyInReplyTo>parent456@example.com</OPFMessageCopyInReplyTo>
<OPFMessageCopyThreadTopic>Quarterly numbers</OPFMessageCopyThreadTopic>
<OPFMessageCopySentTime>2022-05-26T17:55:40</OPFMessageCopySentTime>
<OPFMessageCopyReceivedTime>2022-05-26T17:55:42</OPFMessageCopyReceivedTime>
<OPFMessageCopyFromAddresses>
<emailAddress OPFContactEmailAddressName="Alex Sender" OPFContactEmailAddressAddress="alex@example.com"/>
</OPFMessageCopyFromAddresses>
<OPFMessageCopySenderAddress>
<emailAddress OPFContactEmailAddressName="Alex Sender" OPFContactEmailAddressAddress="alex@example.com"/>
</OPFMessageCopySenderAddress>
<OPFMessageCopyToAddresses>
<emailAddress OPFContactEmailAddressName="Bea Reader" OPFContactEmailAddressAddress="bea@example.com"/>
<emailAddress OPFContactEmailAddressName="" OPFContactEmailAddressAddress="team@example.org"/>
</OPFMessageCopyToAddresses>
<OPFMessageCopyCCAddresses>
<emailAddress OPFContactEmailAddressName="Casey Copy" OPFContactEmailAddressAddress="casey@example.net"/>
</OPFMessageCopyCCAddresses>
<OPFMessageCopyBody>Hello,

Numbers attached. Line with = sign and café.
</OPFMessageCopyBody>
<OPFMessageCopyHTMLBody>&lt;html&gt;&lt;body&gt;&lt;p&gt;Numbers &lt;b&gt;attached&lt;/b&gt;.&lt;/p&gt;&lt;img src="cid:img1@example.com"&gt;&lt;/body&gt;&lt;/html&gt;</OPFMessageCopyHTMLBody>
<OPFMessageCopyAttachmentList>
<messageAttachment OPFAttachmentContentType="text/csv" OPFAttachmentName="numbers.csv" OPFAttachmentURL="Accounts/acct/com.microsoft.__Attachments/message_00001/numbers.csv"/>
<messageAttachment OPFAttachmentContentType="image/png" OPFAttachmentName="logo.png" OPFAttachmentContentID="img1@example.com" OPFAttachmentURL="Accounts/acct/com.microsoft.__Attachments/message_00001/logo.png"/>
</OPFMessageCopyAttachmentList>
</email>
</emails>
`

func TestParseMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	msg, err := ParseMessage(strings.NewReader(sampleMessageXML))
	require.NoError(err)

	assert.Equal("Quarterly numbers — draft", msg.Subject)
	assert.Equal("<abc123@example.com>", msg.MessageID)
	assert.Equal("parent456@example.com", msg.InReplyTo)
	assert.Equal("Quarterly numbers", msg.ThreadTopic)

	// Timestamps are UTC even though the XML carries no zone suffix.
	assert.Equal(time.Date(2022, 5, 26, 17, 55, 40, 0, time.UTC), msg.SentAt)
	assert.Equal(time.Date(2022, 5, 26, 17, 55, 42, 0, time.UTC), msg.ReceivedAt)

	require.Len(msg.From, 1)
	assert.Equal(Address{Name: "Alex Sender", Email: "alex@example.com"}, msg.From[0])
	require.Len(msg.To, 2)
	assert.Equal(Address{Name: "Bea Reader", Email: "bea@example.com"}, msg.To[0])
	assert.Equal(Address{Email: "team@example.org"}, msg.To[1])
	require.Len(msg.CC, 1)
	assert.Equal("casey@example.net", msg.CC[0].Email)
	assert.Empty(msg.BCC)

	assert.Contains(msg.BodyText, "Numbers attached.")
	assert.Equal(`<html><body><p>Numbers <b>attached</b>.</p><img src="cid:img1@example.com"></body></html>`, msg.BodyHTML)

	require.Len(msg.Attachments, 2)
	assert.Equal(AttachmentRef{
		Name:        "numbers.csv",
		ContentType: "text/csv",
		URL:         "Accounts/acct/com.microsoft.__Attachments/message_00001/numbers.csv",
	}, msg.Attachments[0])
	assert.Equal("img1@example.com", msg.Attachments[1].ContentID)
}

func TestParseMessage_RejectsNonEmailDocument(t *testing.T) {
	_, err := ParseMessage(strings.NewReader(`<?xml version="1.0"?><contacts><contact/></contacts>`))
	require.ErrorIs(t, err, ErrNotMessage)
}

func TestParseOLMTime(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want time.Time
	}{
		{"bare utc", "2022-05-26T17:55:40", time.Date(2022, 5, 26, 17, 55, 40, 0, time.UTC)},
		{"zoned", "2022-05-26T13:55:40-04:00", time.Date(2022, 5, 26, 17, 55, 40, 0, time.UTC)},
		{"space separated", "2022-05-26 17:55:40", time.Date(2022, 5, 26, 17, 55, 40, 0, time.UTC)},
		{"empty", "", time.Time{}},
		{"garbage", "yesterday", time.Time{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parseOLMTime(tc.in))
		})
	}
}

func TestMessageFolder(t *testing.T) {
	tests := []struct {
		entry      string
		wantFolder string
		wantOK     bool
	}{
		{"Accounts/acct/com.microsoft.__Messages/Inbox/message_00001.xml", "Inbox", true},
		{"Accounts/acct/com.microsoft.__Messages/Inbox/Projects/message_00002.xml", "Inbox/Projects", true},
		{"Accounts/acct/com.microsoft.__Messages/message_00003.xml", "acct", true},
		{"Accounts/acct/com.microsoft.__Contacts/contact_00001.xml", "", false},
		{"Accounts/acct/com.microsoft.__Messages/Inbox/attachment.pdf", "", false},
		{"Categories.xml", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.entry, func(t *testing.T) {
			folder, ok := messageFolder(tc.entry)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantFolder, folder)
		})
	}
}

// writeFixtureArchive builds a synthetic .olm with two folders, one
// non-mail item, and two attachments.
func writeFixtureArchive(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fixture.olm")
	testutil.CreateZip(t, p, []testutil.ArchiveEntry{
		{Name: "Accounts/acct/com.microsoft.__Messages/Inbox/message_00001.xml", Content: sampleMessageXML},
		{Name: "Accounts/acct/com.microsoft.__Messages/Inbox/Projects/message_00002.xml", Content: strings.ReplaceAll(sampleMessageXML, "abc123", "def456")},
		{Name: "Accounts/acct/com.microsoft.__Messages/Sent Items/message_00003.xml", Content: strings.ReplaceAll(sampleMessageXML, "abc123", "ghi789")},
		{Name: "Accounts/acct/com.microsoft.__Contacts/contact_00001.xml", Content: "<contacts><contact/></contacts>"},
		{Name: "Accounts/acct/com.microsoft.__Attachments/message_00001/numbers.csv", Content: "a,b\n1,2\n"},
		{Name: "Accounts/acct/com.microsoft.__Attachments/message_00001/logo.png", Content: "\x89PNG fake"},
		{Name: "Categories.xml", Content: "<categories/>"},
	})
	return p
}

func TestOpen_IndexesFoldersAndFingerprint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	p := writeFixtureArchive(t)
	a, err := Open(p)
	require.NoError(err)
	defer func() { _ = a.Close() }()

	folders := a.Folders()
	require.Len(folders, 3)
	assert.Equal("Inbox", folders[0].Path)
	assert.Equal("Inbox", folders[0].Name)
	assert.Equal("Inbox/Projects", folders[1].Path)
	assert.Equal("Projects", folders[1].Name)
	assert.Equal("Sent Items", folders[2].Path)
	require.Len(folders[0].Messages, 1)
	assert.Equal("Accounts/acct/com.microsoft.__Messages/Inbox/message_00001.xml", folders[0].Messages[0].EntryPath)

	assert.True(strings.HasPrefix(a.Fingerprint, "o"), "fingerprint %q", a.Fingerprint)
	assert.Len(a.Fingerprint, 13)

	// Same bytes at a different path yield the same fingerprint.
	b, err := Open(p)
	require.NoError(err)
	defer func() { _ = b.Close() }()
	assert.Equal(a.Fingerprint, b.Fingerprint)

	rc, err := a.OpenMessage(folders[0].Messages[0])
	require.NoError(err)
	msg, err := ParseMessage(rc)
	require.NoError(rc.Close())
	require.NoError(err)
	assert.Equal("<abc123@example.com>", msg.MessageID)
}

func TestOpen_NoMessages(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty.olm")
	testutil.CreateZip(t, p, []testutil.ArchiveEntry{
		{Name: "Accounts/acct/com.microsoft.__Contacts/contact_00001.xml", Content: "<contacts/>"},
	})
	_, err := Open(p)
	require.ErrorIs(t, err, ErrNoMessages)
}

func TestOpen_NotAZip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bogus.olm")
	require.NoError(t, os.WriteFile(p, []byte("this is not a zip"), 0o600))
	_, err := Open(p)
	require.Error(t, err)
}

func TestReadEntry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	p := writeFixtureArchive(t)
	a, err := Open(p)
	require.NoError(err)
	defer func() { _ = a.Close() }()

	data, err := a.ReadEntry("Accounts/acct/com.microsoft.__Attachments/message_00001/numbers.csv", 0)
	require.NoError(err)
	assert.Equal("a,b\n1,2\n", string(data))

	// Leading slash and backslashes are tolerated.
	data, err = a.ReadEntry(`/Accounts\acct\com.microsoft.__Attachments\message_00001\numbers.csv`, 0)
	require.NoError(err)
	assert.Equal("a,b\n1,2\n", string(data))

	_, err = a.ReadEntry("Accounts/acct/com.microsoft.__Attachments/message_00001/numbers.csv", 3)
	require.Error(err, "size limit enforced")

	_, err = a.ReadEntry("../etc/passwd", 0)
	require.Error(err, "traversal rejected")

	_, err = a.ReadEntry("Accounts/acct/missing.bin", 0)
	require.Error(err)
}

func TestBuildRFC5322(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	msg, err := ParseMessage(strings.NewReader(sampleMessageXML))
	require.NoError(err)

	raw, err := BuildRFC5322(msg, []Attachment{
		{Ref: msg.Attachments[0], Content: []byte("a,b\n1,2\n")},
		{Ref: msg.Attachments[1], Content: []byte("\x89PNG fake")},
	})
	require.NoError(err)

	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	require.NoError(err)
	h := parsed.Header

	assert.Equal("<abc123@example.com>", h.Get("Message-Id"))
	assert.Equal("<parent456@example.com>", h.Get("In-Reply-To"))
	assert.Equal("olm", h.Get("X-Msgvault-Source"))
	assert.Equal("true", h.Get("X-Msgvault-Synthesized"))
	assert.Equal("Thu, 26 May 2022 17:55:40 +0000", h.Get("Date"))

	from, err := h.AddressList("From")
	require.NoError(err)
	require.Len(from, 1)
	assert.Equal("Alex Sender", from[0].Name)
	assert.Equal("alex@example.com", from[0].Address)

	to, err := h.AddressList("To")
	require.NoError(err)
	require.Len(to, 2)
	assert.Equal("bea@example.com", to[0].Address)
	assert.Equal("team@example.org", to[1].Address)

	subj, err := new(mime.WordDecoder).DecodeHeader(h.Get("Subject"))
	require.NoError(err)
	assert.Equal("Quarterly numbers — draft", subj)

	ct := h.Get("Content-Type")
	assert.True(strings.HasPrefix(ct, "multipart/mixed"), "content-type %q", ct)
	body := string(raw)
	assert.Contains(body, "multipart/alternative")
	assert.Contains(body, `name="numbers.csv"`)
	assert.Contains(body, "Content-Id: <img1@example.com>")
	assert.Contains(body, "Content-Disposition: inline")
}

func TestBuildRFC5322_FallsBackToSenderAndReceivedTime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	msg := &Message{
		Subject:    "no from",
		Sender:     []Address{{Name: "Only Sender", Email: "sender@example.com"}},
		ReceivedAt: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC),
		BodyText:   "plain",
	}
	raw, err := BuildRFC5322(msg, nil)
	require.NoError(err)
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	require.NoError(err)
	assert.Equal("Only Sender <sender@example.com>", parsed.Header.Get("From"))
	assert.Equal("Tue, 02 Jan 2024 03:04:05 +0000", parsed.Header.Get("Date"))
	assert.Empty(parsed.Header.Get("Message-Id"))
	assert.True(strings.HasPrefix(parsed.Header.Get("Content-Type"), "text/plain"))
}

func TestFormatAddressList_StripsHeaderInjection(t *testing.T) {
	got := formatAddressList([]Address{{Name: "Evil\r\nBcc: x@example.com", Email: "e@example.com"}})
	assert.NotContains(t, got, "\n")
	assert.Contains(t, got, "<e@example.com>")
}
