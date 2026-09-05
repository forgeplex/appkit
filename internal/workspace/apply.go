package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"
)

// ErrLockUnsupported identifies a host or filesystem that cannot provide the
// cross-process exclusion required for workspace mutation.
var ErrLockUnsupported = errors.New("workspace: inter-process locking unsupported")

// ErrCommitAdmission identifies a commit gate that rejected or returned an
// invalid receipt. The callback's original error remains in the error chain.
var ErrCommitAdmission = errors.New("workspace: commit admission rejected")

// ApplyDisposition distinguishes a new commit from an idempotent replay.
type ApplyDisposition string

const (
	ApplyCommitted ApplyDisposition = "committed"
	ApplyReplayed  ApplyDisposition = "replayed"
)

// A non-empty ApplyResult disposition means every target reached the planned
// final state. Replayed means no filesystem mutation was necessary.
type ApplyResult struct {
	PlanDigest             string
	Disposition            ApplyDisposition
	AdmissionReceiptDigest string
}

// CommitGate runs inside the exclusive workspace lock. Inspect is an optional
// deterministic endpoint check performed after crash recovery but before
// staging or state classification. Admit runs after staging and returns a
// durable receipt digest; Recheck verifies it after the recovery journal is
// synced and immediately before the first public mutation. SubjectDigest binds
// a higher-level immutable plan (for example an upgrade apply plan). If the
// process dies after public mutation starts, recovery trusts only the persisted
// domain, subject, and receipt and never invokes any callback.
type CommitGate struct {
	Domain        string
	SubjectDigest string
	Inspect       func(context.Context) error
	Admit         func(context.Context) (string, error)
	Recheck       func(context.Context, string) error
}

// Apply installs a valid plan after rechecking its exact precondition under a
// process-local and host-process workspace lock. Files are staged and backed
// up on the same filesystem, and a failed commit is rolled back in reverse
// order. A prepared transaction is durably recorded before the first public
// rename. If a process terminates mid-commit, only the exact same Plan may
// recover it; an unrelated plan fails closed with ErrRecovery.
//
// Cooperative appkit processes are serialized. Unrelated writers that ignore
// the advisory lock are detected by target preconditions where possible and
// remain outside this primitive's authority.
func Apply(ctx context.Context, root string, plan Plan) (result ApplyResult, resultErr error) {
	return apply(ctx, root, plan, applyOptions{})
}

// ApplyWithCommitGate applies a plan through a higher-level admission gate.
// It has the same crash recovery semantics as Apply, but an active prepared
// transaction can be recovered only by the exact gate domain and subject.
func ApplyWithCommitGate(
	ctx context.Context,
	root string,
	plan Plan,
	gate CommitGate,
) (result ApplyResult, resultErr error) {
	return apply(ctx, root, plan, applyOptions{gate: &gate})
}

type applyFaultPoint string

const (
	faultAfterBackup           applyFaultPoint = "after-backup"
	faultAfterInstall          applyFaultPoint = "after-install"
	faultAfterOperation        applyFaultPoint = "after-operation"
	faultBeforeRollbackRemove  applyFaultPoint = "before-rollback-remove"
	faultBeforeRollbackRestore applyFaultPoint = "before-rollback-restore"
)

type applyOptions struct {
	inject func(point applyFaultPoint, target string) error
	gate   *CommitGate
}

type stagedChange struct {
	path string
}

type journalEntry struct {
	target        string
	backup        string
	originalMoved bool
	installed     bool
}

