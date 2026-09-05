package workspace

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestBuildPlanIsDeterministicAndOwnsContent(t *testing.T) {
	root := t.TempDir()
	writeMode(t, filepath.Join(root, "update.txt"), []byte("before"), 0o600)
	writeMode(t, filepath.Join(root, "delete.txt"), []byte("delete"), 0o640)
	created := []byte("created")
	updated := []byte("updated")
	changes := []Change{
		{Path: "update.txt", Operation: OperationUpdate, Content: updated, Mode: 0o644},
		{Path: "nested/create.txt", Operation: OperationCreate, Content: created, Mode: 0o640},
		{Path: "delete.txt", Operation: OperationDelete},
	}
	first, err := BuildPlan(root, changes)
	if err != nil {
		t.Fatal(err)
	}
	reversed := slices.Clone(changes)
	slices.Reverse(reversed)
	second, err := BuildPlan(root, reversed)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest() != second.Digest() ||
		first.SnapshotDigest() != second.SnapshotDigest() ||
		first.FinalDigest() != second.FinalDigest() ||
		!reflect.DeepEqual(first.Changes(), second.Changes()) {
		t.Fatalf("plans differ by input order:\n%#v\n%#v", first.Changes(), second.Changes())
	}
	if got := []string{first.Changes()[0].Path, first.Changes()[1].Path, first.Changes()[2].Path}; !reflect.DeepEqual(got, []string{"delete.txt", "nested/create.txt", "update.txt"}) {
		t.Fatalf("canonical paths = %v", got)
	}

	created[0] = 'X'
	updated[0] = 'Y'
	result, err := Apply(context.Background(), root, first)
	if err != nil || result.Disposition != ApplyCommitted {
		t.Fatalf("Apply() = (%#v, %v)", result, err)
	}
	if got := readFile(t, filepath.Join(root, "nested/create.txt")); got != "created" {
		t.Fatalf("created content = %q", got)
	}
	if got := readFile(t, filepath.Join(root, "update.txt")); got != "updated" {
		t.Fatalf("updated content = %q", got)
	}
}

func TestPlanDigestBindsPreconditionContentModeAndOperation(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "target"), "before")
	base, err := BuildPlan(root, []Change{{Path: "target", Operation: OperationUpdate, Content: []byte("after"), Mode: 0o600}})
	if err != nil {
		t.Fatal(err)
	}
	contentChanged, err := BuildPlan(root, []Change{{Path: "target", Operation: OperationUpdate, Content: []byte("other"), Mode: 0o600}})
	if err != nil {
		t.Fatal(err)
	}
	modeChanged, err := BuildPlan(root, []Change{{Path: "target", Operation: OperationUpdate, Content: []byte("after"), Mode: 0o640}})
	if err != nil {
		t.Fatal(err)
	}
	if base.Digest() == contentChanged.Digest() || base.Digest() == modeChanged.Digest() {
		t.Fatal("plan digest did not bind desired content and mode")
	}
	if base.SnapshotDigest() != contentChanged.SnapshotDigest() || base.SnapshotDigest() != modeChanged.SnapshotDigest() {
		t.Fatal("identical preconditions produced different snapshot digests")
	}

	writeFile(t, filepath.Join(root, "target"), "new-before")
	preconditionChanged, err := BuildPlan(root, []Change{{Path: "target", Operation: OperationUpdate, Content: []byte("after"), Mode: 0o600}})
	if err != nil {
		t.Fatal(err)
	}
	if base.SnapshotDigest() == preconditionChanged.SnapshotDigest() || base.Digest() == preconditionChanged.Digest() {
		t.Fatal("plan digest did not bind the existing snapshot")
	}

	returned := base.Changes()
	returned[0].Path = "changed"
	if base.Changes()[0].Path != "target" {
		t.Fatal("plan mutated through Changes result")
	}
}

func TestBuildPlanRejectsInvalidConflictingAndContradictoryChanges(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "exists"), "value")
	for name, changes := range map[string][]Change{
		"empty":        nil,
		"unnormalized": {{Path: "a/../b", Operation: OperationCreate, Mode: 0o600}},
		"duplicate": {
			{Path: "a", Operation: OperationCreate, Mode: 0o600},
			{Path: "a", Operation: OperationCreate, Mode: 0o600},
		},
		"parent-child-nonadjacent": {
			{Path: "a", Operation: OperationCreate, Mode: 0o600},
			{Path: "a-b", Operation: OperationCreate, Mode: 0o600},
			{Path: "a/b", Operation: OperationCreate, Mode: 0o600},
		},
		"create-existing": {{Path: "exists", Operation: OperationCreate, Mode: 0o600}},
		"update-missing":  {{Path: "missing", Operation: OperationUpdate, Mode: 0o600}},
		"delete-missing":  {{Path: "missing", Operation: OperationDelete}},
		"delete-content":  {{Path: "exists", Operation: OperationDelete, Content: []byte{}}},
		"assert-content":  {{Path: "exists", Operation: OperationAssert, Content: []byte{}}},
		"mode-type-bits":  {{Path: "missing", Operation: OperationCreate, Mode: fs.ModeDir | 0o700}},
		"unreadable-mode": {{Path: "missing", Operation: OperationCreate, Mode: 0o200}},
		"reserved-state":  {{Path: ".appkit-workspace-txn-user/value", Operation: OperationCreate, Mode: 0o600}},
		"unknown":         {{Path: "missing", Operation: "replace", Mode: 0o600}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := BuildPlan(root, changes); !errors.Is(err, ErrInvalidChange) {
				t.Fatalf("BuildPlan() error = %v, want ErrInvalidChange", err)
			}
		})
	}
}

