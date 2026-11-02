// Package operation collects information about in-progress multi-step Git
// operations (rebase, multi-commit cherry-pick and revert), using the on-disk
// metadata Git maintains for them (.git/rebase-merge, .git/rebase-apply and
// .git/sequencer).
//
// The information complements github.com/arl/gitstatus, which only reports the
// kind of special state a working tree is in, without any detail about the
// original branch, the step progress or the reason why the operation paused.
package operation

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/arl/gitstatus"
)

// Unknown is reported for a numeric value (step or total) when the metadata is
// missing, unreadable or inconsistent.
const Unknown = -1

// Kind is the kind of in-progress operation.
type Kind string

const (
	KindRebase     Kind = "rebase"
	KindCherryPick Kind = "cherry-pick"
	KindRevert     Kind = "revert"
)

// Reason is the evidenced reason why an operation is paused.
type Reason string

const (
	// ReasonConflict: porcelain reports unmerged entries.
	ReasonConflict Reason = "conflict"
	// ReasonEdit: the last executed rebase command is an 'edit' command.
	ReasonEdit Reason = "edit"
	// ReasonReword: the last executed rebase command is a 'reword' command.
	ReasonReword Reason = "reword"
	// ReasonBreak: the last executed rebase command is a 'break' command.
	ReasonBreak Reason = "break"
	// ReasonStaged: conflicts were cleared but changes are staged, waiting
	// for the user to continue the operation.
	ReasonStaged Reason = "staged"
	// ReasonEmpty: the commit being applied results in an empty tree,
	// waiting for the user to continue or skip.
	ReasonEmpty Reason = "empty"
	// ReasonUnknown: metadata exists (operation in progress) but no
	// positive evidence explains why it is paused. In particular, a zero
	// conflict count never means the operation is complete.
	ReasonUnknown Reason = "unknown"
)

// Info describes a paused multi-step Git operation.
//
// A zero/negative Step or Total (Unknown) means the progress could not be
// determined; an empty Branch means the original branch could not be
// determined. The presentation layer renders these as "unknown".
type Info struct {
	// Kind is the kind of operation.
	Kind Kind

	// Branch is the branch the operation is running on: the original branch
	// for a rebase (read from head-name), the current (attached) branch for a
	// cherry-pick or revert. Empty when unknown.
	Branch string

	// Step is the current step (1-based), Unknown when it can't be determined.
	Step int

	// Total is the total number of steps, Unknown when it can't be determined.
	Total int

	// Reason is the evidenced reason why the operation is paused.
	Reason Reason

	// Conflicts is the count of unmerged entries, meaningful (and possibly
	// zero) for every paused operation.
	Conflicts int
}

// Collect inspects the current working tree and returns information about the
// in-progress operation, or nil if st does not represent a rebase,
// cherry-pick or revert in progress.
//
// Unreadable or inconsistent metadata never causes an error: the
// corresponding Info fields are set to unknown instead, so the status bar
// keeps reporting that an operation is in progress.
func Collect(ctx context.Context, st *gitstatus.Status) *Info {
	if st == nil || st.IsInitial {
		return nil
	}

	switch st.State {
	case gitstatus.Rebasing:
		return collectRebase(ctx, st)
	case gitstatus.CherryPicking:
		return collectSequencer(ctx, st, KindCherryPick, "CHERRY_PICK_HEAD")
	case gitstatus.Reverting:
		return collectSequencer(ctx, st, KindRevert, "REVERT_HEAD")
	default:
		return nil
	}
}

// gitDir returns the absolute path of the current repository git directory.
func gitDir(ctx context.Context) (string, bool) {
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--git-dir").Output()
	if err != nil {
		return "", false
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", false
	}
	if !filepath.IsAbs(dir) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", false
		}
		dir = filepath.Join(cwd, dir)
	}
	return dir, true
}