func apply(ctx context.Context, root string, plan Plan, options applyOptions) (result ApplyResult, resultErr error) {
	if ctx == nil {
		panic("workspace: nil context")
	}
	result.PlanDigest = plan.digest
	if err := plan.validate(); err != nil {
		return result, err
	}
	if err := validateCommitGate(options.gate); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("workspace: apply: %w", err)
	}

	rooted, err := openWorkspaceRoot(root)
	if err != nil {
		return result, err
	}
	defer func() {
		if closeErr := rooted.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("workspace: close root: %w", closeErr))
		}
	}()

	unlock, err := acquireApplyLock(ctx, rooted)
	if err != nil {
		return result, err
	}
	defer func() {
		if unlockErr := unlock(); unlockErr != nil {
			resultErr = errors.Join(resultErr, unlockErr)
		}
	}()

	recovery, err := recoverTransactions(ctx, rooted, plan, options.gate)
	if err != nil {
		return result, err
	}
	if options.gate != nil && recovery.Prepared {
		state, err := classifyState(rooted, plan)
		if err != nil {
			return result, err
		}
		if state == stateFinal {
			result.Disposition = ApplyReplayed
			result.AdmissionReceiptDigest = recovery.ReceiptDigest
			return result, nil
		}
		return result, ErrRecoveryRestart
	}
	if err := inspectCommit(ctx, options.gate); err != nil {
		return result, err
	}

	state, err := classifyState(rooted, plan)
	if err != nil {
		return result, err
	}
	if state == stateFinal {
		result.Disposition = ApplyReplayed
		result.AdmissionReceiptDigest = recovery.ReceiptDigest
		return result, nil
	}
	txn, err := createTransactionDir(rooted, plan)
	if err != nil {
		return result, err
	}
	staged, err := stageWrites(ctx, rooted, txn, plan)
	if err != nil {
		return result, errors.Join(err, cleanupTransaction(rooted, txn))
	}

	// Staging can be arbitrarily expensive. Recheck immediately before the
	// first final-state rename so stale work cannot cross the commit boundary.
	state, err = classifyState(rooted, plan)
	if err != nil {
		return result, errors.Join(err, cleanupTransaction(rooted, txn))
	}
	if state == stateFinal {
		result.Disposition = ApplyReplayed
		if err := cleanupTransaction(rooted, txn); err != nil {
			return result, err
		}
		return result, nil
	}
	if state != stateBefore {
		return result, errors.Join(
			fmt.Errorf("%w: targets reached final state while staging", ErrChanged),
			cleanupTransaction(rooted, txn),
		)
	}
	if err := verifyOpenedRootPath(rooted); err != nil {
		return result, errors.Join(err, cleanupTransaction(rooted, txn))
	}
	admission, err := admitCommit(ctx, options.gate)
	if err != nil {
		return result, errors.Join(err, cleanupTransaction(rooted, txn))
	}
	if err := prepareTransaction(rooted, txn, plan, admission); err != nil {
		return result, errors.Join(err, cleanupTransaction(rooted, txn))
	}

	journal := make([]journalEntry, 0, len(plan.changes))
	createdDirectories := make([]string, 0)
	admissionRechecked := options.gate == nil
	if len(plan.guards) > 0 {
		// Parent-directory creation is itself a public membership change. Check
		// the complete before trees after admission and before creating parents.
		if !admissionRechecked {
			if err := recheckCommit(ctx, options.gate, admission.ReceiptDigest); err != nil {
				return result, errors.Join(err, cleanupTransaction(rooted, txn))
			}
			admissionRechecked = true
		}
		if err := verifyDirectoryGuards(rooted, plan, false); err != nil {
			return result, errors.Join(err, cleanupTransaction(rooted, txn))
		}
	}
	for index := range plan.changes {
		if err := ctx.Err(); err != nil {
			return result, failCommit(rooted, txn, plan, journal, createdDirectories, options, fmt.Errorf("workspace: apply: %w", err))
		}
		change := plan.changes[index]
		before := plan.before.files[index]
		final := plan.finalFiles[index]
		if err := verifyOpenedRootPath(rooted); err != nil {
			return result, failCommit(rooted, txn, plan, journal, createdDirectories, options, err)
		}
		if change.public.Operation == OperationAssert {
			if err := verifyOneTarget(rooted, before); err != nil {
				return result, failCommit(rooted, txn, plan, journal, createdDirectories, options, err)
			}
			continue
		}

		created, parentErr := ensureTargetParents(rooted, change.public.Path, change.public.Operation == OperationCreate)
		createdDirectories = append(createdDirectories, created...)
		if parentErr != nil {
			return result, failCommit(rooted, txn, plan, journal, createdDirectories, options, parentErr)
		}
		if err := verifyOneTarget(rooted, before); err != nil {
			return result, failCommit(rooted, txn, plan, journal, createdDirectories, options, err)
		}
		if !admissionRechecked {
			if err := recheckCommit(ctx, options.gate, admission.ReceiptDigest); err != nil {
				return result, failCommit(rooted, txn, plan, journal, createdDirectories, options, err)
			}
			admissionRechecked = true
		}

		entry := journalEntry{target: change.public.Path}
		journal = append(journal, entry)
		entryIndex := len(journal) - 1
		if before.Exists {
			journal[entryIndex].backup = path.Join(txn, fmt.Sprintf("%06d.backup", index))
			if err := rooted.Rename(change.public.Path, journal[entryIndex].backup); err != nil {
				return result, failCommit(rooted, txn, plan, journal, createdDirectories, options,
					fmt.Errorf("workspace: back up %q: %w", change.public.Path, err))
			}
			journal[entryIndex].originalMoved = true
			if err := syncRename(rooted, change.public.Path, journal[entryIndex].backup); err != nil {
				return result, failCommit(rooted, txn, plan, journal, createdDirectories, options, err)
			}
			if err := verifyEquivalentFile(rooted, journal[entryIndex].backup, before); err != nil {
				return result, failCommit(rooted, txn, plan, journal, createdDirectories, options, err)
			}
			if err := injectFault(options, faultAfterBackup, change.public.Path); err != nil {
				return result, failCommit(rooted, txn, plan, journal, createdDirectories, options, err)
			}
		}

		if change.public.Operation != OperationDelete {
			if err := rooted.Rename(staged[index].path, change.public.Path); err != nil {
				return result, failCommit(rooted, txn, plan, journal, createdDirectories, options,
					fmt.Errorf("workspace: install %q: %w", change.public.Path, err))
			}
			journal[entryIndex].installed = true
			if err := syncRename(rooted, staged[index].path, change.public.Path); err != nil {
				return result, failCommit(rooted, txn, plan, journal, createdDirectories, options, err)
			}
			if err := verifyOneTarget(rooted, final); err != nil {
				return result, failCommit(rooted, txn, plan, journal, createdDirectories, options, err)
			}
			if err := injectFault(options, faultAfterInstall, change.public.Path); err != nil {
				return result, failCommit(rooted, txn, plan, journal, createdDirectories, options, err)
			}
		}
		if err := injectFault(options, faultAfterOperation, change.public.Path); err != nil {
			return result, failCommit(rooted, txn, plan, journal, createdDirectories, options, err)
		}
	}

	current, err := capturePlanFiles(rooted, plan)
	if err != nil {
		return result, failCommit(rooted, txn, plan, journal, createdDirectories, options, err)
	}
	if digestFiles(current) != plan.finalDigest {
		return result, failCommit(rooted, txn, plan, journal, createdDirectories, options,
			fmt.Errorf("%w: final target set does not match plan", ErrChanged))
	}
	if err := verifyDirectoryGuards(rooted, plan, true); err != nil {
		return result, failCommit(rooted, txn, plan, journal, createdDirectories, options, err)
	}
	if err := verifyOpenedRootPath(rooted); err != nil {
		return result, failCommit(rooted, txn, plan, journal, createdDirectories, options, err)
	}
	result.Disposition = ApplyCommitted
	result.AdmissionReceiptDigest = admission.ReceiptDigest
	if err := finalizeTransaction(rooted, txn); err != nil {
		// All targets are already verified in their complete final state. A
		// cleanup error may leave private staging debris, never a partial target
		// set, and is still surfaced to the caller.
		return result, err
	}
	return result, nil
}

