package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/remote"
)

func TestShellQuote(t *testing.T) {
	for argument, expected := range map[string]string{
		"train.py":        "train.py",
		"--lr=3e-4":       "--lr=3e-4",
		"":                "''",
		"two words":       "'two words'",
		"it's":            `'it'\''s'`,
		"$HOME":           "'$HOME'",
		"a/b_c,d:e+f@g%h": "a/b_c,d:e+f@g%h",
	} {
		if quoted := shellQuote(argument); quoted != expected {
			t.Errorf("incorrect quoting of %q: %s", argument, quoted)
		}
	}
}

func TestFormatElapsed(t *testing.T) {
	for elapsed, expected := range map[time.Duration]string{
		0:                                "0s",
		1499 * time.Millisecond:          "1s",
		59 * time.Second:                 "59s",
		12*time.Minute + 5*time.Second:   "12m05s",
		3*time.Hour + 2*time.Minute + 40: "3h02m",
		50 * time.Hour:                   "50h00m",
	} {
		if formatted := formatElapsed(elapsed); formatted != expected {
			t.Errorf("incorrect formatting of %v: %s", elapsed, formatted)
		}
	}
}

func TestPrintJobs(t *testing.T) {
	started := time.Date(2026, 10, 22, 14, 3, 11, 0, time.Local)
	locator := &locator{root: "/project", base: "sub", contained: true}
	jobs := []*remote.Job{
		{ID: "3f9a1c07", State: remote.JobRunning, Elapsed: 12*time.Minute + 5*time.Second, Started: started,
			Log: "/server/project/sub/train.log", LogPath: "sub/train.log", Arguments: []string{"python", "train.py", "--name", "a b"}},
		{ID: "b2c4d6e8", State: remote.JobExited, ExitCode: 1, Elapsed: 3 * time.Second, Started: started,
			Log: "/home/u/.mutagen/remote-jobs/b2c4d6e8/log", Script: "-", Arguments: []string{"x\ny"}},
		{ID: "91aa0b3c", State: remote.JobLost, Started: started, Log: "/l", Arguments: []string{"sleep", "60"}},
	}
	var output strings.Builder
	if err := printJobs(&output, locator, "beta", jobs, 2); err != nil {
		t.Fatal(err)
	}
	expected := "" +
		"ID        STATE     ELAPSED  STARTED              LOG                                        COMMAND\n" +
		"3f9a1c07  running   12m05s   2026-10-22 14:03:11  train.log                                  python train.py --name 'a b'\n" +
		"b2c4d6e8  exited 1  3s       2026-10-22 14:03:11  /home/u/.mutagen/remote-jobs/b2c4d6e8/log  --script - 'x\\ny'\n" +
		"91aa0b3c  lost      -        2026-10-22 14:03:11  /l                                         sleep 60\n" +
		"(2 jobs started at other roots on beta aren't shown.)\n"
	if output.String() != expected {
		t.Fatalf("incorrect listing:\n%s\nexpected:\n%s", output.String(), expected)
	}

	output.Reset()
	if err := printJobs(&output, locator, "alpha", nil, 1); err != nil {
		t.Fatal(err)
	} else if output.String() != "No jobs were started at this session's root on alpha.\n(1 job started at another root on alpha isn't shown.)\n" {
		t.Fatalf("incorrect empty listing: %q", output.String())
	}
}
