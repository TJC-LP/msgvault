package olm

import (
	"context"
	"strings"
)

// AddressBook maps recipient display names to email addresses learned from
// the archive itself.
//
// Most OLM message records list recipients only in OPFMessageCopyDisplayTo, a
// semicolon-separated string of display names, while the emailAddress
// elements that do carry addresses (From, Sender, the occasional To/CC)
// use the same Exchange display names. Counting every name/address pairing
// across the archive therefore recovers addresses for the bulk of name-only
// recipients.
type AddressBook struct {
	// counts[name][email] is how often the pairing appeared.
	counts map[string]map[string]int
	// MinShare is the fraction of a name's sightings its most common address
	// must hold to be trusted. Defaults to 0.7. Names that appear under
	// several addresses more evenly than that stay unresolved rather than
	// guessing.
	MinShare float64
}

// NewAddressBook returns an empty address book.
func NewAddressBook() *AddressBook {
	return &AddressBook{counts: make(map[string]map[string]int), MinShare: 0.7}
}

// Learn records every name/address pairing found on a message.
func (b *AddressBook) Learn(msg *Message) {
	for _, list := range [][]Address{msg.From, msg.Sender, msg.To, msg.CC, msg.BCC, msg.ReplyTo} {
		for _, a := range list {
			b.add(a.Name, a.Email)
		}
	}
}

func (b *AddressBook) add(name, email string) {
	name = normalizeName(name)
	email = strings.ToLower(strings.TrimSpace(email))
	if name == "" || email == "" || !strings.Contains(email, "@") {
		return
	}
	m := b.counts[name]
	if m == nil {
		m = make(map[string]int)
		b.counts[name] = m
	}
	m[email]++
}

// Resolve returns the address most often paired with a display name when
// that pairing dominates the name's sightings.
func (b *AddressBook) Resolve(name string) (string, bool) {
	if b == nil {
		return "", false
	}
	m := b.counts[normalizeName(name)]
	if len(m) == 0 {
		return "", false
	}
	var (
		best      string
		bestCount int
		total     int
	)
	for email, c := range m {
		total += c
		if c > bestCount || (c == bestCount && email < best) {
			best, bestCount = email, c
		}
	}
	share := b.MinShare
	if share <= 0 {
		share = 0.7
	}
	if float64(bestCount) < share*float64(total) {
		return "", false
	}
	return best, true
}

// Len reports how many distinct display names the book knows.
func (b *AddressBook) Len() int {
	if b == nil {
		return 0
	}
	return len(b.counts)
}

// normalizeName folds case and whitespace so "Reader, Bea" and "reader,  bea"
// share one entry.
func normalizeName(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

// BuildAddressBook parses every message in the archive once and returns the
// learned name/address pairings. It stops early when ctx is cancelled.
func BuildAddressBook(ctx context.Context, a *Archive) (*AddressBook, error) {
	book := NewAddressBook()
	for _, folder := range a.Folders() {
		for _, ref := range folder.Messages {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			rc, err := a.OpenMessage(ref)
			if err != nil {
				continue // unreadable entries are reported by the import pass
			}
			msg, err := ParseMessage(rc)
			_ = rc.Close()
			if err != nil {
				continue
			}
			book.Learn(msg)
		}
	}
	return book, nil
}