func validateCommitGate(gate *CommitGate) error {
	if gate == nil {
		return nil
	}
	if len(gate.Domain) == 0 || len(gate.Domain) > 128 || strings.TrimSpace(gate.Domain) != gate.Domain ||
		strings.ContainsRune(gate.Domain, '\x00') || !validDigest(gate.SubjectDigest) ||
		gate.Admit == nil || gate.Recheck == nil {
		return fmt.Errorf("%w: malformed commit gate", ErrCommitAdmission)
	}
	for _, character := range gate.Domain {
		if character > 0x7f || !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' ||
			character == '.' || character == '-' || character == '/') {
			return fmt.Errorf("%w: malformed commit gate domain", ErrCommitAdmission)
		}
	}
	return nil
}

func admitCommit(ctx context.Context, gate *CommitGate) (transactionAdmission, error) {
	if gate == nil {
		return transactionAdmission{}, nil
	}
	receipt, err := gate.Admit(ctx)
	if err != nil {
		return transactionAdmission{}, fmt.Errorf("%w: admit: %w", ErrCommitAdmission, err)
	}
	if !validDigest(receipt) {
		return transactionAdmission{}, fmt.Errorf("%w: admit returned an invalid receipt digest", ErrCommitAdmission)
	}
	return transactionAdmission{Domain: gate.Domain, SubjectDigest: gate.SubjectDigest, ReceiptDigest: receipt}, nil
}

