// Package inbound reads the email sent to ServiceLater (docs/apps-platform.md §6.5): a reply to a notification
// becomes a comment on its record, as the person it was sent to; a new email from an allowed, DMARC-authenticated
// domain files an incident. The mailbox is IMAP or a Microsoft 365 mailbox read through Graph (mailbox.go); what
// each message becomes is process.go.
package inbound

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	_ "github.com/emersion/go-message/charset" // decode the charsets mail is written in, not only UTF-8
	"github.com/emersion/go-message/mail"
)

const (
	// MaxMessage is the largest message read whole: bigger ones are logged and left.
	MaxMessage = 30 << 20
	// maxFiles and maxFileBytes bound what one email attaches to a record.
	maxFiles     = 10
	maxFileBytes = 25 << 20
	maxText      = 10000
)

// File is an attachment of an email.
type File struct {
	Name, ContentType string
	Data              []byte
}

// Mail is what processing needs of a message.
type Mail struct {
	MessageID string
	// From is the sender's address, lowercased; FromName their display name.
	From, FromName string
	Subject        string
	// InReplyTo and References are the Message-IDs the message answers, which is how a reply is matched.
	InReplyTo, References []string
	// Text is the message's text: its plain part, or its HTML part as text.
	Text string
	// Files are its attachments (up to maxFiles, maxFileBytes in all); Dropped says how many were left out.
	Files   []File
	Dropped int
	// Automatic says no person wrote it: an auto-reply, a bounce, a mailing list. It is never answered or filed.
	Automatic bool
	// AuthResults are its Authentication-Results headers, topmost (the receiving server's) first.
	AuthResults []string
}

// Domain is the sender's domain.
func (m *Mail) Domain() string {
	_, d, _ := strings.Cut(m.From, "@")
	return d
}

// Parse reads a message.
func Parse(raw []byte) (*Mail, error) {
	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil && mr == nil {
		return nil, fmt.Errorf("not an email: %w", err)
	}
	h := mr.Header
	m := &Mail{}
	m.MessageID, _ = h.MessageID()
	if from, err := h.AddressList("From"); err == nil && len(from) > 0 {
		m.From, m.FromName = strings.ToLower(from[0].Address), from[0].Name
	}
	m.Subject, _ = h.Subject()
	m.InReplyTo, _ = h.MsgIDList("In-Reply-To")
	m.References, _ = h.MsgIDList("References")
	m.AuthResults = h.Values("Authentication-Results")
	m.Automatic = automatic(h, m.From)

	var plain, htmlText string
	total := 0
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if plain == "" && htmlText == "" {
				return nil, fmt.Errorf("reading the email: %w", err)
			}
			break // keep what was read
		}
		switch ph := p.Header.(type) {
		case *mail.InlineHeader:
			ct, _, _ := ph.ContentType()
			body, _ := io.ReadAll(io.LimitReader(p.Body, 1<<20))
			switch {
			case ct == "text/plain" && plain == "":
				plain = string(body)
			case ct == "text/html" && htmlText == "":
				htmlText = htmlToText(string(body))
			}
		case *mail.AttachmentHeader:
			name, _ := ph.Filename()
			ct, _, _ := ph.ContentType()
			data, _ := io.ReadAll(io.LimitReader(p.Body, maxFileBytes+1))
			if name == "" || len(m.Files) >= maxFiles || total+len(data) > maxFileBytes {
				m.Dropped++
				continue
			}
			total += len(data)
			m.Files = append(m.Files, File{Name: name, ContentType: ct, Data: data})
		}
	}
	m.Text = plain
	if strings.TrimSpace(m.Text) == "" {
		m.Text = htmlText
	}
	m.Text = strings.ReplaceAll(m.Text, "\r\n", "\n")
	return m, nil
}

// automatic says a message was sent by a program, not a person (RFC 3834 and the usual conventions): answering or
// filing it could loop.
func automatic(h mail.Header, from string) bool {
	if v := strings.ToLower(strings.TrimSpace(h.Get("Auto-Submitted"))); v != "" && v != "no" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(h.Get("Precedence"))) {
	case "bulk", "junk", "list", "auto_reply":
		return true
	}
	if h.Has("X-Autoreply") || h.Has("X-Autorespond") || h.Has("List-Id") || h.Get("X-Auto-Response-Suppress") == "All" {
		return true
	}
	if rp := strings.TrimSpace(h.Get("Return-Path")); rp == "<>" {
		return true
	}
	local, _, _ := strings.Cut(from, "@")
	return local == "mailer-daemon" || local == "postmaster" || local == "noreply" || local == "no-reply"
}

var (
	breakRE = regexp.MustCompile(`(?i)<br\s*/?>|</p\s*>|</div\s*>|</li\s*>|</tr\s*>|</h[1-6]\s*>`)
	dropRE  = regexp.MustCompile(`(?is)<(style|script|head)[^>]*>.*?</(style|script|head)\s*>`)
	tagRE   = regexp.MustCompile(`(?s)<[^>]*>`)
	blankRE = regexp.MustCompile(`\n{3,}`)
)

// htmlToText is an HTML body's words, with line breaks where paragraphs and breaks were.
func htmlToText(s string) string {
	s = dropRE.ReplaceAllString(s, "")
	s = breakRE.ReplaceAllString(s, "\n")
	s = tagRE.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(strings.Join(strings.Fields(l), " "), " ")
	}
	return strings.TrimSpace(blankRE.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

var (
	wroteRE    = regexp.MustCompile(`(?i)^(on\b.{0,200}\bwrote:|le\b.{0,200}\ba écrit\s?:|am\b.{0,200}\bschrieb\b.{0,200}:)\s*$`)
	onStartRE  = regexp.MustCompile(`(?i)^on\s.{0,200}$`)
	origRE     = regexp.MustCompile(`(?i)^-{2,}\s*(original message|forwarded message)\s*-{2,}`)
	headerRE   = regexp.MustCompile(`(?i)^(from|von|de)\s*:\s*\S`)
	sentRE     = regexp.MustCompile(`(?i)^(sent|date|gesendet|envoyé|to)\s*:`)
	ruleLineRE = regexp.MustCompile(`^_{10,}\s*$`)
)

// Reply is the new part of a reply: the text above the quoted message ("On … wrote:", an Outlook header block,
// "-----Original Message-----"), without ">" lines or a "-- " signature, and no longer than a comment may be.
func Reply(text string) string {
	lines := strings.Split(text, "\n")
	var out []string
cut:
	for i, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case wroteRE.MatchString(t), origRE.MatchString(t), ruleLineRE.MatchString(t):
			break cut
		case onStartRE.MatchString(t) && i+1 < len(lines) && strings.HasSuffix(strings.TrimSpace(lines[i+1]), "wrote:"):
			break cut // "On Mon, 17 Nov at 10:00, Someone <a@b>" / "wrote:"
		case headerRE.MatchString(t) && i+1 < len(lines) && sentRE.MatchString(strings.TrimSpace(lines[i+1])):
			break cut // Outlook: "From: …" then "Sent: …"
		case l == "-- " || t == "--":
			break cut
		case strings.HasPrefix(t, ">"):
			continue
		}
		out = append(out, strings.TrimRight(l, " \t"))
	}
	s := strings.TrimSpace(blankRE.ReplaceAllString(strings.Join(out, "\n"), "\n\n"))
	if utf8.RuneCountInString(s) > maxText {
		s = string([]rune(s)[:maxText-1]) + "…"
	}
	return s
}
