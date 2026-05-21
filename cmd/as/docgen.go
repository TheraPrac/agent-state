package main

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

//go:embed preamble.md
var docgenPreamble string

//go:embed postamble.md
var docgenPostamble string

// docgenGroupOrder fixes the section order so regenerated output is
// deterministic — map iteration would churn the doc on every run.
// Any GroupID not listed here (including the implicit "other" bucket
// for commands without a GroupID) renders after these, alphabetically.
var docgenGroupOrder = []string{
	"queue-stack",
	"state-mgmt",
	"workflow",
	"testing",
	"uat-pipeline",
	"querying",
	"deps",
	"epics-sprints-notes",
	"arcs",
	"agents",
	"maintenance",
}

func newDocgenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "docgen",
		Short:  "Regenerate docs/st-cli-reference.md from the live cobra tree",
		Hidden: true,
		Args:   cobra.NoArgs,
		Run: func(c *cobra.Command, args []string) {
			out, _ := c.Flags().GetString("output")
			var w io.Writer = os.Stdout
			if out != "" {
				f, err := os.Create(out)
				if err != nil {
					fmt.Fprintf(os.Stderr, "docgen: %v\n", err)
					exitCode = 1
					return
				}
				defer f.Close()
				w = f
			}
			if err := renderDocs(w, c.Root()); err != nil {
				fmt.Fprintf(os.Stderr, "docgen: %v\n", err)
				exitCode = 1
				return
			}
			if out != "" {
				fmt.Fprintf(os.Stderr, "Regenerated %s\n", out)
			}
		},
	}
	cmd.Flags().StringP("output", "o", "", "write to file (default: stdout)")
	return cmd
}

// renderDocs writes preamble + auto-generated command reference + postamble.
func renderDocs(w io.Writer, root *cobra.Command) error {
	if _, err := io.WriteString(w, docgenPreamble); err != nil {
		return err
	}
	if !strings.HasSuffix(docgenPreamble, "\n") {
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "## Command reference\n"); err != nil {
		return err
	}

	groups := collectGroups(root)
	rendered := map[string]bool{}
	for _, gid := range docgenGroupOrder {
		if cmds := groups[gid]; len(cmds) > 0 {
			renderGroup(w, root, gid, cmds)
			rendered[gid] = true
		}
	}
	var extra []string
	for gid := range groups {
		if !rendered[gid] {
			extra = append(extra, gid)
		}
	}
	sort.Strings(extra)
	for _, gid := range extra {
		renderGroup(w, root, gid, groups[gid])
	}

	if docgenPostamble != "" {
		if !strings.HasPrefix(docgenPostamble, "\n") {
			if _, err := io.WriteString(w, "\n"); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, docgenPostamble); err != nil {
			return err
		}
		if !strings.HasSuffix(docgenPostamble, "\n") {
			if _, err := io.WriteString(w, "\n"); err != nil {
				return err
			}
		}
	}
	return nil
}

// collectGroups buckets the root's direct children by GroupID. Commands
// without a GroupID land in the "other" bucket. Hidden commands and
// help/completion plumbing are skipped.
func collectGroups(root *cobra.Command) map[string][]*cobra.Command {
	out := map[string][]*cobra.Command{}
	for _, c := range root.Commands() {
		if !shouldInclude(c) {
			continue
		}
		gid := c.GroupID
		if gid == "" {
			gid = "other"
		}
		out[gid] = append(out[gid], c)
	}
	for gid := range out {
		sort.SliceStable(out[gid], func(i, j int) bool {
			return cmdName(out[gid][i]) < cmdName(out[gid][j])
		})
	}
	return out
}

func shouldInclude(c *cobra.Command) bool {
	if c.Hidden {
		return false
	}
	if !c.IsAvailableCommand() {
		return false
	}
	switch c.Name() {
	case "help", "completion":
		return false
	}
	return true
}

func renderGroup(w io.Writer, root *cobra.Command, gid string, cmds []*cobra.Command) {
	title := groupTitle(root, gid)
	fmt.Fprintf(w, "\n### %s\n\n", title)
	lines := []docLine{}
	for _, c := range cmds {
		lines = append(lines, collectCommandLines(c, "st")...)
	}
	maxUse := 0
	for _, l := range lines {
		if n := utf8.RuneCountInString(l.use); n > maxUse {
			maxUse = n
		}
	}
	fmt.Fprintln(w, "```bash")
	for _, l := range lines {
		if l.short == "" {
			fmt.Fprintln(w, l.use)
			continue
		}
		pad := maxUse - utf8.RuneCountInString(l.use)
		if pad < 0 {
			pad = 0
		}
		fmt.Fprintf(w, "%s%s   # %s\n", l.use, strings.Repeat(" ", pad), l.short)
	}
	fmt.Fprintln(w, "```")
}

type docLine struct {
	use   string
	short string
}

// collectCommandLines flattens a top-level command into one or more bash
// lines. Leaves render as a single line; containers expand to their
// visible subcommands. Two-level containers (rare) expand further so a
// command like `st sprint plan` shows up explicitly instead of just `st
// sprint`.
func collectCommandLines(c *cobra.Command, prefix string) []docLine {
	subs := visibleSubcommands(c)
	if len(subs) == 0 {
		return []docLine{{use: prefix + " " + c.Use, short: c.Short}}
	}
	var lines []docLine
	childPrefix := prefix + " " + cmdName(c)
	for _, s := range subs {
		gc := visibleSubcommands(s)
		if len(gc) == 0 {
			lines = append(lines, docLine{
				use:   childPrefix + " " + s.Use,
				short: s.Short,
			})
			continue
		}
		grandPrefix := childPrefix + " " + cmdName(s)
		for _, g := range gc {
			lines = append(lines, docLine{
				use:   grandPrefix + " " + g.Use,
				short: g.Short,
			})
		}
	}
	return lines
}

func visibleSubcommands(c *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	for _, s := range c.Commands() {
		if !shouldInclude(s) {
			continue
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return cmdName(out[i]) < cmdName(out[j])
	})
	return out
}

func cmdName(c *cobra.Command) string {
	use := c.Use
	if i := strings.IndexAny(use, " \t"); i >= 0 {
		return use[:i]
	}
	return use
}

func groupTitle(root *cobra.Command, gid string) string {
	if gid == "other" {
		return "Other"
	}
	for _, g := range root.Groups() {
		if g.ID == gid {
			return g.Title
		}
	}
	return gid
}