func TestApplyCreateUpdateDeleteModeAndReplay(t *testing.T) {
	root := t.TempDir()
	writeMode(t, filepath.Join(root, "update.txt"), []byte("before"), 0o600)
	writeMode(t, filepath.Join(root, "delete.txt"), []byte("delete"), 0o640)
	plan, err := BuildPlan(root, []Change{
		{Path: "nested/create.txt", Operation: OperationCreate, Content: []byte("created"), Mode: 0o640},
		{Path: "update.txt", Operation: OperationUpdate, Content: []byte("after"), Mode: 0o644},
		{Path: "delete.txt", Operation: OperationDelete},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := Apply(context.Background(), root, plan)
	if err != nil || result.PlanDigest != plan.Digest() || result.Disposition != ApplyCommitted {
		t.Fatalf("Apply() = (%#v, %v)", result, err)
	}
	assertContentMode(t, filepath.Join(root, "nested/create.txt"), "created", 0o640)
	assertContentMode(t, filepath.Join(root, "update.txt"), "after", 0o644)
	if _, err := os.Lstat(filepath.Join(root, "delete.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deleted file Lstat() = %v", err)
	}
	assertNoTransactions(t, root)

	replayed, err := Apply(context.Background(), root, plan)
	if err != nil || replayed.Disposition != ApplyReplayed {
		t.Fatalf("Apply(replay) = (%#v, %v)", replayed, err)
	}
	assertNoTransactions(t, root)
}

func TestCommitGateRejectsBeforePublicMutationAndReturnsReceipt(t *testing.T) {
	root := t.TempDir()
	plan, err := BuildPlan(root, []Change{{
		Path: "target", Operation: OperationCreate, Content: []byte("value"), Mode: 0o600,
	}})
	if err != nil {
		t.Fatal(err)
	}
	receipt := digestBytes([]byte("admission receipt"))
	denied := errors.New("authorization expired")
	for _, test := range []struct {
		name        string
		admit       func(context.Context) (string, error)
		recheck     func(context.Context, string) error
		wantAdmits  int
		wantChecks  int
		wantReceipt string
	}{
		{
			name: "admit rejected",
			admit: func(context.Context) (string, error) {
				return "", denied
			},
			recheck:    func(context.Context, string) error { return nil },
			wantAdmits: 1,
		},
		{
			name: "recheck rejected",
			admit: func(context.Context) (string, error) {
				return receipt, nil
			},
			recheck:    func(context.Context, string) error { return denied },
			wantAdmits: 1, wantChecks: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			admitCalls, recheckCalls := 0, 0
			gate := CommitGate{
				Domain: "appkit.test.commit-gate.v1", SubjectDigest: digestBytes([]byte(test.name)),
				Admit: func(ctx context.Context) (string, error) {
					admitCalls++
					return test.admit(ctx)
				},
				Recheck: func(ctx context.Context, actual string) error {
					recheckCalls++
					return test.recheck(ctx, actual)
				},
			}
			result, err := ApplyWithCommitGate(context.Background(), root, plan, gate)
			if !errors.Is(err, ErrCommitAdmission) || !errors.Is(err, denied) || result.Disposition != "" ||
				admitCalls != test.wantAdmits || recheckCalls != test.wantChecks {
				t.Fatalf("ApplyWithCommitGate() = (%#v, %v), calls %d/%d", result, err, admitCalls, recheckCalls)
			}
			if _, err := os.Lstat(filepath.Join(root, "target")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("rejected gate exposed target: %v", err)
			}
			assertNoTransactions(t, root)
		})
	}

	admitCalls, recheckCalls := 0, 0
	gate := CommitGate{
		Domain: "appkit.test.commit-gate.v1", SubjectDigest: digestBytes([]byte("success")),
		Admit: func(context.Context) (string, error) {
			admitCalls++
			return receipt, nil
		},
		Recheck: func(_ context.Context, actual string) error {
			recheckCalls++
			if actual != receipt {
				return errors.New("wrong receipt")
			}
			return nil
		},
	}
	result, err := ApplyWithCommitGate(context.Background(), root, plan, gate)
	if err != nil || result.Disposition != ApplyCommitted || result.AdmissionReceiptDigest != receipt ||
		admitCalls != 1 || recheckCalls != 1 {
		t.Fatalf("ApplyWithCommitGate() = (%#v, %v), calls %d/%d", result, err, admitCalls, recheckCalls)
	}
	replayed, err := ApplyWithCommitGate(context.Background(), root, plan, gate)
	if err != nil || replayed.Disposition != ApplyReplayed || admitCalls != 1 || recheckCalls != 1 {
		t.Fatalf("replay = (%#v, %v), calls %d/%d", replayed, err, admitCalls, recheckCalls)
	}
}

func TestCommitGateInspectRunsAfterRecoveryAndBeforeStateClassification(t *testing.T) {
	root := t.TempDir()
	plan, err := BuildPlan(root, []Change{{
		Path: "target", Operation: OperationCreate, Content: []byte("value"), Mode: 0o600,
	}})
	if err != nil {
		t.Fatal(err)
	}
	denied := errors.New("semantic endpoint mismatch")
	inspectCalls, admitCalls := 0, 0
	gate := CommitGate{
		Domain: "appkit.test.inspect.v1", SubjectDigest: plan.Digest(),
		Inspect: func(context.Context) error {
			inspectCalls++
			return denied
		},
		Admit: func(context.Context) (string, error) {
			admitCalls++
			return digestBytes([]byte("unused")), nil
		},
		Recheck: func(context.Context, string) error { return nil },
	}
	result, err := ApplyWithCommitGate(context.Background(), root, plan, gate)
	if !errors.Is(err, ErrCommitAdmission) || !errors.Is(err, denied) ||
		result.Disposition != "" || inspectCalls != 1 || admitCalls != 0 {
		t.Fatalf("ApplyWithCommitGate() = (%#v, %v), inspect/admit %d/%d", result, err, inspectCalls, admitCalls)
	}
	if _, err := os.Lstat(filepath.Join(root, "target")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("inspect rejection exposed target: %v", err)
	}
	assertNoTransactions(t, root)
}

func TestCommitGateCrashRecoveryDoesNotRerunAdmission(t *testing.T) {
	root := t.TempDir()
	plan, err := BuildPlan(root, []Change{
		{Path: "a", Operation: OperationCreate, Content: []byte("new-a"), Mode: 0o600},
		{Path: "b", Operation: OperationCreate, Content: []byte("new-b"), Mode: 0o600},
	})
	if err != nil {
		t.Fatal(err)
	}
	subject := digestBytes([]byte("higher-level apply plan"))
	receipt := digestBytes([]byte("persisted authorization"))
	txn := transactionDirectoryName(plan.Digest(), "00000000000000000000000000000000")
	if err := os.Mkdir(filepath.Join(root, txn), 0o700); err != nil {
		t.Fatal(err)
	}
	rooted, err := openWorkspaceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareTransaction(rooted, txn, plan, transactionAdmission{
		Domain: "appkit.test.upgrade.v1", SubjectDigest: subject, ReceiptDigest: receipt,
	}); err != nil {
		_ = rooted.Close()
		t.Fatal(err)
	}
	if err := rooted.Close(); err != nil {
		t.Fatal(err)
	}
	writeMode(t, filepath.Join(root, "a"), []byte("new-a"), 0o600)

	inspectCalls, admitCalls, recheckCalls := 0, 0, 0
	gate := CommitGate{
		Domain: "appkit.test.upgrade.v1", SubjectDigest: subject,
		Inspect: func(context.Context) error {
			inspectCalls++
			return errors.New("must not inspect recovered transaction")
		},
		Admit: func(context.Context) (string, error) {
			admitCalls++
			return "", errors.New("expired evidence must not block recovery")
		},
		Recheck: func(context.Context, string) error {
			recheckCalls++
			return errors.New("must not recheck recovered receipt")
		},
	}
	result, err := ApplyWithCommitGate(context.Background(), root, plan, gate)
	if !errors.Is(err, ErrRecoveryRestart) || result.Disposition != "" ||
		inspectCalls != 0 || admitCalls != 0 || recheckCalls != 0 {
		t.Fatalf("recovery = (%#v, %v), calls %d/%d/%d", result, err, inspectCalls, admitCalls, recheckCalls)
	}
	for _, target := range []string{"a", "b"} {
		if _, err := os.Lstat(filepath.Join(root, target)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("recovery did not restore missing %s: %v", target, err)
		}
	}
	assertNoTransactions(t, root)

	gate.Admit = func(context.Context) (string, error) {
		admitCalls++
		return receipt, nil
	}
	gate.Recheck = func(context.Context, string) error {
		recheckCalls++
		return nil
	}
	gate.Inspect = func(context.Context) error {
		inspectCalls++
		return nil
	}
	result, err = ApplyWithCommitGate(context.Background(), root, plan, gate)
	if err != nil || result.Disposition != ApplyCommitted || result.AdmissionReceiptDigest != receipt ||
		inspectCalls != 1 || admitCalls != 1 || recheckCalls != 1 {
		t.Fatalf("fresh retry = (%#v, %v), calls %d/%d/%d", result, err, inspectCalls, admitCalls, recheckCalls)
	}
}

func TestApplyAssertBindsStateWithoutWritingIt(t *testing.T) {
	root := t.TempDir()
	writeMode(t, filepath.Join(root, "guard"), []byte("locked-input"), 0o640)
	plan, err := BuildPlan(root, []Change{
		{Path: "guard", Operation: OperationAssert},
		{Path: "output", Operation: OperationCreate, Content: []byte("generated"), Mode: 0o644},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeMode(t, filepath.Join(root, "guard"), []byte("changed"), 0o640)
	if _, err := Apply(context.Background(), root, plan); !errors.Is(err, ErrChanged) {
		t.Fatalf("Apply(changed assert) error = %v, want ErrChanged", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "output")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale assert allowed output mutation: %v", err)
	}

	writeMode(t, filepath.Join(root, "guard"), []byte("locked-input"), 0o640)
	result, err := Apply(context.Background(), root, plan)
	if err != nil || result.Disposition != ApplyCommitted {
		t.Fatalf("Apply = (%#v, %v)", result, err)
	}
	assertContentMode(t, filepath.Join(root, "guard"), "locked-input", 0o640)
	replayed, err := Apply(context.Background(), root, plan)
	if err != nil || replayed.Disposition != ApplyReplayed {
		t.Fatalf("Apply(replay) = (%#v, %v)", replayed, err)
	}
}

func TestApplyRejectsStaleAndMixedPreFinalState(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "update.txt"), "before")
	writeFile(t, filepath.Join(root, "delete.txt"), "delete")
	plan, err := BuildPlan(root, []Change{
		{Path: "update.txt", Operation: OperationUpdate, Content: []byte("after"), Mode: 0o600},
		{Path: "delete.txt", Operation: OperationDelete},
	})
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(root, "update.txt"), "stale")
	if _, err := Apply(context.Background(), root, plan); !errors.Is(err, ErrChanged) {
		t.Fatalf("Apply(stale) error = %v, want ErrChanged", err)
	}
	if got := readFile(t, filepath.Join(root, "update.txt")); got != "stale" {
		t.Fatalf("stale target was overwritten: %q", got)
	}

	writeFile(t, filepath.Join(root, "update.txt"), "before")
	if err := os.Remove(filepath.Join(root, "delete.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), root, plan); !errors.Is(err, ErrChanged) {
		t.Fatalf("Apply(mixed) error = %v, want ErrChanged", err)
	}
	if got := readFile(t, filepath.Join(root, "update.txt")); got != "before" {
		t.Fatalf("mixed target was overwritten: %q", got)
	}
	assertNoTransactions(t, root)
}

func TestApplyIgnoresFilesOutsideExplicitTargetSet(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "target"), "before")
	writeFile(t, filepath.Join(root, "unrelated"), "one")
	plan, err := BuildPlan(root, []Change{{Path: "target", Operation: OperationUpdate, Content: []byte("after"), Mode: 0o600}})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "unrelated"), "two")
	if result, err := Apply(context.Background(), root, plan); err != nil || result.Disposition != ApplyCommitted {
		t.Fatalf("Apply() = (%#v, %v)", result, err)
	}
	if got := readFile(t, filepath.Join(root, "unrelated")); got != "two" {
		t.Fatalf("unrelated file = %q", got)
	}
}

func TestApplyRejectsSymlinkReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require elevated permissions")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target")
	writeFile(t, target, "before")
	plan, err := BuildPlan(root, []Change{{
		Path: "target", Operation: OperationUpdate, Content: []byte("after"), Mode: 0o600,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	writeFile(t, outside, "outside")
	if err := os.Symlink(outside, target); err != nil {
		t.Fatal(err)
	}

	if _, err := Apply(context.Background(), root, plan); !errors.Is(err, ErrChanged) || !errors.Is(err, ErrSymlink) {
		t.Fatalf("Apply(symlink) error = %v, want ErrChanged and ErrSymlink", err)
	}
	if got := readFile(t, outside); got != "outside" {
		t.Fatalf("outside file changed: %q", got)
	}
}

func TestConcurrentCompetingPlansOnlyOneCommits(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "target"), "before")
	left, err := BuildPlan(root, []Change{{Path: "target", Operation: OperationUpdate, Content: []byte("left"), Mode: 0o600}})
	if err != nil {
		t.Fatal(err)
	}
	right, err := BuildPlan(root, []Change{{Path: "target", Operation: OperationUpdate, Content: []byte("right"), Mode: 0o600}})
	if err != nil {
		t.Fatal(err)
	}

	type response struct {
		result ApplyResult
		err    error
	}
	start := make(chan struct{})
	responses := make(chan response, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, plan := range []Plan{left, right} {
		go func(plan Plan) {
			ready.Done()
			<-start
			result, applyErr := Apply(context.Background(), root, plan)
			responses <- response{result: result, err: applyErr}
		}(plan)
	}
	ready.Wait()
	close(start)
	first, second := <-responses, <-responses
	values := []response{first, second}
	committed, stale := 0, 0
	for _, value := range values {
		switch {
		case value.err == nil && value.result.Disposition == ApplyCommitted:
			committed++
		case errors.Is(value.err, ErrChanged):
			stale++
		default:
			t.Fatalf("unexpected concurrent result (%#v, %v)", value.result, value.err)
		}
	}
	if committed != 1 || stale != 1 {
		t.Fatalf("committed=%d stale=%d", committed, stale)
	}
	if got := readFile(t, filepath.Join(root, "target")); got != "left" && got != "right" {
		t.Fatalf("final target = %q", got)
	}
	assertNoTransactions(t, root)
}

func TestApplyLockSerializesIndependentProcesses(t *testing.T) {
	if !hostProcessLockSupported {
		t.Skip("host process lock is unsupported")
	}
	root := t.TempDir()
	rooted, err := openWorkspaceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := acquireApplyLock(context.Background(), rooted)
	if err != nil {
		_ = rooted.Close()
		t.Fatal(err)
	}

	blocked := workspaceLockHelperCommand(t, root, "timeout")
	blockedOutput, blockedErr := blocked.CombinedOutput()
	if blockedErr != nil {
		_ = unlock()
		_ = rooted.Close()
		t.Fatalf("blocked helper failed: %v\n%s", blockedErr, blockedOutput)
	}
	if err := unlock(); err != nil {
		_ = rooted.Close()
		t.Fatal(err)
	}
	if err := rooted.Close(); err != nil {
		t.Fatal(err)
	}

	acquired := workspaceLockHelperCommand(t, root, "acquire")
	acquiredOutput, acquiredErr := acquired.CombinedOutput()
	if acquiredErr != nil {
		t.Fatalf("acquiring helper failed: %v\n%s", acquiredErr, acquiredOutput)
	}
}

func TestApplyRollsBackIfWorkspacePathIsReboundDuringCommit(t *testing.T) {
	if !hostProcessLockSupported {
		t.Skip("host process lock is unsupported")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "target"), "before")
	plan, err := BuildPlan(root, []Change{{
		Path: "target", Operation: OperationUpdate, Content: []byte("after"), Mode: 0o600,
	}})
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "moved")
	rebound := false
	_, err = apply(context.Background(), root, plan, applyOptions{inject: func(point applyFaultPoint, target string) error {
		if point != faultAfterOperation || target != "target" || rebound {
			return nil
		}
		rebound = true
		if err := os.Rename(root, moved); err != nil {
			return err
		}
		if err := os.Mkdir(root, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(root, "replacement"), []byte("untouched"), 0o600)
	}})
	if !errors.Is(err, ErrChanged) || errors.Is(err, ErrRollback) {
		t.Fatalf("Apply(rebound root) error = %v, want ErrChanged without rollback failure", err)
	}
	if !rebound {
		t.Fatal("fault did not rebind workspace path")
	}
	if got := readFile(t, filepath.Join(moved, "target")); got != "before" {
		t.Fatalf("moved workspace was not rolled back: %q", got)
	}
	if got := readFile(t, filepath.Join(root, "replacement")); got != "untouched" {
		t.Fatalf("replacement workspace was modified: %q", got)
	}
	if _, statErr := os.Lstat(filepath.Join(root, "target")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("replacement workspace gained target: %v", statErr)
	}
	assertNoTransactions(t, moved)
}