func inspectCommit(ctx context.Context, gate *CommitGate) error {
	if gate == nil || gate.Inspect == nil {
		return nil
	}
	if err := gate.Inspect(ctx); err != nil {
		return fmt.Errorf("%w: inspect: %w", ErrCommitAdmission, err)
	}
	return nil
}

func recheckCommit(ctx context.Context, gate *CommitGate, receipt string) error {
	if gate == nil {
		return nil
	}
	if err := gate.Recheck(ctx, receipt); err != nil {
		return fmt.Errorf("%w: recheck: %w", ErrCommitAdmission, err)
	}
	return nil
}

type planState uint8

const (
	stateBefore planState = iota
	stateFinal
)

func classifyState(root *os.Root, plan Plan) (planState, error) {
	files, err := capturePlanFiles(root, plan)
	if err != nil {
		return 0, fmt.Errorf("%w: inspect targets: %w", ErrChanged, err)
	}
	digest := digestFiles(files)
	if digest == plan.finalDigest {
		if err := verifyDirectoryGuards(root, plan, true); err == nil {
			return stateFinal, nil
		} else if digest != plan.before.digest {
			return 0, err
		}
	}
	if digest == plan.before.digest {
		if err := verifyDirectoryGuards(root, plan, false); err != nil {
			return 0, err
		}
		return stateBefore, nil
	}
	return 0, fmt.Errorf("%w: target set digest is %s; expected %s or replay state %s", ErrChanged, digest, plan.before.digest, plan.finalDigest)
}

func capturePlanFiles(root *os.Root, plan Plan) ([]File, error) {
	files := make([]File, len(plan.changes))
	for index, change := range plan.changes {
		file, err := captureFile(root, change.public.Path)
		if err != nil {
			return nil, err
		}
		files[index] = file
	}
	return files, nil
}

func createTransactionDir(root *os.Root, plan Plan) (string, error) {
	for attempt := 0; attempt < 16; attempt++ {
		var entropy [16]byte
		if _, err := io.ReadFull(rand.Reader, entropy[:]); err != nil {
			return "", fmt.Errorf("workspace: transaction entropy: %w", err)
		}
		name := transactionDirectoryName(plan.digest, hex.EncodeToString(entropy[:]))
		conflict := false
		for _, change := range plan.changes {
			if change.public.Path == name || pathHasPrefix(change.public.Path, name) {
				conflict = true
				break
			}
		}
		if conflict {
			continue
		}
		if err := root.Mkdir(name, 0o700); err == nil {
			return name, nil
		} else if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("workspace: create transaction directory: %w", err)
		}
	}
	return "", fmt.Errorf("workspace: create transaction directory: too many name collisions")
}

func stageWrites(ctx context.Context, root *os.Root, txn string, plan Plan) ([]stagedChange, error) {
	result := make([]stagedChange, len(plan.changes))
	for index, change := range plan.changes {
		if change.public.Operation == OperationDelete || change.public.Operation == OperationAssert {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("workspace: stage: %w", err)
		}
		name := path.Join(txn, fmt.Sprintf("%06d.content", index))
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, fmt.Errorf("workspace: create staged content for %q: %w", change.public.Path, err)
		}
		writeErr := writeContent(ctx, file, change.content)
		if writeErr == nil {
			writeErr = file.Chmod(change.public.Mode)
		}
		if writeErr == nil {
			writeErr = file.Sync()
		}
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			var result error
			if writeErr != nil {
				result = errors.Join(result, fmt.Errorf("workspace: write staged content for %q: %w", change.public.Path, writeErr))
			}
			if closeErr != nil {
				result = errors.Join(result, fmt.Errorf("workspace: close staged content for %q: %w", change.public.Path, closeErr))
			}
			return nil, result
		}
		expected := plan.finalFiles[index]
		if err := verifyEquivalentFile(root, name, expected); err != nil {
			return nil, err
		}
		result[index] = stagedChange{path: name}
	}
	return result, nil
}

