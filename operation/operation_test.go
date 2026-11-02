package operation

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arl/gitstatus"
)

// testRepo is a throwaway Git repository used to exercise the collector
// against the real on-disk metadata Git maintains.
type testRepo struct {
	dir string
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	r := &testRepo{dir: dir}
	r.must(t, "init", "-q", "-b", "main")
	r.must(t, "config", "user.email", "t@example.com")
	r.must(t, "config", "user.name", "Tester")
	return r
}

func (r *testRepo) must(t *testing.T, args ...string) string {
	t.Helper()
	out, err := r.run(args...)
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// fail runs a git command that is expected to stop with a non-zero status
// (conflict, empty commit, ...).
func (r *testRepo) fail(t *testing.T, args ...string) {
	t.Helper()
	if _, err := r.run(args...); err == nil {
		t.Fatalf("git %s unexpectedly succeeded", strings.Join(args, " "))
	}
}

func (r *testRepo) run(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GIT_EDITOR=true")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (r *testRepo) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (r *testRepo) commit(t *testing.T, name, file, content string) {
	t.Helper()
	r.write(t, file, content)
	r.must(t, "add", file)
	r.must(t, "commit", "-q", "-m", name)
}

func (r *testRepo) gitDirFile(elems ...string) string {
	return filepath.Join(append([]string{r.dir, ".git"}, elems...)...)
}

func (r *testRepo) rmGitFile(t *testing.T, elems ...string) {
	t.Helper()
	if err := os.Remove(r.gitDirFile(elems...)); err != nil {
		t.Fatal(err)
	}
}

func (r *testRepo) writeGitFile(t *testing.T, content string, elems ...string) {
	t.Helper()
	p := r.gitDirFile(elems...)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// collect chdirs into the repository (gitstatus works on the current
// directory) and collects the operation info.
func (r *testRepo) collect(t *testing.T) *Info {
	t.Helper()
	t.Chdir(r.dir)
	st, err := gitstatus.NewWithContext(context.Background())
	if err != nil {
		t.Fatalf("gitstatus: %v", err)
	}
	return Collect(context.Background(), st)
}

// divergentRebaseRepo creates a branch 'feat' with 2 commits that both
// conflict when rebased onto main, because every commit rewrites f entirely.
func divergentRebaseRepo(t *testing.T) *testRepo {
	r := newTestRepo(t)
	r.commit(t, "base", "f", "base\n")

	r.must(t, "checkout", "-q", "-b", "feat")
	r.commit(t, "f1", "f", "feat-1\n")
	r.commit(t, "f2", "f", "feat-2\n")

	r.must(t, "checkout", "-q", "main")
	r.commit(t, "main-x", "f", "main-x\n")
	return r
}

func TestRebaseConflictProgressContinueDone(t *testing.T) {
	r := divergentRebaseRepo(t)
	r.must(t, "checkout", "-q", "feat")
	r.fail(t, "rebase", "main")

	op := r.collect(t)
	if op == nil {
		t.Fatal("expected operation in progress during rebase conflict")
	}
	if op.Kind != KindRebase || op.Branch != "feat" {
		t.Fatalf("got kind=%q branch=%q, want rebase/feat", op.Kind, op.Branch)
	}
	if op.Step != 1 || op.Total != 2 {
		t.Fatalf("got progress %d/%d, want 1/2", op.Step, op.Total)
	}
	if op.Reason != ReasonConflict || op.Conflicts != 1 {
		t.Fatalf("got reason=%q conflicts=%d, want conflict/1", op.Reason, op.Conflicts)
	}

	// Metadata missing: branch and progress become unknown, but the operation
	// itself stays visible.
	r.rmGitFile(t, "rebase-merge", "head-name")
	r.writeGitFile(t, "garbage\n", "rebase-merge", "msgnum")
	op = r.collect(t)
	if op.Branch != "" {
		t.Fatalf("got branch %q, want unknown", op.Branch)
	}
	if op.Step != Unknown || op.Total != Unknown {
		t.Fatalf("got progress %d/%d, want unknown", op.Step, op.Total)
	}
	if op.Reason != ReasonConflict {
		t.Fatalf("reason must still be evidenced by conflicts, got %q", op.Reason)
	}

	// Metadata restored: progress is readable again.
	r.writeGitFile(t, "refs/heads/feat\n", "rebase-merge", "head-name")
	r.writeGitFile(t, "1\n", "rebase-merge", "msgnum")
	r.writeGitFile(t, "2\n", "rebase-merge", "end")

	// Zero conflicts after resolving does NOT mean the operation is done:
	// changes are staged, waiting for --continue.
	r.write(t, "f", "resolved-1\n")
	r.must(t, "add", "f")
	op = r.collect(t)
	if op.Step != 1 || op.Total != 2 {
		t.Fatalf("got progress %d/%d, want 1/2", op.Step, op.Total)
	}
	if op.Reason != ReasonStaged {
		t.Fatalf("got reason %q, want staged (zero conflicts != complete)", op.Reason)
	}

	// Continue: first commit applied, second one conflicts, step advances.
	r.fail(t, "rebase", "--continue")
	op = r.collect(t)
	if op.Step != 2 || op.Total != 2 || op.Reason != ReasonConflict {
		t.Fatalf("got progress=%d/%d reason=%q, want 2/2 conflict", op.Step, op.Total, op.Reason)
	}

	// Continue to completion: metadata gone, back on the original branch.
	r.write(t, "f", "resolved-2\n")
	r.must(t, "add", "f")
	r.must(t, "rebase", "--continue")
	if op := r.collect(t); op != nil {
		t.Fatalf("expected no operation after successful rebase, got %+v", op)
	}
	if branch := r.must(t, "branch", "--show-current"); strings.TrimSpace(branch) != "feat" {
		t.Fatalf("got branch %q, want feat", branch)
	}
}

func TestRebaseAbort(t *testing.T) {
	r := divergentRebaseRepo(t)
	tipBefore := strings.TrimSpace(r.must(t, "rev-parse", "feat"))

	r.must(t, "checkout", "-q", "feat")
	r.fail(t, "rebase", "main")
	if op := r.collect(t); op == nil || op.Step != 1 || op.Total != 2 {
		t.Fatalf("want paused rebase 1/2, got %+v", op)
	}

	r.must(t, "rebase", "--abort")
	if op := r.collect(t); op != nil {
		t.Fatalf("want no operation after abort, got %+v", op)
	}
	if branch := strings.TrimSpace(r.must(t, "branch", "--show-current")); branch != "feat" {
		t.Fatalf("want branch feat after abort, got %q", branch)
	}
	if tipAfter := strings.TrimSpace(r.must(t, "rev-parse", "feat")); tipAfter != tipBefore {
		t.Fatalf("want feat at %s after abort, got %s", tipBefore, tipAfter)
	}
}

func TestRebaseEditStopZeroConflicts(t *testing.T) {
	r := newTestRepo(t)
	r.commit(t, "c1", "f", "1\n")
	r.commit(t, "c2", "f", "1\n2\n")
	r.commit(t, "c3", "f", "1\n2\n3\n")

	// Portable sequence editor: turn the first 'pick' into 'edit'.
	editor := filepath.Join(r.dir, "seq-editor.sh")
	r.write(t, "seq-editor.sh", "#!/bin/sh\n"+
		"sed '1s/^pick/edit/' \"$1\" > \"$1.tmp\" && mv \"$1.tmp\" \"$1\"\n")
	if err := os.Chmod(editor, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("git", "rebase", "-i", "HEAD~2")
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GIT_EDITOR=true", "GIT_SEQUENCE_EDITOR="+editor)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("interactive rebase edit stop: %v\n%s", err, out)
	}

	// Zero conflicts, yet the rebase must still be reported as in progress.
	op := r.collect(t)
	if op == nil {
		t.Fatal("expected paused rebase at edit stop")
	}
	if op.Kind != KindRebase || op.Branch != "main" || op.Step != 1 || op.Total != 2 {
		t.Fatalf("got %+v, want rebase main 1/2", op)
	}
	if op.Conflicts != 0 || op.Reason != ReasonEdit {
		t.Fatalf("got conflicts=%d reason=%q, want 0/edit", op.Conflicts, op.Reason)
	}

	r.must(t, "rebase", "--continue")
	if op := r.collect(t); op != nil {
		t.Fatalf("want no operation after continue, got %+v", op)
	}
}

// threeConflictingCommits creates side commits a/b/c each replacing f
// entirely, and a main commit doing the same: every pick/revert conflicts.
func threeConflictingCommits(t *testing.T) *testRepo {
	r := newTestRepo(t)
	r.commit(t, "c1", "f", "base\n")
	r.must(t, "checkout", "-q", "-b", "side")
	r.commit(t, "a", "f", "a\n")
	r.commit(t, "b", "f", "b\n")
	r.commit(t, "c", "f", "c\n")
	r.must(t, "checkout", "-q", "main")
	r.commit(t, "x", "f", "main-x\n")
	return r
}

func TestCherryPickMultiContinueAbort(t *testing.T) {
	r := threeConflictingCommits(t)
	tipBefore := strings.TrimSpace(r.must(t, "rev-parse", "HEAD"))

	r.fail(t, "cherry-pick", "side~2", "side~1", "side")
	op := r.collect(t)
	if op == nil || op.Kind != KindCherryPick || op.Branch != "main" {
		t.Fatalf("got %+v, want cherry-pick on main", op)
	}
	if op.Step != 1 || op.Total != 3 || op.Reason != ReasonConflict {
		t.Fatalf("got %d/%d %q, want 1/3 conflict", op.Step, op.Total, op.Reason)
	}

	// Continue -> stopped at step 2.
	r.write(t, "f", "resolved-a\n")
	r.must(t, "add", "f")
	r.fail(t, "cherry-pick", "--continue")
	op = r.collect(t)
	if op.Step != 2 || op.Total != 3 || op.Reason != ReasonConflict {
		t.Fatalf("got %d/%d %q, want 2/3 conflict", op.Step, op.Total, op.Reason)
	}

	// Metadata changed out-of-band (todo no longer matches CHERRY_PICK_HEAD):
	// progress becomes unknown but the operation stays visible.
	todoPath := r.gitDirFile("sequencer", "todo")
	original, err := os.ReadFile(todoPath)
	if err != nil {
		t.Fatal(err)
	}
	r.writeGitFile(t, "pick deadbeef b\n"+string(original), "sequencer", "todo")
	op = r.collect(t)
	if op.Step != Unknown || op.Total != Unknown {
		t.Fatalf("got %d/%d, want unknown progress after metadata change", op.Step, op.Total)
	}
	if op.Reason != ReasonConflict || op.Branch != "main" {
		t.Fatalf("got reason=%q branch=%q, want conflict/main", op.Reason, op.Branch)
	}
	if err := os.WriteFile(todoPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	// Abort: sequencer metadata gone, HEAD restored.
	r.must(t, "cherry-pick", "--abort")
	if op := r.collect(t); op != nil {
		t.Fatalf("want no operation after abort, got %+v", op)
	}
	if tip := strings.TrimSpace(r.must(t, "rev-parse", "HEAD")); tip != tipBefore {
		t.Fatalf("want HEAD %s after abort, got %s", tipBefore, tip)
	}
}

func TestCherryPickEmptyIsNotComplete(t *testing.T) {
	r := newTestRepo(t)
	r.commit(t, "c1", "f", "base\n")
	r.must(t, "checkout", "-q", "-b", "side")
	r.commit(t, "sx", "f", "X\n")
	side := strings.TrimSpace(r.must(t, "rev-parse", "side"))
	r.must(t, "checkout", "-q", "main")
	r.commit(t, "mx", "f", "X\n")

	// Picking a commit whose change is already present pauses with zero
	// conflicts ("previous cherry-pick is now empty").
	r.fail(t, "cherry-pick", side)

	t.Chdir(r.dir)
	st, err := gitstatus.NewWithContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.IsClean || st.NumConflicts != 0 {
		t.Fatalf("test setup: want clean tree with 0 conflicts, got clean=%v conflicts=%d", st.IsClean, st.NumConflicts)
	}
	op := Collect(context.Background(), st)
	if op == nil {
		t.Fatal("zero conflicts must not be interpreted as operation completion")
	}
	if op.Kind != KindCherryPick || op.Branch != "main" {
		t.Fatalf("got %+v, want cherry-pick on main", op)
	}
	if op.Step != Unknown || op.Total != Unknown {
		t.Fatalf("single-commit pick has unknown progress, got %d/%d", op.Step, op.Total)
	}
	if op.Reason != ReasonEmpty {
		t.Fatalf("got reason %q, want empty", op.Reason)
	}

	r.must(t, "cherry-pick", "--abort")
	if op := r.collect(t); op != nil {
		t.Fatalf("want no operation after abort, got %+v", op)
	}
}

func TestRevertMultiContinueAbortAndCleanDone(t *testing.T) {
	r := newTestRepo(t)
	r.commit(t, "base", "f", "base\n")
	r.commit(t, "a", "f", "a\n")
	r.commit(t, "b", "f", "b\n")
	r.commit(t, "c", "f", "c\n")
	r.commit(t, "x", "f", "main-x\n")
	c := strings.TrimSpace(r.must(t, "rev-parse", "HEAD~1"))
	b := strings.TrimSpace(r.must(t, "rev-parse", "HEAD~2"))
	a := strings.TrimSpace(r.must(t, "rev-parse", "HEAD~3"))
	tipBefore := strings.TrimSpace(r.must(t, "rev-parse", "HEAD"))

	r.fail(t, "revert", "--no-edit", c, b, a)
	op := r.collect(t)
	if op == nil || op.Kind != KindRevert || op.Branch != "main" {
		t.Fatalf("got %+v, want revert on main", op)
	}
	if op.Step != 1 || op.Total != 3 || op.Reason != ReasonConflict {
		t.Fatalf("got %d/%d %q, want 1/3 conflict", op.Step, op.Total, op.Reason)
	}

	r.write(t, "f", "resolved-c\n")
	r.must(t, "add", "f")
	r.fail(t, "revert", "--continue")
	op = r.collect(t)
	if op.Step != 2 || op.Total != 3 || op.Reason != ReasonConflict {
		t.Fatalf("got %d/%d %q, want 2/3 conflict", op.Step, op.Total, op.Reason)
	}

	r.must(t, "revert", "--abort")
	if op := r.collect(t); op != nil {
		t.Fatalf("want no operation after abort, got %+v", op)
	}
	if tip := strings.TrimSpace(r.must(t, "rev-parse", "HEAD")); tip != tipBefore {
		t.Fatalf("want HEAD restored after abort")
	}

	// A conflict-free multi-revert completes and leaves no metadata.
	r2 := newTestRepo(t)
	r2.commit(t, "base", "f", "1\n")
	r2.commit(t, "p", "g", "2\n")
	r2.commit(t, "q", "h", "3\n")
	r2.must(t, "revert", "--no-edit", "HEAD~1", "HEAD~2")
	if op := r2.collect(t); op != nil {
		t.Fatalf("want no operation after clean revert sequence, got %+v", op)
	}
}

func TestReasonEvidence(t *testing.T) {
	tests := []struct {
		name       string
		state      gitstatus.TreeState
		lastAction string
		st         gitstatus.Porcelain
		want       Reason
	}{
		{"conflict beats everything", gitstatus.Rebasing, "pick", gitstatus.Porcelain{NumConflicts: 2}, ReasonConflict},
		{"rebase edit stop, zero conflicts", gitstatus.Rebasing, "edit", gitstatus.Porcelain{}, ReasonEdit},
		{"rebase reword stop", gitstatus.Rebasing, "reword", gitstatus.Porcelain{}, ReasonReword},
		{"rebase break stop", gitstatus.Rebasing, "break", gitstatus.Porcelain{}, ReasonBreak},
		{"rebase empty pick, clean tree", gitstatus.Rebasing, "pick", gitstatus.Porcelain{}, ReasonEmpty},
		{"rebase failed exec is not an empty commit", gitstatus.Rebasing, "exec", gitstatus.Porcelain{}, ReasonUnknown},
		{"rebase missing done action stays unknown", gitstatus.Rebasing, "", gitstatus.Porcelain{}, ReasonUnknown},
		{"rebase resolved and staged, zero conflicts", gitstatus.Rebasing, "pick", gitstatus.Porcelain{NumStaged: 1}, ReasonStaged},
		{"rebase worktree-only modifications are unknown", gitstatus.Rebasing, "pick", gitstatus.Porcelain{NumModified: 1}, ReasonUnknown},
		{"cherry-pick empty, clean tree", gitstatus.CherryPicking, "", gitstatus.Porcelain{}, ReasonEmpty},
		{"cherry-pick staged resolution", gitstatus.CherryPicking, "", gitstatus.Porcelain{NumStaged: 1}, ReasonStaged},
		{"revert empty, clean tree", gitstatus.Reverting, "", gitstatus.Porcelain{}, ReasonEmpty},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &gitstatus.Status{State: tt.state, Porcelain: tt.st}
			if got := reason(st, tt.lastAction); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCollectNilOutsideOperations(t *testing.T) {
	r := newTestRepo(t)
	r.commit(t, "c1", "f", "base\n")
	if op := r.collect(t); op != nil {
		t.Fatalf("want nil on a plain branch, got %+v", op)
	}
}