func TestWorkspaceLockHelperProcess(t *testing.T) {
	mode := os.Getenv("APPKIT_WORKSPACE_LOCK_HELPER")
	if mode == "" {
		return
	}
	rooted, err := openWorkspaceRoot(os.Getenv("APPKIT_WORKSPACE_LOCK_ROOT"))
	if err != nil {
		t.Fatal(err)
	}
	defer rooted.Close()

	ctx := context.Background()
	if mode == "timeout" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 150*time.Millisecond)
		defer cancel()
	}
	unlock, err := acquireApplyLock(ctx, rooted)
	if mode == "timeout" {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("blocked lock error = %v, want deadline", err)
		}
		return
	}
	if mode != "acquire" {
		t.Fatalf("unknown helper mode %q", mode)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
}

func workspaceLockHelperCommand(t *testing.T, root, mode string) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestWorkspaceLockHelperProcess$")
	command.Env = append(os.Environ(),
		"APPKIT_WORKSPACE_LOCK_HELPER="+mode,
		"APPKIT_WORKSPACE_LOCK_ROOT="+root,
	)
	return command
}

func TestApplyRecoversDurableTransactionsAfterProcessTermination(t *testing.T) {
	if !hostProcessLockSupported {
		t.Skip("host process lock is unsupported")
	}
	for _, test := range []struct {
		name        string
		point       applyFaultPoint
		target      string
		disposition ApplyDisposition
	}{
		{name: "after backup", point: faultAfterBackup, target: "a", disposition: ApplyCommitted},
		{name: "after install", point: faultAfterInstall, target: "a", disposition: ApplyCommitted},
		{name: "between operations", point: faultAfterOperation, target: "b", disposition: ApplyCommitted},
		{name: "all targets installed", point: faultAfterOperation, target: "nested/c", disposition: ApplyReplayed},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeMode(t, filepath.Join(root, "a"), []byte("old-a"), 0o600)
			writeMode(t, filepath.Join(root, "b"), []byte("old-b"), 0o640)
			plan := crashTestPlan(t, root)

			command := workspaceCrashHelperCommand(t, root, test.point, test.target)
			output, err := command.CombinedOutput()
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != 87 {
				t.Fatalf("crash helper = %v, output=%s", err, output)
			}
			transactions, err := filepath.Glob(filepath.Join(root, transactionPrefix+"*"))
			if err != nil || len(transactions) != 1 {
				t.Fatalf("active transactions after crash = %v, error %v", transactions, err)
			}

			result, err := Apply(context.Background(), root, plan)
			if err != nil || result.Disposition != test.disposition {
				t.Fatalf("Apply(recover) = (%#v, %v), want %s", result, err, test.disposition)
			}
			assertContentMode(t, filepath.Join(root, "a"), "new-a", 0o644)
			if _, err := os.Lstat(filepath.Join(root, "b")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("deleted b after recovery: %v", err)
			}
			assertContentMode(t, filepath.Join(root, "nested/c"), "new-c", 0o600)
			assertNoTransactions(t, root)
		})
	}
}

