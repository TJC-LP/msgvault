package olm

import (
	"bytes"
	"fmt"
	"mime"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/pst"
)

// Attachment pairs an attachment reference with its resolved bytes.
type Attachment struct {
	Ref     AttachmentRef
	Content []byte
}

// BuildRFC5322 synthesizes RFC 5322/MIME bytes for an OLM message.
//
// OLM does not preserve the original transport headers, so From, To, Cc,
// Bcc, Date, Subject, Message-ID, In-Reply-To, References, Thread-Topic and
// Thread-Index are rebuilt from the exported fields. The body and attachment
// structure is delegated to the PST builder, which already handles
// multipart/alternative and multipart/mixed layout, quoted-printable and
// base64 encoding, and filename sanitization.
//
// book may be nil. When provided, name-only recipients from
// OPFMessageCopyDisplayTo are resolved to addresses learned from the archive.
func BuildRFC5322(msg *Message, attachments []Attachment, book *AddressBook) ([]byte, error) {
	var hdr bytes.Buffer

	from := msg.From
	if len(from) == 0 {
		from = msg.Sender
	}
	if v := formatAddressList(from); v != "" {
		writeHeader(&hdr, "From", v)
	}
	if v := formatAddressList(msg.To); v != "" {
		writeHeader(&hdr, "To", v)
	} else if v := formatDisplayList(msg.DisplayTo, book); v != "" {
		// Most OLM records carry recipients only as a display string of
		// names without addresses. Resolve what the archive can vouch for
		// and emit the rest as bare display names.
		writeHeader(&hdr, "To", v)
	}
	if v := formatAddressList(msg.CC); v != "" {
		writeHeader(&hdr, "Cc", v)
	}
	if v := formatAddressList(msg.BCC); v != "" {
		writeHeader(&hdr, "Bcc", v)
	}
	if v := formatAddressList(msg.ReplyTo); v != "" {
		writeHeader(&hdr, "Reply-To", v)
	}

	t := msg.SentAt
	if t.IsZero() {
		t = msg.ReceivedAt
	}
	if !t.IsZero() {
		writeHeader(&hdr, "Date", t.UTC().Format(time.RFC1123Z))
	}

	if msg.Subject != "" {
		writeHeader(&hdr, "Subject", mime.QEncoding.Encode("utf-8", sanitizeHeaderValue(msg.Subject)))
	}
	if mid := angleWrap(msg.MessageID); mid != "" {
		writeHeader(&hdr, "Message-Id", mid)
	}
	if irt := angleWrap(msg.InReplyTo); irt != "" {
		writeHeader(&hdr, "In-Reply-To", irt)
	}
	if refs := strings.TrimSpace(sanitizeHeaderValue(msg.References)); refs != "" {
		writeHeader(&hdr, "References", refs)
	}
	if v := strings.TrimSpace(sanitizeHeaderValue(msg.ThreadTopic)); v != "" {
		writeHeader(&hdr, "Thread-Topic", mime.QEncoding.Encode("utf-8", v))
	}
	if v := strings.TrimSpace(sanitizeHeaderValue(msg.ThreadIndex)); v != "" {
		writeHeader(&hdr, "Thread-Index", v)
	}
	writeHeader(&hdr, "X-Msgvault-Source", "olm")
	writeHeader(&hdr, "X-Msgvault-Synthesized", "true")
	if missing := missingAttachmentNames(msg); len(missing) > 0 {
		// Outlook listed these attachments but did not export their bytes.
		// Record the names so the gap is visible on the archived message.
		writeHeader(&hdr, "X-Msgvault-Olm-Attachments-Missing",
			mime.QEncoding.Encode("utf-8", strings.Join(missing, "; ")))
	}

	pstAtts := make([]pst.AttachmentEntry, 0, len(attachments))
	for _, a := range attachments {
		pstAtts = append(pstAtts, pst.AttachmentEntry{
			Filename:  a.Ref.Name,
			MIMEType:  a.Ref.ContentType,
			ContentID: a.Ref.ContentID,
			Size:      int32(min(len(a.Content), 1<<31-1)),
			Content:   a.Content,
		})
	}

	entry := &pst.MessageEntry{
		// The PST builder copies transport headers verbatim (minus MIME
		// content headers it rebuilds), which is exactly what we want for
		// the synthesized header block above.
		TransportHeaders: hdr.String(),
		BodyText:         msg.BodyText,
		BodyHTML:         msg.BodyHTML,
	}
	return pst.BuildRFC5322(entry, pstAtts)
}

func writeHeader(buf *bytes.Buffer, name, value string) {
	buf.WriteString(name)
	buf.WriteString(": ")
	buf.WriteString(value)
	buf.WriteString("\r\n")
}

// sanitizeHeaderValue strips CR and LF to prevent header injection from
// exported field values.
func sanitizeHeaderValue(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, s)
}

// angleWrap normalizes a message identifier to "<id>" form, or returns ""
// when the value is empty.
func angleWrap(id string) string {
	id = strings.TrimSpace(sanitizeHeaderValue(id))
	if id == "" {
		return ""
	}
	if !strings.HasPrefix(id, "<") {
		id = "<" + id
	}
	if !strings.HasSuffix(id, ">") {
		id += ">"
	}
	return id
}

// missingAttachmentNames lists attachments whose payload is absent from the
// archive, in export order.
func missingAttachmentNames(msg *Message) []string {
	var out []string
	for _, a := range msg.Attachments {
		if a.HasPayload() {
			continue
		}
		name := strings.TrimSpace(sanitizeHeaderValue(a.Name))
		if name == "" {
			name = "(unnamed)"
		}
		out = append(out, name)
	}
	return out
}

// formatDisplayList converts Outlook's semicolon-separated display string
// into a comma-separated address header. Names may themselves contain
// commas ("Last, First"), which is why the split is on semicolons only. A
// token that is itself an address is emitted as one; a name the address
// book resolves becomes "Name <email>"; anything else stays a bare
// Q-encoded display name.
func formatDisplayList(display string, book *AddressBook) string {
	parts := strings.Split(display, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(sanitizeHeaderValue(p))
		if p == "" {
			continue
		}
		if strings.Contains(p, "@") && !strings.ContainsAny(p, " <>\"") {
			out = append(out, "<"+p+">")
			continue
		}
		if email, ok := book.Resolve(p); ok {
			out = append(out, fmt.Sprintf("%s <%s>", mime.QEncoding.Encode("utf-8", p), email))
			continue
		}
		out = append(out, mime.QEncoding.Encode("utf-8", p))
	}
	return strings.Join(out, ", ")
}

// formatAddressList renders addresses as a comma-separated header value,
// Q-encoding display names and dropping entries with neither name nor email.
func formatAddressList(addrs []Address) string {
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		name := strings.TrimSpace(sanitizeHeaderValue(a.Name))
		email := strings.TrimSpace(sanitizeHeaderValue(a.Email))
		switch {
		case name == "" && email == "":
			continue
		case email == "":
			parts = append(parts, mime.QEncoding.Encode("utf-8", name))
		case name == "" || strings.EqualFold(name, email):
			parts = append(parts, "<"+email+">")
		default:
			parts = append(parts, fmt.Sprintf("%s <%s>", mime.QEncoding.Encode("utf-8", name), email))
		}
	}
	return strings.Join(parts, ", ")
}