// readFile reads and trims a file under gitdir, returning false when missing
// or unreadable.
func readFile(gitdir string, elems ...string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(append([]string{gitdir}, elems...)...))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// parseCount parses a positive step/total counter file.
func parseCount(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return Unknown, false
	}
	return n, true
}

// branchFromHeadName converts a 'refs/heads/<name>' head-name content into a
// branch name. Anything else is reported as unknown.
func branchFromHeadName(headName string) string {
	const prefix = "refs/heads/"
	if !strings.HasPrefix(headName, prefix) {
		return ""
	}
	name := strings.TrimPrefix(headName, prefix)
	if name == "" {
		return ""
	}
	return name
}

// collectRebase handles both the merge backend (.git/rebase-merge, used by
// modern Git for every rebase) and the legacy apply backend (.git/rebase-apply).
func collectRebase(ctx context.Context, st *gitstatus.Status) *Info {
	gitdir, ok := gitDir(ctx)
	if !ok {
		return &Info{Kind: KindRebase, Step: Unknown, Total: Unknown, Reason: reason(st, ""), Conflicts: st.NumConflicts}
	}

	info := &Info{Kind: KindRebase, Step: Unknown, Total: Unknown, Conflicts: st.NumConflicts}

	var done []string
	switch {
	case dirExists(filepath.Join(gitdir, "rebase-merge")):
		if headName, ok := readFile(gitdir, "rebase-merge", "head-name"); ok {
			info.Branch = branchFromHeadName(headName)
		}

		// 'msgnum' is the modern name, older Git versions used 'msgnr'.
		curStr, curOK := readFile(gitdir, "rebase-merge", "msgnum")
		if !curOK {
			curStr, curOK = readFile(gitdir, "rebase-merge", "msgnr")
		}
		endStr, endOK := readFile(gitdir, "rebase-merge", "end")
		if curOK && endOK {
			cur, curOK2 := parseCount(curStr)
			end, endOK2 := parseCount(endStr)
			// Metadata changed or is corrupt: report unknown progress.
			if curOK2 && endOK2 && end >= 1 && cur >= 1 && cur <= end {
				info.Step, info.Total = cur, end
			}
		}

		if content, ok := readFile(gitdir, "rebase-merge", "done"); ok {
			done = splitLines(content)
		}

	case dirExists(filepath.Join(gitdir, "rebase-apply")):
		if headName, ok := readFile(gitdir, "rebase-apply", "head-name"); ok {
			info.Branch = branchFromHeadName(headName)
		}

		nextStr, nextOK := readFile(gitdir, "rebase-apply", "next")
		lastStr, lastOK := readFile(gitdir, "rebase-apply", "last")
		if nextOK && lastOK {
			next, nextOK2 := parseCount(nextStr)
			last, lastOK2 := parseCount(lastStr)
			if nextOK2 && lastOK2 && last >= 1 && next >= 1 && next <= last {
				info.Step, info.Total = next, last
			}
		}
	}

	info.Reason = reason(st, lastCommandAction(done))
	return info
}