func TestApplyRequiresTheExactPlanForCrashRecovery(t *testing.T) {
	if !hostProcessLockSupported {
		t.Skip("host process lock is unsupported")
	}
	root := t.TempDir()
	writeMode(t, filepath.Join(root, "a"), []byte("old-a"), 0o600)
	writeMode(t, filepath.Join(root, "b"), []byte("old-b"), 0o640)
	crashedPlan := crashTestPlan(t, root)
	unrelated, err := BuildPlan(root, []Change{{
		Path: "unrelated", Operation: OperationCreate, Content: []byte("no"), Mode: 0o600,
	}})
	if err != nil {
		t.Fatal(err)
	}
	command := workspaceCrashHelperCommand(t, root, faultAfterBackup, "a")
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("crash helper unexpectedly succeeded: %s", output)
	}
	if _, err := Apply(context.Background(), root, unrelated); !errors.Is(err, ErrRecovery) {
		t.Fatalf("unrelated recovery error = %v, want ErrRecovery", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "unrelated")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unrelated plan mutated workspace: %v", err)
	}
	if _, err := Apply(context.Background(), root, crashedPlan); err != nil {
		t.Fatalf("exact plan recovery failed: %v", err)
	}
	assertNoTransactions(t, root)
}

func TestCrashRecoveryDoesNotOverwriteAnUnrecognizedConcurrentTarget(t *testing.T) {
	if !hostProcessLockSupported {
		t.Skip("host process lock is unsupported")
	}
	root := t.TempDir()
	writeMode(t, filepath.Join(root, "a"), []byte("old-a"), 0o600)
	writeMode(t, filepath.Join(root, "b"), []byte("old-b"), 0o640)
	plan := crashTestPlan(t, root)
	command := workspaceCrashHelperCommand(t, root, faultAfterInstall, "a")
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("crash helper unexpectedly succeeded: %s", output)
	}
	writeMode(t, filepath.Join(root, "a"), []byte("concurrent"), 0o600)
	if _, err := Apply(context.Background(), root, plan); !errors.Is(err, ErrRecovery) {
		t.Fatalf("concurrent-target recovery error = %v, want ErrRecovery", err)
	}
	if got := readFile(t, filepath.Join(root, "a")); got != "concurrent" {
		t.Fatalf("recovery overwrote unrecognized target: %q", got)
	}
}

