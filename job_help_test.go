package main

import (
	"flag"
	"io"
	"regexp"
	"strings"
	"testing"
)

// THE TOP-LEVEL HELP BLOCK IS A SECOND COPY OF THE FLAG SET, AND IT DRIFTED.
//
// `aq --help` documented three of the fourteen flags `aq job create` accepts:
// --name, --on and --secret. Every flag the image-source path added (--image,
// --gpu-model, --any-gpu, --gpu-order, --disk-gb, --registry-secret,
// --output-path) and both checkpoint flags were invisible at the one place a
// user looks before reaching for a subcommand's own -h. So the whole
// image-source way of creating a job read as if it did not exist.
//
// This guard reads the bytes the user sees and compares them against the flag
// set the parser actually registers, so a new flag cannot land undocumented.
func TestTopLevelHelpDocumentsEveryJobFlag(t *testing.T) {
	// The block under the "job:" heading is the only place a `aq job` flag may
	// be documented; finding "--image" somewhere else in the help would not
	// help anyone reading about jobs.
	jobSection := helpSection(t, "job:")

	for _, c := range []struct {
		command  string
		register func(*flag.FlagSet)
	}{
		{"aq job create", func(fs *flag.FlagSet) { registerJobCreateFlags(fs) }},
		{"aq job run", func(fs *flag.FlagSet) { registerJobRunFlags(fs) }},
		{"aq job pull", func(fs *flag.FlagSet) { registerJobPullFlags(fs) }},
	} {
		fs := flag.NewFlagSet(c.command, flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		c.register(fs)

		var missing []string
		fs.VisitAll(func(fl *flag.Flag) {
			if !documentsFlag(jobSection, fl.Name) {
				missing = append(missing, "--"+fl.Name)
			}
		})
		if len(missing) > 0 {
			t.Errorf("%s accepts flags the top-level help's job: section never names: %s\n"+
				"Add them to usageText in main.go (reuse the flag's own usage string), or, if a flag was "+
				"removed, delete it from the flag set rather than from this guard.", c.command, strings.Join(missing, " "))
		}
	}
}

// documentsFlag reports whether the help names exactly this flag. A plain
// substring test is not enough, and this is not hypothetical: it is how the
// first version of this guard passed its own negative control. "--disk-gb" is
// a substring of "--disk-gb-XX", so renaming a documented flag would leave the
// guard still satisfied for the name that is now gone. The boundary classes
// are every character that could continue a flag name.
//
// A one-character flag is spelled with ONE dash in this help ("-f", "-n"),
// which is how users write it and what Go's flag package accepts either way,
// so it is matched in that form rather than as "--f".
func documentsFlag(help, name string) bool {
	dashes := "--"
	if len(name) == 1 {
		dashes = "-"
	}
	return regexp.MustCompile(`(^|[^A-Za-z0-9-])` + dashes + regexp.QuoteMeta(name) + `([^A-Za-z0-9-]|$)`).MatchString(help)
}

// The guard above is only worth anything if the section it reads is the real
// one and the match it makes has a boundary, so assert both here rather than
// trusting that a passing run means anything.
func TestHelpSectionIsBoundedToItsOwnSection(t *testing.T) {
	jobSection := helpSection(t, "job:")
	if documentsFlag(jobSection, "warn-after") {
		t.Error("the job: section bled into the idle: section that follows it; helpSection is not bounding")
	}
	if !strings.Contains(jobSection, "aq job create") {
		t.Error("the job: section does not contain `aq job create`; helpSection is reading the wrong text")
	}
	// A name that is only a PREFIX of a documented flag must not count as
	// documented, or the guard cannot see a rename.
	if documentsFlag(jobSection, "disk") {
		t.Error("documentsFlag matched --disk on the strength of --disk-gb; the boundary is not holding")
	}
	// A one-character flag is only ever written "-f" here, so the matcher has
	// to look for that spelling; searching for "--f" found nothing and made
	// `aq job run` look undocumented when it was not.
	if !documentsFlag(jobSection, "f") {
		t.Error("documentsFlag did not find the short flag -f in the job: section")
	}
}

// helpSection returns usageText from the given heading up to the next
// column-0 heading, failing if the heading is gone (a renamed section must
// rename this guard with it, not silently match nothing).
func helpSection(t *testing.T, heading string) string {
	t.Helper()
	start := strings.Index(usageText, "\n"+heading+"\n")
	if start < 0 {
		t.Fatalf("no %q section in the top-level help; if it was renamed, rename it here too", heading)
	}
	rest := usageText[start+len(heading)+2:]
	lines := strings.Split(rest, "\n")
	for i, line := range lines {
		if line != "" && !strings.HasPrefix(line, " ") {
			return strings.Join(lines[:i], "\n")
		}
	}
	return rest
}
