package cli

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// recordingRunner captures the args and timeout of the last Run call and
// returns a canned Result.
type recordingRunner struct {
	gotArgs    []string
	gotTimeout time.Duration
	result     Result
}

func (r *recordingRunner) Run(ctx context.Context, timeout time.Duration, args ...string) Result {
	r.gotArgs = args
	r.gotTimeout = timeout
	return r.result
}

func TestScaleSortsAndBuildsArgv(t *testing.T) {
	r := &recordingRunner{}
	c := NewClient(r)

	c.Scale(context.Background(), map[string]int{
		"https://github.com/z/z": 1,
		"https://github.com/a/a": 3,
	})

	want := []string{
		"--scale", "https://github.com/a/a=3",
		"--scale", "https://github.com/z/z=1",
	}
	if !reflect.DeepEqual(r.gotArgs, want) {
		t.Errorf("argv = %v, want %v", r.gotArgs, want)
	}
	if r.gotTimeout != LongTimeout {
		t.Errorf("timeout = %v, want LongTimeout (%v)", r.gotTimeout, LongTimeout)
	}
}

func TestSingleRunnerVerbs(t *testing.T) {
	cases := []struct {
		name string
		call func(c Client) Result
		want []string
	}{
		{"drain", func(c Client) Result { return c.Drain(context.Background(), 3) }, []string{"--drain", "3"}},
		{"stop", func(c Client) Result { return c.Stop(context.Background(), 3) }, []string{"--stop", "3"}},
		{"start", func(c Client) Result { return c.Start(context.Background(), 3) }, []string{"--start", "3"}},
		{"remove", func(c Client) Result { return c.Remove(context.Background(), 3) }, []string{"--remove", "3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &recordingRunner{}
			c := NewClient(r)
			tc.call(c)
			if !reflect.DeepEqual(r.gotArgs, tc.want) {
				t.Errorf("argv = %v, want %v", r.gotArgs, tc.want)
			}
			if r.gotTimeout != DefaultTimeout {
				t.Errorf("timeout = %v, want DefaultTimeout", r.gotTimeout)
			}
		})
	}
}

func TestSetBoundsClearForm(t *testing.T) {
	r := &recordingRunner{}
	c := NewClient(r)

	c.SetBounds(context.Background(), "https://github.com/a/a", 0, 0)
	want := []string{"--set-bounds", "https://github.com/a/a=none"}
	if !reflect.DeepEqual(r.gotArgs, want) {
		t.Errorf("argv = %v, want %v", r.gotArgs, want)
	}

	c.SetBounds(context.Background(), "https://github.com/a/a", 1, 5)
	want = []string{"--set-bounds", "https://github.com/a/a=1-5"}
	if !reflect.DeepEqual(r.gotArgs, want) {
		t.Errorf("argv = %v, want %v", r.gotArgs, want)
	}
}

func TestOtherVerbsArgv(t *testing.T) {
	cases := []struct {
		name string
		call func(c Client) Result
		want []string
	}{
		{"add-target", func(c Client) Result { return c.AddTarget(context.Background(), "owner/repo") }, []string{"--add-target", "owner/repo"}},
		{"remove-target", func(c Client) Result { return c.RemoveTarget(context.Background(), "https://github.com/a/a") }, []string{"--remove-target", "https://github.com/a/a"}},
		{"set-config", func(c Client) Result { return c.SetConfig(context.Background(), "MAX_RUNNERS", "10") }, []string{"--set-config", "MAX_RUNNERS=10"}},
		{"poll", func(c Client) Result { return c.Poll(context.Background()) }, []string{"--poll"}},
		{"gh-status", func(c Client) Result { return c.GHStatus(context.Background()) }, []string{"--gh-status"}},
		{"stop-all", func(c Client) Result { return c.StopAll(context.Background()) }, []string{"--stop-all"}},
		{"start-all", func(c Client) Result { return c.StartAll(context.Background()) }, []string{"--start-all"}},
		{"install-service", func(c Client) Result { return c.InstallService(context.Background()) }, []string{"--install-service"}},
		{"stop-daemon", func(c Client) Result { return c.StopDaemon(context.Background()) }, []string{"--stop-daemon"}},
		{"status-json", func(c Client) Result { return c.StatusJSON(context.Background()) }, []string{"--status", "--json"}},
		{"version", func(c Client) Result { return c.Version(context.Background()) }, []string{"--version"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &recordingRunner{}
			c := NewClient(r)
			tc.call(c)
			if !reflect.DeepEqual(r.gotArgs, tc.want) {
				t.Errorf("argv = %v, want %v", r.gotArgs, tc.want)
			}
		})
	}
}

func TestLongTimeoutVerbs(t *testing.T) {
	r := &recordingRunner{}
	c := NewClient(r)
	c.Download(context.Background())
	if r.gotTimeout != LongTimeout {
		t.Errorf("Download timeout = %v, want LongTimeout", r.gotTimeout)
	}
}