// collectSequencer handles multi/single cherry-pick and revert. Multiple
// commits are tracked by Git in .git/sequencer (todo + head files); a single
// commit operation only has CHERRY_PICK_HEAD/REVERT_HEAD and no progress
// metadata.
func collectSequencer(ctx context.Context, st *gitstatus.Status, kind Kind, headFile string) *Info {
	gitdir, _ := gitDir(ctx)

	info := &Info{
		Kind:      kind,
		Step:      Unknown,
		Total:     Unknown,
		Reason:    reason(st, ""),
		Conflicts: st.NumConflicts,
	}

	// Unlike a rebase, cherry-pick and revert keep HEAD attached to the
	// current branch.
	if !st.IsDetached {
		info.Branch = st.LocalBranch
	}

	if gitdir == "" {
		return info
	}

	curSHA, ok := readFile(gitdir, headFile)
	if !ok {
		// State detection and metadata disagree (metadata changed/removed):
		// stay in unknown-progress mode.
		return info
	}

	todoContent, ok := readFile(gitdir, "sequencer", "todo")
	if !ok {
		// Single-commit operation: no sequence, no progress available.
		return info
	}

	todo := parseTodo(todoContent)
	if len(todo) == 0 {
		return info
	}

	origHead, ok := readFile(gitdir, "sequencer", "head")
	if !ok {
		return info
	}

	// The first todo entry must be the commit the operation is currently
	// stopped on (its SHA matches CHERRY_PICK_HEAD/REVERT_HEAD; todo entries
	// may use an abbreviated SHA). If not, the metadata was modified
	// out-of-band: report unknown progress.
	if !shaMatches(todo[0].sha, curSHA) {
		return info
	}

	completed, ok := countCommits(ctx, origHead)
	if !ok {
		return info
	}
	if completed < 0 || completed > len(todo) {
		// completed > len(todo) can't happen for an unchanged sequence.
		return info
	}

	info.Step = completed + 1
	info.Total = completed + len(todo)
	return info
}

type todoEntry struct {
	action string
	sha    string
}

// parseTodo parses the non-comment instruction lines of a sequencer todo file.
// Only 'pick' (cherry-pick) and 'revert' instructions are part of the step
// sequence.
func parseTodo(content string) []todoEntry {
	var entries []todoEntry
	for _, line := range splitLines(content) {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "pick", "p", "revert":
			entries = append(entries, todoEntry{action: f[0], sha: f[1]})
		}
	}
	return entries
}

// shaMatches reports whether two object names refer to the same SHA, allowing
// one of them to be an abbreviated form (prefix) as written in sequencer/todo.
func shaMatches(short, full string) bool {
	switch {
	case short == full:
		return true
	case len(short) >= 7 && strings.HasPrefix(full, short):
		return true
	case len(full) >= 7 && strings.HasPrefix(short, full):
		return true
	}
	return false
}

// countCommits returns `git rev-list --count <head>..HEAD`, i.e. the number of
// commits already applied on top of the operation's original HEAD.
func countCommits(ctx context.Context, head string) (int, bool) {
	out, err := exec.CommandContext(ctx, "git", "rev-list", "--count", head+"..HEAD").Output()
	if err != nil {
		return Unknown, false
	}
	return parseCount(string(out))
}

// reason determines, from positive evidence only, why an operation is paused.
// lastAction is the action of the last executed rebase todo command ("" for a
// cherry-pick/revert or when the 'done' file can't be read).
func reason(st *gitstatus.Status, lastAction string) Reason {
	if st.NumConflicts > 0 {
		return ReasonConflict
	}

	if st.State == gitstatus.Rebasing {
		switch lastAction {
		case "edit", "e":
			return ReasonEdit
		case "reword", "r":
			return ReasonReword
		case "break":
			return ReasonBreak
		}
	}

	// No unmerged entries: staged changes mean conflicts were resolved and
	// the operation is waiting to be continued.
	if st.NumStaged > 0 {
		return ReasonStaged
	}

	// A completely clean index and working tree only proves the applied
	// commit became empty for a picking command (or for cherry-pick/revert,
	// whose todo lists have no edit/exec commands). Any other rebase command
	// (e.g. a failed 'exec') and missing 'done' metadata stay unknown.
	if st.NumModified == 0 {
		if st.State != gitstatus.Rebasing {
			return ReasonEmpty
		}
		switch lastAction {
		case "pick", "p", "revert":
			return ReasonEmpty
		}
	}

	return ReasonUnknown
}

// lastCommandAction returns the action keyword of the last non-comment rebase
// 'done' line (e.g. pick, edit, break), or "" when there is none.
func lastCommandAction(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		f := strings.Fields(lines[i])
		if len(f) == 0 {
			continue
		}
		return f[0]
	}
	return ""
}

func splitLines(s string) []string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