func writeContent(ctx context.Context, file *os.File, content []byte) error {
	for len(content) != 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		written, err := file.Write(content)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		content = content[written:]
	}
	return nil
}

func ensureTargetParents(root *os.Root, target string, allowCreate bool) ([]string, error) {
	parent := path.Dir(target)
	if parent == "." {
		return nil, nil
	}
	parts := strings.Split(parent, "/")
	created := make([]string, 0)
	prefix := ""
	for _, part := range parts {
		if part == "" || part == "/" {
			continue
		}
		if prefix == "" {
			prefix = part
		} else {
			prefix = path.Join(prefix, part)
		}
		info, err := root.Lstat(prefix)
		if errors.Is(err, fs.ErrNotExist) {
			if !allowCreate {
				return created, fmt.Errorf("%w: parent %q disappeared", ErrChanged, prefix)
			}
			if err := root.Mkdir(prefix, 0o755); err != nil {
				return created, fmt.Errorf("workspace: create parent %q: %w", prefix, err)
			}
			if err := syncDirectory(root, path.Dir(prefix)); err != nil {
				return created, err
			}
			created = append(created, prefix)
			continue
		}
		if err != nil {
			return created, fmt.Errorf("workspace: inspect parent %q: %w", prefix, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return created, fmt.Errorf("%w: %q", ErrSymlink, prefix)
		}
		if !info.IsDir() {
			return created, fmt.Errorf("%w: %q has a non-directory parent", ErrInvalidPath, target)
		}
	}
	return created, nil
}

func verifyOneTarget(root *os.Root, expected File) error {
	current, err := captureFile(root, expected.Path)
	if err != nil {
		return fmt.Errorf("%w: inspect %q: %w", ErrChanged, expected.Path, err)
	}
	if current != expected {
		return fmt.Errorf("%w: target %q no longer matches its planned state", ErrChanged, expected.Path)
	}
	return nil
}

func verifyEquivalentFile(root *os.Root, actualPath string, expected File) error {
	current, err := captureFile(root, actualPath)
	if err != nil {
		return fmt.Errorf("%w: verify staged state for %q: %w", ErrChanged, expected.Path, err)
	}
	if !current.Exists || current.Mode != expected.Mode || current.Digest != expected.Digest {
		return fmt.Errorf("%w: staged state for %q does not match plan", ErrChanged, expected.Path)
	}
	return nil
}

func failCommit(
	root *os.Root,
	txn string,
	plan Plan,
	journal []journalEntry,
	createdDirectories []string,
	options applyOptions,
	primary error,
) error {
	rollbackErr := rollback(root, txn, plan, journal, createdDirectories, options)
	return errors.Join(primary, rollbackErr)
}

func rollback(
	root *os.Root,
	txn string,
	plan Plan,
	journal []journalEntry,
	createdDirectories []string,
	options applyOptions,
) error {
	var failures error
	for index := len(journal) - 1; index >= 0; index-- {
		entry := journal[index]
		if entry.installed {
			if err := injectFault(options, faultBeforeRollbackRemove, entry.target); err != nil {
				failures = errors.Join(failures, err)
			} else if err := root.Remove(entry.target); err != nil && !errors.Is(err, fs.ErrNotExist) {
				failures = errors.Join(failures, fmt.Errorf("remove installed %q: %w", entry.target, err))
			} else if err == nil {
				failures = errors.Join(failures, syncDirectory(root, path.Dir(entry.target)))
			}
		}
		if entry.originalMoved {
			if err := injectFault(options, faultBeforeRollbackRestore, entry.target); err != nil {
				failures = errors.Join(failures, err)
			} else if err := root.Rename(entry.backup, entry.target); err != nil {
				failures = errors.Join(failures, fmt.Errorf("restore %q: %w", entry.target, err))
			} else {
				failures = errors.Join(failures, syncRename(root, entry.backup, entry.target))
			}
		}
	}
	for index := len(createdDirectories) - 1; index >= 0; index-- {
		if err := root.Remove(createdDirectories[index]); err != nil && !errors.Is(err, fs.ErrNotExist) {
			failures = errors.Join(failures, fmt.Errorf("remove created directory %q: %w", createdDirectories[index], err))
		} else if err == nil {
			failures = errors.Join(failures, syncDirectory(root, path.Dir(createdDirectories[index])))
		}
	}
	current, err := capturePlanFiles(root, plan)
	if err != nil {
		failures = errors.Join(failures, fmt.Errorf("verify rollback: %w", err))
	} else if digestFiles(current) != plan.before.digest {
		failures = errors.Join(failures, fmt.Errorf("verify rollback: %w", ErrChanged))
	}
	if err := verifyDirectoryGuards(root, plan, false); err != nil {
		failures = errors.Join(failures, fmt.Errorf("verify rollback directories: %w", err))
	}
	// If restoring a target failed, its backup may be the only recoverable
	// copy. Preserve the transaction directory instead of deleting evidence.
	if failures == nil {
		if err := cleanupTransaction(root, txn); err != nil {
			failures = errors.Join(failures, err)
		}
	}
	if failures == nil {
		return nil
	}
	return fmt.Errorf("%w; recovery data retained in %q: %w", ErrRollback, txn, failures)
}

func cleanupTransaction(root *os.Root, txn string) error {
	if err := root.RemoveAll(txn); err != nil {
		return fmt.Errorf("workspace: clean transaction %q: %w", txn, err)
	}
	if err := syncDirectory(root, "."); err != nil {
		return err
	}
	return nil
}

func injectFault(options applyOptions, point applyFaultPoint, target string) error {
	if options.inject == nil {
		return nil
	}
	if err := options.inject(point, target); err != nil {
		return fmt.Errorf("workspace: injected fault at %s for %q: %w", point, target, err)
	}
	return nil
}

func pathHasPrefix(name, parent string) bool {
	return len(name) > len(parent) && name[:len(parent)] == parent && name[len(parent)] == '/'
}

type processLock struct {
	token chan struct{}
	refs  int
}

var processLockRegistry = struct {
	sync.Mutex
	byRoot map[string]*processLock
}{byRoot: make(map[string]*processLock)}

func acquireProcessLock(ctx context.Context, root string) (func(), error) {
	processLockRegistry.Lock()
	lock := processLockRegistry.byRoot[root]
	if lock == nil {
		lock = &processLock{token: make(chan struct{}, 1)}
		lock.token <- struct{}{}
		processLockRegistry.byRoot[root] = lock
	}
	lock.refs++
	processLockRegistry.Unlock()

	select {
	case <-ctx.Done():
		releaseProcessLockReference(root, lock)
		return nil, fmt.Errorf("workspace: wait for apply lock: %w", ctx.Err())
	case <-lock.token:
		if err := ctx.Err(); err != nil {
			lock.token <- struct{}{}
			releaseProcessLockReference(root, lock)
			return nil, fmt.Errorf("workspace: wait for apply lock: %w", err)
		}
		var once sync.Once
		return func() {
			once.Do(func() {
				lock.token <- struct{}{}
				releaseProcessLockReference(root, lock)
			})
		}, nil
	}
}

func releaseProcessLockReference(root string, lock *processLock) {
	processLockRegistry.Lock()
	defer processLockRegistry.Unlock()
	lock.refs--
	if lock.refs == 0 && processLockRegistry.byRoot[root] == lock {
		delete(processLockRegistry.byRoot, root)
	}
}

// acquireApplyLock composes an in-process queue with the platform advisory
// lock. The local queue is acquired first so platforms whose lock ownership is
// process-scoped cannot accidentally allow two goroutines into Apply at once.
func acquireApplyLock(ctx context.Context, root *os.Root) (func() error, error) {
	localUnlock, err := acquireProcessLock(ctx, root.Name())
	if err != nil {
		return nil, err
	}
	hostUnlock, err := acquireHostProcessLock(ctx, root)
	if err != nil {
		localUnlock()
		return nil, err
	}
	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			releaseErr = hostUnlock()
			localUnlock()
		})
		return releaseErr
	}, nil
}
