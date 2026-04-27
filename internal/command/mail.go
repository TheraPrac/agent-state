package command

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/mail"
	"github.com/jfinlinson/agent-state/internal/store"
)

// MailSendOpts holds flags for `st mail send`.
type MailSendOpts struct {
	Kind string
	Body string
	Item string // optional related item id
	From string // override identity (default: cfg.Identity().ID)
}

// MailSend writes a message into <to>'s mailbox and st-syncs so the
// recipient's next poll picks it up. T-313.
func MailSend(s *store.Store, cfg *config.Config, to string, opts MailSendOpts) int {
	from := opts.From
	if from == "" {
		from = cfg.Identity().ID
	}
	if from == "" {
		fmt.Fprintln(os.Stderr, "no agent identity resolved — set --from or run from a per-agent dir")
		return 1
	}
	id, err := mail.Send(cfg, mail.Message{
		From: from,
		To:   to,
		Kind: mail.Kind(opts.Kind),
		Body: opts.Body,
		Item: opts.Item,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mail send: %v\n", err)
		return 1
	}
	fmt.Printf("Sent %s → %s (kind=%s, id=%s)\n", from, to, opts.Kind, id)
	if err := s.GitSync(fmt.Sprintf("mail send: %s -> %s (%s)", from, to, opts.Kind)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: sync after send failed: %v\n", err)
	}
	return 0
}

// MailListOpts holds flags for `st mail list`.
type MailListOpts struct {
	Agent string // recipient (default: this agent)
}

// MailList prints pending messages for the recipient. Read-only;
// nothing is moved to archive — that's MailArchive or st run's poll.
func MailList(cfg *config.Config, opts MailListOpts) int {
	recipient := opts.Agent
	if recipient == "" {
		recipient = cfg.Identity().ID
	}
	if recipient == "" {
		fmt.Fprintln(os.Stderr, "no agent identity resolved — set --agent or run from a per-agent dir")
		return 1
	}
	msgs, err := mail.List(cfg, recipient)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mail list: %v\n", err)
		return 1
	}
	if len(msgs) == 0 {
		fmt.Printf("No pending mail for %s\n", recipient)
		return 0
	}
	renderMailList(os.Stdout, recipient, msgs)
	return 0
}

// MailShow prints a single message by id, from pending or archive.
// Does NOT consume — that's the operator-facing read.
func MailShow(cfg *config.Config, recipient, id string) int {
	if recipient == "" {
		recipient = cfg.Identity().ID
	}
	msg, err := mail.Show(cfg, recipient, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mail show: %v\n", err)
		return 1
	}
	if msg == nil {
		fmt.Fprintf(os.Stderr, "mail show: %s not found in %s mailbox\n", id, recipient)
		return 1
	}
	renderMailMessage(os.Stdout, msg)
	return 0
}

// MailArchive moves a pending message to archive. Read-receipt for the
// operator; st run's poller does this automatically.
func MailArchive(s *store.Store, cfg *config.Config, recipient, id string) int {
	if recipient == "" {
		recipient = cfg.Identity().ID
	}
	if err := mail.Archive(cfg, recipient, id); err != nil {
		fmt.Fprintf(os.Stderr, "mail archive: %v\n", err)
		return 1
	}
	fmt.Printf("Archived %s\n", id)
	if err := s.GitSync(fmt.Sprintf("mail archive: %s/%s", recipient, id)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: sync after archive failed: %v\n", err)
	}
	return 0
}

// pollAndSurfaceMail is called between pipeline steps in st run. It
// pulls all pending mail for the active agent, prints a banner per
// message, and returns true when ANY blocking message (alert/pause)
// was surfaced — the caller halts the pipeline.
//
// Mail is consumed (moved to archive) as a side effect of Poll, so a
// subsequent invocation only sees newly-arrived messages. T-313.
func pollAndSurfaceMail(cfg *config.Config) bool {
	recipient := cfg.Identity().ID
	if recipient == "" {
		return false
	}
	msgs, err := mail.Poll(cfg, recipient)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: mail poll: %v\n", err)
		return false
	}
	blocking := false
	for _, m := range msgs {
		printMailBanner(os.Stderr, &m)
		if m.Kind.IsBlocking() {
			blocking = true
		}
	}
	return blocking
}

// --- rendering ---

func renderMailList(w io.Writer, recipient string, msgs []mail.Message) {
	fmt.Fprintf(w, "%sPending mail for %s%s (%d)\n", cBold, recipient, cReset, len(msgs))
	for _, m := range msgs {
		var color string
		switch m.Kind {
		case mail.KindAlert, mail.KindPause:
			color = cRed
		case mail.KindWarning, mail.KindNeedHelp:
			color = cYellow
		case mail.KindResume:
			color = cGreen
		default:
			color = cCyan
		}
		fmt.Fprintf(w, "  %s%s%s  %s%-9s%s  from %s",
			cDim, m.At, cReset,
			color, m.Kind, cReset,
			m.From,
		)
		if m.Item != "" {
			fmt.Fprintf(w, " (%s)", m.Item)
		}
		fmt.Fprintf(w, "\n    %s\n", m.Body)
		fmt.Fprintf(w, "    %sid: %s%s\n", cDim, m.ID, cReset)
	}
}

func renderMailMessage(w io.Writer, m *mail.Message) {
	fmt.Fprintf(w, "%sid:%s   %s\n", cBold, cReset, m.ID)
	fmt.Fprintf(w, "%sfrom:%s %s\n", cBold, cReset, m.From)
	fmt.Fprintf(w, "%sto:%s   %s\n", cBold, cReset, m.To)
	fmt.Fprintf(w, "%skind:%s %s\n", cBold, cReset, m.Kind)
	fmt.Fprintf(w, "%sat:%s   %s\n", cBold, cReset, m.At)
	if m.Item != "" {
		fmt.Fprintf(w, "%sitem:%s %s\n", cBold, cReset, m.Item)
	}
	fmt.Fprintf(w, "%sbody:%s\n  %s\n", cBold, cReset, strings.ReplaceAll(m.Body, "\n", "\n  "))
}

// printMailBanner is the one-line surfacing format for st run's poller.
// Blocking kinds get a louder marker so the operator notices the halt.
func printMailBanner(w io.Writer, m *mail.Message) {
	marker := "✉"
	color := cCyan
	switch m.Kind {
	case mail.KindAlert:
		marker = "⛔"
		color = cRed
	case mail.KindPause:
		marker = "⏸"
		color = cRed
	case mail.KindWarning:
		marker = "⚠"
		color = cYellow
	case mail.KindNeedHelp:
		marker = "🆘"
		color = cYellow
	case mail.KindResume:
		marker = "▶"
		color = cGreen
	}
	itemTag := ""
	if m.Item != "" {
		itemTag = " (" + m.Item + ")"
	}
	fmt.Fprintf(w, "%s%s mail from %s [%s]%s%s: %s\n", color, marker, m.From, m.Kind, itemTag, cReset, m.Body)
}