func TestCrashRecoveryFailsClosedWhenAssertChanged(t *testing.T) {
	if !hostProcessLockSupported {
		t.Skip("host process lock is unsupported")
	}
	root := t.TempDir()
	writeMode(t, filepath.Join(root, "a"), []byte("old-a"), 0o600)
	writeMode(t, filepath.Join(root, "b"), []byte("old-b"), 0o640)
	plan := crashTestPlan(t, root)
	command := workspaceCrashHelperCommand(t, root, faultAfterInstall, "a")
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("crash helper unexpectedly succeeded: %s", output)
	}
	writeFile(t, filepath.Join(root, "guard-does-not-exist"), "concurrent input")
	if _, err := Apply(context.Background(), root, plan); !errors.Is(err, ErrRecovery) {
		t.Fatalf("changed assertion recovery error = %v, want ErrRecovery", err)
	}
	if got := readFile(t, filepath.Join(root, "a")); got != "new-a" {
		t.Fatalf("failed-closed recovery changed installed target: %q", got)
	}
	if err := os.Remove(filepath.Join(root, "guard-does-not-exist")); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), root, plan); err != nil {
		t.Fatalf("exact recovery after restoring assertion failed: %v", err)
	}
	assertNoTransactions(t, root)
}

func TestApplyCleansAnUnpreparedMatchingTransactionOnlyFromBeforeState(t *testing.T) {
	root := t.TempDir()
	plan, err := BuildPlan(root, []Change{{
		Path: "target", Operation: OperationCreate, Content: []byte("value"), Mode: 0o600,
	}})
	if err != nil {
		t.Fatal(err)
	}
	name := transactionDirectoryName(plan.Digest(), "00000000000000000000000000000000")
	if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := Apply(context.Background(), root, plan)
	if err != nil || result.Disposition != ApplyCommitted {
		t.Fatalf("Apply(unprepared retry) = (%#v, %v)", result, err)
	}
	assertNoTransactions(t, root)
}

