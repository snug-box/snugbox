package cli

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var rewrite = flag.Bool("update", false, "rewrite docs/reference/commands.md from the commands")

// referencePath is the command reference, generated from the commands
// themselves so it can't drift from them.
var referencePath = filepath.Join("..", "..", "..", "docs", "reference", "commands.md")

func TestCommandReferenceIsCurrent(t *testing.T) {
	got := commandReference(newRoot(&env{}))
	if *rewrite {
		if err := os.MkdirAll(filepath.Dir(referencePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(referencePath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(referencePath)
	if err != nil {
		t.Fatalf("%v; run go test ./agent/internal/cli -run TestCommandReferenceIsCurrent -update", err)
	}
	if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), got) {
		t.Errorf("%s is out of date; run go test ./agent/internal/cli -run TestCommandReferenceIsCurrent -update", referencePath)
	}
}

func commandReference(root *cobra.Command) []byte {
	var b bytes.Buffer
	b.WriteString("# Command reference\n\n")
	b.WriteString("This page is generated from the commands themselves by `go test ./agent/internal/cli -run TestCommandReferenceIsCurrent -update`; edit the commands, not this file.\n\n")
	b.WriteString("Every command takes `--help`. Colour is used only on a terminal, and never when `NO_COLOR` is set.\n\n")
	b.WriteString("| Exit code | Meaning |\n| --- | --- |\n")
	for _, c := range []struct {
		code int
		what string
	}{{ExitOK, "done"}, {ExitError, "an error"}, {ExitUsage, "wrong usage"}, {ExitRisks, "the audit found risks"}, {ExitNeedsRoot, "the command needs root"}} {
		fmt.Fprintf(&b, "| %d | %s |\n", c.code, c.what)
	}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if !sub.IsAvailableCommand() || sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			if sub.Runnable() {
				writeCommand(&b, sub)
			}
			walk(sub)
		}
	}
	walk(root)
	return b.Bytes()
}

func writeCommand(b *bytes.Buffer, c *cobra.Command) {
	fmt.Fprintf(b, "\n## %s\n\n%s.\n\n```text\n%s\n```\n", c.CommandPath(), c.Short, c.UseLine())
	if c.Long != "" {
		fmt.Fprintf(b, "\n%s\n", strings.Join(strings.Fields(c.Long), " "))
	}
	if c.Example != "" {
		fmt.Fprintf(b, "\nExamples:\n\n```sh\n%s\n```\n", strings.TrimRight(dedent(c.Example), "\n"))
	}
	var flags []*pflag.Flag
	c.NonInheritedFlags().VisitAll(func(f *pflag.Flag) {
		if f.Name != "help" && !f.Hidden {
			flags = append(flags, f)
		}
	})
	if len(flags) == 0 {
		return
	}
	b.WriteString("\n| Flag | Default | Does |\n| --- | --- | --- |\n")
	for _, f := range flags {
		def := f.DefValue
		switch {
		case f.Value.Type() == "bool" && def == "false", def == "" || def == "[]":
			def = ""
		default:
			def = "`" + def + "`"
		}
		fmt.Fprintf(b, "| `--%s` | %s | %s |\n", f.Name, def, strings.ReplaceAll(f.Usage, "|", "\\|"))
	}
}

// dedent removes the two spaces cobra examples are indented with.
func dedent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimPrefix(l, "  ")
	}
	return strings.Join(lines, "\n")
}
