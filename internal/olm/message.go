package olm

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Address is one sender or recipient from an OLM message.
type Address struct {
	Name  string
	Email string
}

// AttachmentRef describes an attachment listed in a message. Content is
// resolved separately through Archive.ReadEntry so callers can apply size
// limits and skip attachments entirely.
type AttachmentRef struct {
	Name        string
	ContentType string
	ContentID   string
	// URL is the zip entry name holding the attachment bytes. Empty for
	// attachments Outlook exported without a payload.
	URL string
}

// Message holds the fields parsed from one OLM message XML document.
type Message struct {
	Subject   string
	MessageID string
	InReplyTo string
	// References is the raw References header value when Outlook exported one.
	References string
	// ThreadTopic and ThreadIndex mirror the Exchange Thread-Topic and
	// Thread-Index headers.
	ThreadTopic string
	ThreadIndex string

	From []Address
	// Sender is OPFMessageCopySenderAddress, which can differ from From for
	// on-behalf-of mail. Used as a fallback when From is empty.
	Sender []Address
	To     []Address
	CC     []Address
	BCC    []Address

	BodyText string
	BodyHTML string

	// SentAt and ReceivedAt are UTC. OLM writes them without a zone suffix.
	SentAt     time.Time
	ReceivedAt time.Time

	Attachments []AttachmentRef
}

// ErrNotMessage is returned when the XML root is not an OLM <emails> document.
var ErrNotMessage = errors.New("not an olm email document")

// ParseMessage parses one message XML document.
//
// The parser is a streaming token walk rather than struct unmarshalling so
// unknown elements are ignored and a large HTML body is not decoded twice.
func ParseMessage(r io.Reader) (*Message, error) {
	dec := xml.NewDecoder(r)
	dec.Strict = false
	dec.CharsetReader = charsetReader

	msg := &Message{}
	var (
		sawRoot   bool
		sawEmail  bool
		addrList  *[]Address // current address container, if any
		textField *string    // current scalar element being collected
		text      strings.Builder
		timeField *time.Time
	)

	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("parse olm message: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			name := t.Name.Local
			if !sawRoot {
				sawRoot = true
				if name != "emails" && name != "email" {
					return nil, ErrNotMessage
				}
				if name == "email" {
					sawEmail = true
				}
				continue
			}
			switch name {
			case "email":
				sawEmail = true
			case "OPFMessageCopyFromAddresses":
				addrList = &msg.From
			case "OPFMessageCopySenderAddress":
				addrList = &msg.Sender
			case "OPFMessageCopyToAddresses":
				addrList = &msg.To
			case "OPFMessageCopyCCAddresses":
				addrList = &msg.CC
			case "OPFMessageCopyBCCAddresses":
				addrList = &msg.BCC
			case "emailAddress":
				if addrList != nil {
					*addrList = append(*addrList, Address{
						Name:  attr(t, "OPFContactEmailAddressName"),
						Email: attr(t, "OPFContactEmailAddressAddress"),
					})
				}
			case "messageAttachment":
				msg.Attachments = append(msg.Attachments, AttachmentRef{
					Name:        attr(t, "OPFAttachmentName"),
					ContentType: attr(t, "OPFAttachmentContentType"),
					ContentID:   attr(t, "OPFAttachmentContentID"),
					URL:         attr(t, "OPFAttachmentURL"),
				})
			case "OPFMessageCopySubject":
				textField = &msg.Subject
			case "OPFMessageCopyMessageID":
				textField = &msg.MessageID
			case "OPFMessageCopyInReplyTo":
				textField = &msg.InReplyTo
			case "OPFMessageCopyReferences":
				textField = &msg.References
			case "OPFMessageCopyThreadTopic":
				textField = &msg.ThreadTopic
			case "OPFMessageCopyThreadIndex":
				textField = &msg.ThreadIndex
			case "OPFMessageCopyBody":
				textField = &msg.BodyText
			case "OPFMessageCopyHTMLBody":
				textField = &msg.BodyHTML
			case "OPFMessageCopySentTime":
				timeField = &msg.SentAt
			case "OPFMessageCopyReceivedTime":
				timeField = &msg.ReceivedAt
			}
			if textField != nil || timeField != nil {
				text.Reset()
			}

		case xml.CharData:
			if textField != nil || timeField != nil {
				text.Write(t)
			}

		case xml.EndElement:
			switch t.Name.Local {
			case "OPFMessageCopyFromAddresses", "OPFMessageCopySenderAddress",
				"OPFMessageCopyToAddresses", "OPFMessageCopyCCAddresses", "OPFMessageCopyBCCAddresses":
				addrList = nil
			}
			if textField != nil {
				*textField = text.String()
				textField = nil
			}
			if timeField != nil {
				*timeField = parseOLMTime(strings.TrimSpace(text.String()))
				timeField = nil
			}
		}
	}

	if !sawEmail {
		return nil, ErrNotMessage
	}
	return msg, nil
}

// parseOLMTime parses the timestamps Outlook writes into OLM XML. They are
// UTC but carry no zone designator ("2022-05-26T17:55:40"); parsing them as
// local time would shift every date in the archive by the machine's UTC
// offset. Zoned forms are accepted too in case a future export adds them.
func parseOLMTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t
		}
	}
	return time.Time{}
}

func attr(el xml.StartElement, name string) string {
	for _, a := range el.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// charsetReader accepts the UTF-8 spellings Outlook uses in the XML
// declaration and rejects anything else explicitly rather than mis-decoding.
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "", "utf-8", "utf8":
		return input, nil
	}
	return nil, fmt.Errorf("unsupported olm xml charset %q", charset)
}
