package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sdawka/gh-runner-swarm/tui/internal/cli"
)

type fakeCLI struct {
	res cli.Result
}

func (f fakeCLI) StatusJSON(ctx context.Context) cli.Result { return f.res }

func TestFromCLISuccess(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "full.json"))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := FromCLI(context.Background(), fakeCLI{res: cli.Result{Stdout: string(data), ExitCode: 0}})
	if err != nil {
		t.Fatalf("FromCLI: %v", err)
	}
	if snap.Source != SourceCLI {
		t.Errorf("Source = %v, want SourceCLI", snap.Source)
	}
	if snap.DaemonPID != 41213 {
		t.Errorf("DaemonPID = %d, want 41213", snap.DaemonPID)
	}
}

func TestFromCLINonZeroExit(t *testing.T) {
	_, err := FromCLI(context.Background(), fakeCLI{res: cli.Result{
		ExitCode: 1,
		Stderr:   "Error: no configuration found\n",
	}})
	if err == nil {
		t.Fatal("FromCLI succeeded, want an error for a non-zero exit")
	}
}

func TestFromCLILaunchFailure(t *testing.T) {
	_, err := FromCLI(context.Background(), fakeCLI{res: cli.Result{Err: context.DeadlineExceeded}})
	if err == nil {
		t.Fatal("FromCLI succeeded, want an error for a launch failure")
	}
}