func TestWorkspaceCrashHelperProcess(t *testing.T) {
	point := applyFaultPoint(os.Getenv("APPKIT_WORKSPACE_CRASH_POINT"))
	if point == "" {
		return
	}
	root := os.Getenv("APPKIT_WORKSPACE_CRASH_ROOT")
	target := os.Getenv("APPKIT_WORKSPACE_CRASH_TARGET")
	plan := crashTestPlan(t, root)
	_, err := apply(context.Background(), root, plan, applyOptions{inject: func(actual applyFaultPoint, actualTarget string) error {
		if actual == point && actualTarget == target {
			os.Exit(87)
		}
		return nil
	}})
	t.Fatalf("Apply did not terminate at %s for %q: %v", point, target, err)
}

func crashTestPlan(t testing.TB, root string) Plan {
	t.Helper()
	plan, err := BuildPlan(root, []Change{
		{Path: "a", Operation: OperationUpdate, Content: []byte("new-a"), Mode: 0o644},
		{Path: "b", Operation: OperationDelete},
		{Path: "guard-does-not-exist", Operation: OperationAssert},
		{Path: "nested/c", Operation: OperationCreate, Content: []byte("new-c"), Mode: 0o600},
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func workspaceCrashHelperCommand(t *testing.T, root string, point applyFaultPoint, target string) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestWorkspaceCrashHelperProcess$")
	command.Env = append(os.Environ(),
		"APPKIT_WORKSPACE_CRASH_POINT="+string(point),
		"APPKIT_WORKSPACE_CRASH_ROOT="+root,
		"APPKIT_WORKSPACE_CRASH_TARGET="+target,
	)
	return command
}

func TestConcurrentSamePlanCommitsOnceAndReplaysOnce(t *testing.T) {
	root := t.TempDir()
	plan, err := BuildPlan(root, []Change{{Path: "target", Operation: OperationCreate, Content: []byte("value"), Mode: 0o600}})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan ApplyResult, 2)
	errorsChannel := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			result, applyErr := Apply(context.Background(), root, plan)
			results <- result
			errorsChannel <- applyErr
		}()
	}
	close(start)
	firstErr, secondErr := <-errorsChannel, <-errorsChannel
	if firstErr != nil || secondErr != nil {
		t.Fatalf("concurrent replay errors = (%v, %v)", firstErr, secondErr)
	}
	dispositions := []ApplyDisposition{(<-results).Disposition, (<-results).Disposition}
	slices.Sort(dispositions)
	if !reflect.DeepEqual(dispositions, []ApplyDisposition{ApplyCommitted, ApplyReplayed}) {
		t.Fatalf("dispositions = %v", dispositions)
	}
}

func TestMidCommitFaultRollsBackEveryTarget(t *testing.T) {
	root := t.TempDir()
	writeMode(t, filepath.Join(root, "a"), []byte("old-a"), 0o600)
	writeMode(t, filepath.Join(root, "b"), []byte("old-b"), 0o640)
	writeMode(t, filepath.Join(root, "c"), []byte("old-c"), 0o644)
	plan, err := BuildPlan(root, []Change{
		{Path: "a", Operation: OperationUpdate, Content: []byte("new-a"), Mode: 0o644},
		{Path: "b", Operation: OperationUpdate, Content: []byte("new-b"), Mode: 0o600},
		{Path: "c", Operation: OperationUpdate, Content: []byte("new-c"), Mode: 0o600},
	})
	if err != nil {
		t.Fatal(err)
	}
	fault := errors.New("synthetic mid-commit fault")
	rollbackOrder := make([]string, 0)
	result, err := apply(context.Background(), root, plan, applyOptions{inject: func(point applyFaultPoint, target string) error {
		if point == faultAfterOperation && target == "b" {
			return fault
		}
		if point == faultBeforeRollbackRemove {
			rollbackOrder = append(rollbackOrder, target)
		}
		return nil
	}})
	if !errors.Is(err, fault) || errors.Is(err, ErrRollback) || result.Disposition != "" {
		t.Fatalf("apply(fault) = (%#v, %v)", result, err)
	}
	assertContentMode(t, filepath.Join(root, "a"), "old-a", 0o600)
	assertContentMode(t, filepath.Join(root, "b"), "old-b", 0o640)
	assertContentMode(t, filepath.Join(root, "c"), "old-c", 0o644)
	if !reflect.DeepEqual(rollbackOrder, []string{"b", "a"}) {
		t.Fatalf("rollback order = %v, want reverse commit order", rollbackOrder)
	}
	assertNoTransactions(t, root)
}

func TestFaultAfterBackupRestoresOriginal(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "target"), "before")
	plan, err := BuildPlan(root, []Change{{Path: "target", Operation: OperationDelete}})
	if err != nil {
		t.Fatal(err)
	}
	fault := errors.New("synthetic post-backup fault")
	_, err = apply(context.Background(), root, plan, applyOptions{inject: func(point applyFaultPoint, _ string) error {
		if point == faultAfterBackup {
			return fault
		}
		return nil
	}})
	if !errors.Is(err, fault) || errors.Is(err, ErrRollback) {
		t.Fatalf("apply(fault) error = %v", err)
	}
	if got := readFile(t, filepath.Join(root, "target")); got != "before" {
		t.Fatalf("restored target = %q", got)
	}
	assertNoTransactions(t, root)
}

func TestRollbackRemovesParentsCreatedForNestedTarget(t *testing.T) {
	root := t.TempDir()
	plan, err := BuildPlan(root, []Change{
		{Path: "nested/deep/a", Operation: OperationCreate, Content: []byte("a"), Mode: 0o600},
		{Path: "z", Operation: OperationCreate, Content: []byte("z"), Mode: 0o600},
	})
	if err != nil {
		t.Fatal(err)
	}
	fault := errors.New("synthetic nested create fault")
	_, err = apply(context.Background(), root, plan, applyOptions{inject: func(point applyFaultPoint, target string) error {
		if point == faultAfterOperation && target == "nested/deep/a" {
			return fault
		}
		return nil
	}})
	if !errors.Is(err, fault) || errors.Is(err, ErrRollback) {
		t.Fatalf("apply(fault) error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "nested")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("created parent remains after rollback: %v", err)
	}
	assertNoTransactions(t, root)
}

func TestCancelledApplyDoesNotMutate(t *testing.T) {
	root := t.TempDir()
	plan, err := BuildPlan(root, []Change{{Path: "target", Operation: OperationCreate, Content: []byte("value"), Mode: 0o600}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Apply(ctx, root, plan); !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply(cancelled) error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "target")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("cancelled apply created target: %v", err)
	}
	assertNoTransactions(t, root)
}

func TestRollbackFailureIsJoinedAndPreservesRecoveryData(t *testing.T) {
	root := t.TempDir()
	plan, err := BuildPlan(root, []Change{
		{Path: "a", Operation: OperationCreate, Content: []byte("new-a"), Mode: 0o600},
		{Path: "b", Operation: OperationCreate, Content: []byte("new-b"), Mode: 0o600},
	})
	if err != nil {
		t.Fatal(err)
	}
	commitFault := errors.New("synthetic commit fault")
	rollbackFault := errors.New("synthetic rollback fault")
	_, err = apply(context.Background(), root, plan, applyOptions{inject: func(point applyFaultPoint, target string) error {
		if point == faultAfterOperation && target == "a" {
			return commitFault
		}
		if point == faultBeforeRollbackRemove && target == "a" {
			return rollbackFault
		}
		return nil
	}})
	if !errors.Is(err, commitFault) || !errors.Is(err, rollbackFault) || !errors.Is(err, ErrRollback) {
		t.Fatalf("apply(rollback fault) error = %v", err)
	}
	if got := readFile(t, filepath.Join(root, "a")); got != "new-a" {
		t.Fatalf("faulted target = %q", got)
	}
	entries, globErr := filepath.Glob(filepath.Join(root, ".appkit-workspace-txn-*"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(entries) != 1 {
		t.Fatalf("recovery transaction count = %d, want 1", len(entries))
	}
	result, err := Apply(context.Background(), root, plan)
	if err != nil || result.Disposition != ApplyCommitted {
		t.Fatalf("Apply(recover rollback failure) = (%#v, %v)", result, err)
	}
	if got := readFile(t, filepath.Join(root, "a")); got != "new-a" {
		t.Fatalf("recovered a = %q", got)
	}
	if got := readFile(t, filepath.Join(root, "b")); got != "new-b" {
		t.Fatalf("recovered b = %q", got)
	}
	assertNoTransactions(t, root)
}

func TestApplyRejectsZeroAndTamperedPlan(t *testing.T) {
	root := t.TempDir()
	if _, err := Apply(context.Background(), root, Plan{}); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("Apply(zero plan) error = %v", err)
	}
	plan, err := BuildPlan(root, []Change{{Path: "target", Operation: OperationCreate, Content: []byte("value"), Mode: 0o600}})
	if err != nil {
		t.Fatal(err)
	}
	plan.changes[0].content[0] = 'X'
	if _, err := Apply(context.Background(), root, plan); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("Apply(tampered plan) error = %v", err)
	}
}

func writeMode(t *testing.T, name string, content []byte, mode fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(name, content, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(name, mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func assertContentMode(t *testing.T, name, content string, mode fs.FileMode) {
	t.Helper()
	if got := readFile(t, name); got != content {
		t.Fatalf("%s content = %q, want %q", name, got, content)
	}
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != mode {
		t.Fatalf("%s mode = %v, want %v", name, got, mode)
	}
}

func assertNoTransactions(t *testing.T, root string) {
	t.Helper()
	var entries []string
	for _, prefix := range []string{transactionPrefix, completedPrefix} {
		matches, err := filepath.Glob(filepath.Join(root, prefix+"*"))
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, matches...)
	}
	if len(entries) != 0 {
		t.Fatalf("transaction debris = %v", entries)
	}
}
