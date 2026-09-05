package workspace

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
)

var (
	ErrRecovery = errors.New("workspace: recovery required")
	// ErrRecoveryRestart identifies a successfully rolled-back admitted
	// transaction. A gated caller must invoke apply again so a fresh admission
	// is distinguishable from crash recovery.
	ErrRecoveryRestart = errors.New("workspace: admitted transaction recovered; retry required")
)

const (
	transactionPrefix     = ".appkit-workspace-txn-"
	completedPrefix       = ".appkit-workspace-done-"
	transactionRecordName = "prepared.json"
	transactionFormat     = "appkit.workspace.transaction.v1"
	maxTransactionRecord  = 4 << 10
)

type transactionRecord struct {
	Format           string `json:"format"`
	PlanDigest       string `json:"planDigest"`
	BeforeDigest     string `json:"beforeDigest"`
	FinalDigest      string `json:"finalDigest"`
	ChangeCount      int    `json:"changeCount"`
	AdmissionDomain  string `json:"admissionDomain,omitempty"`
	AdmissionSubject string `json:"admissionSubject,omitempty"`
	AdmissionReceipt string `json:"admissionReceipt,omitempty"`
}

type transactionAdmission struct {
	Domain        string
	SubjectDigest string
	ReceiptDigest string
}

type transactionRecovery struct {
	Prepared      bool
	ReceiptDigest string
}

func reservedWorkspacePath(name string) bool {
	first, _, _ := strings.Cut(name, "/")
	return strings.HasPrefix(first, ".appkit-workspace-")
}

func transactionDirectoryName(planDigest, entropy string) string {
	return transactionPrefix + strings.TrimPrefix(planDigest, "sha256:") + "-" + entropy
}

func parseTransactionDirectoryName(name, prefix string) (string, bool) {
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(name, prefix)
	if len(rest) != 64+1+32 || rest[64] != '-' {
		return "", false
	}
	digestHex, entropyHex := rest[:64], rest[65:]
	if _, err := hex.DecodeString(digestHex); err != nil {
		return "", false
	}
	if entropy, err := hex.DecodeString(entropyHex); err != nil || len(entropy) != 16 {
		return "", false
	}
	return "sha256:" + strings.ToLower(digestHex), digestHex == strings.ToLower(digestHex) && entropyHex == strings.ToLower(entropyHex)
}

func prepareTransaction(root *os.Root, txn string, plan Plan, admission transactionAdmission) error {
	record := transactionRecord{
		Format:           transactionFormat,
		PlanDigest:       plan.digest,
		BeforeDigest:     plan.before.digest,
		FinalDigest:      plan.finalDigest,
		ChangeCount:      len(plan.changes),
		AdmissionDomain:  admission.Domain,
		AdmissionSubject: admission.SubjectDigest,
		AdmissionReceipt: admission.ReceiptDigest,
	}
	encoded, err := encodeTransactionRecord(record)
	if err != nil {
		return fmt.Errorf("workspace: encode recovery journal: %w", err)
	}
	name := path.Join(txn, transactionRecordName)
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("workspace: create recovery journal: %w", err)
	}
	writeErr := writeAll(file, encoded)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return errors.Join(
			wrapOptional("workspace: persist recovery journal", writeErr),
			wrapOptional("workspace: close recovery journal", closeErr),
		)
	}
	if err := syncDirectory(root, txn); err != nil {
		return err
	}
	if err := syncDirectory(root, "."); err != nil {
		return err
	}
	return nil
}

func recoverTransactions(ctx context.Context, root *os.Root, plan Plan, gate *CommitGate) (transactionRecovery, error) {
	entries, err := readRootDirectory(root)
	if err != nil {
		return transactionRecovery{}, fmt.Errorf("%w: list workspace transactions: %w", ErrRecovery, err)
	}
	active := make([]string, 0, 1)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return transactionRecovery{}, fmt.Errorf("workspace: recover: %w", err)
		}
		name := entry.Name()
		if strings.HasPrefix(name, completedPrefix) {
			if _, valid := parseTransactionDirectoryName(name, completedPrefix); !valid || !entry.IsDir() {
				return transactionRecovery{}, fmt.Errorf("%w: malformed completed transaction %q", ErrRecovery, name)
			}
			// The rename to the completed namespace is the durable commit
			// decision. Cleanup never writes public targets and is safe even if a
			// prior RemoveAll was interrupted after deleting prepared.json.
			if err := cleanupTransaction(root, name); err != nil {
				return transactionRecovery{}, fmt.Errorf("%w: %w", ErrRecovery, err)
			}
			continue
		}
		if strings.HasPrefix(name, transactionPrefix) {
			if _, valid := parseTransactionDirectoryName(name, transactionPrefix); !valid || !entry.IsDir() {
				return transactionRecovery{}, fmt.Errorf("%w: malformed active transaction %q", ErrRecovery, name)
			}
			active = append(active, name)
		}
	}
	if len(active) == 0 {
		return transactionRecovery{}, nil
	}
	if len(active) != 1 {
		return transactionRecovery{}, fmt.Errorf("%w: found %d active transactions", ErrRecovery, len(active))
	}
	txn := active[0]
	digest, _ := parseTransactionDirectoryName(txn, transactionPrefix)
	if digest != plan.digest {
		return transactionRecovery{}, fmt.Errorf("%w: active transaction belongs to plan %s, not %s", ErrRecovery, digest, plan.digest)
	}

	record, prepared, err := loadTransactionRecord(root, txn)
	if err != nil {
		return transactionRecovery{}, fmt.Errorf("%w: transaction %q: %w", ErrRecovery, txn, err)
	}
	if !prepared {
		state, stateErr := classifyState(root, plan)
		if stateErr == nil && state == stateBefore {
			return transactionRecovery{}, cleanupTransaction(root, txn)
		}
		return transactionRecovery{}, fmt.Errorf("%w: transaction %q has no durable prepared record", ErrRecovery, txn)
	}
	if !record.matches(plan, gate) {
		return transactionRecovery{}, fmt.Errorf("%w: transaction %q does not match the supplied plan and admission", ErrRecovery, txn)
	}
	if err := recoverPreparedTransaction(ctx, root, txn, plan); err != nil {
		return transactionRecovery{}, err
	}
	return transactionRecovery{Prepared: true, ReceiptDigest: record.AdmissionReceipt}, nil
}

func recoverPreparedTransaction(ctx context.Context, root *os.Root, txn string, plan Plan) error {
	state, err := classifyState(root, plan)
	if err == nil {
		switch state {
		case stateBefore:
			return cleanupTransaction(root, txn)
		case stateFinal:
			return finalizeTransaction(root, txn)
		}
	}
	if err != nil && !errors.Is(err, ErrChanged) {
		return fmt.Errorf("%w: classify interrupted transaction: %w", ErrRecovery, err)
	}
	if err := rollbackPreparedTransaction(ctx, root, txn, plan); err != nil {
		return fmt.Errorf("%w: transaction %q: %w", ErrRecovery, txn, err)
	}
	return nil
}

// rollbackPreparedTransaction first proves that every changed target is in a
// journal-reachable state. It performs no mutation if any target or backup is
// unrecognizable, so a damaged/forged journal cannot become a write gadget.
func rollbackPreparedTransaction(ctx context.Context, root *os.Root, txn string, plan Plan) error {
	current := make([]File, len(plan.changes))
	for index := range plan.changes {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("workspace: recover rollback: %w", err)
		}
		file, err := captureFile(root, plan.changes[index].public.Path)
		if err != nil {
			return err
		}
		current[index] = file
		before, final := plan.before.files[index], plan.finalFiles[index]
		if before.Exists {
			if file == before {
				continue
			}
			if file != final && file.Exists {
				return fmt.Errorf("target %q is neither its planned before nor interrupted state", file.Path)
			}
			backup := path.Join(txn, fmt.Sprintf("%06d.backup", index))
			if err := verifyEquivalentFile(root, backup, before); err != nil {
				return fmt.Errorf("backup for %q is unavailable: %w", file.Path, err)
			}
			continue
		}
		if file != before && file != final {
			return fmt.Errorf("created target %q is neither missing nor its planned final state", file.Path)
		}
		backup := path.Join(txn, fmt.Sprintf("%06d.backup", index))
		if _, missing, err := inspectPath(root, backup); err != nil || !missing {
			if err != nil {
				return err
			}
			return fmt.Errorf("unexpected backup for create target %q", file.Path)
		}
	}
	if err := verifyRecoverableDirectories(root, plan, current); err != nil {
		return err
	}

	for index := len(plan.changes) - 1; index >= 0; index-- {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("workspace: recover rollback: %w", err)
		}
		before := plan.before.files[index]
		target := plan.changes[index].public.Path
		file, err := captureFile(root, target)
		if err != nil {
			return err
		}
		if file == before {
			continue
		}
		if before.Exists {
			backup := path.Join(txn, fmt.Sprintf("%06d.backup", index))
			if err := verifyEquivalentFile(root, backup, before); err != nil {
				return err
			}
			if file.Exists {
				if err := root.Remove(target); err != nil {
					return fmt.Errorf("remove interrupted target %q: %w", target, err)
				}
				if err := syncDirectory(root, path.Dir(target)); err != nil {
					return err
				}
			}
			if err := root.Rename(backup, target); err != nil {
				return fmt.Errorf("restore interrupted target %q: %w", target, err)
			}
			if err := syncRename(root, backup, target); err != nil {
				return err
			}
		} else if file.Exists {
			if err := root.Remove(target); err != nil {
				return fmt.Errorf("remove interrupted create %q: %w", target, err)
			}
			if err := syncDirectory(root, path.Dir(target)); err != nil {
				return err
			}
		}
		if err := verifyOneTarget(root, before); err != nil {
			return err
		}
	}
	files, err := capturePlanFiles(root, plan)
	if err != nil {
		return err
	}
	if digestFiles(files) != plan.before.digest {
		return fmt.Errorf("rollback did not restore the plan precondition")
	}
	if err := removeRecoveredDirectories(root, plan); err != nil {
		return err
	}
	if err := verifyDirectoryGuards(root, plan, false); err != nil {
		return err
	}
	return cleanupTransaction(root, txn)
}

func finalizeTransaction(root *os.Root, txn string) error {
	if !strings.HasPrefix(txn, transactionPrefix) {
		return fmt.Errorf("%w: invalid active transaction %q", ErrRecovery, txn)
	}
	done := completedPrefix + strings.TrimPrefix(txn, transactionPrefix)
	if err := root.Rename(txn, done); err != nil {
		return fmt.Errorf("workspace: mark transaction committed: %w", err)
	}
	if err := syncDirectory(root, "."); err != nil {
		return err
	}
	return cleanupTransaction(root, done)
}

func loadTransactionRecord(root *os.Root, txn string) (transactionRecord, bool, error) {
	name := path.Join(txn, transactionRecordName)
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return transactionRecord{}, false, nil
	}
	if err != nil {
		return transactionRecord{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&fs.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maxTransactionRecord {
		return transactionRecord{}, false, fmt.Errorf("invalid prepared record")
	}
	file, err := root.Open(name)
	if err != nil {
		return transactionRecord{}, false, err
	}
	content, readErr := io.ReadAll(io.LimitReader(file, maxTransactionRecord+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return transactionRecord{}, false, errors.Join(readErr, closeErr)
	}
	if len(content) > maxTransactionRecord {
		return transactionRecord{}, false, fmt.Errorf("prepared record exceeds %d bytes", maxTransactionRecord)
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var record transactionRecord
	if err := decoder.Decode(&record); err != nil {
		return transactionRecord{}, false, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return transactionRecord{}, false, fmt.Errorf("prepared record has trailing data")
	}
	canonical, err := encodeTransactionRecord(record)
	if err != nil || !bytes.Equal(content, canonical) {
		return transactionRecord{}, false, fmt.Errorf("prepared record is non-canonical")
	}
	return record, true, nil
}

func (record transactionRecord) matches(plan Plan, gate *CommitGate) bool {
	base := record.Format == transactionFormat &&
		record.PlanDigest == plan.digest &&
		record.BeforeDigest == plan.before.digest &&
		record.FinalDigest == plan.finalDigest &&
		record.ChangeCount == len(plan.changes)
	if !base {
		return false
	}
	if gate == nil {
		return record.AdmissionDomain == "" && record.AdmissionSubject == "" && record.AdmissionReceipt == ""
	}
	return record.AdmissionDomain == gate.Domain && record.AdmissionSubject == gate.SubjectDigest &&
		validDigest(record.AdmissionReceipt)
}

func encodeTransactionRecord(record transactionRecord) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(record); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func readRootDirectory(root *os.Root) ([]fs.DirEntry, error) {
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	slices.SortFunc(entries, func(left, right fs.DirEntry) int {
		return strings.Compare(left.Name(), right.Name())
	})
	return entries, nil
}

func syncRename(root *os.Root, from, to string) error {
	fromParent, toParent := path.Dir(from), path.Dir(to)
	if err := syncDirectory(root, fromParent); err != nil {
		return err
	}
	if toParent != fromParent {
		if err := syncDirectory(root, toParent); err != nil {
			return err
		}
	}
	return nil
}

func syncDirectory(root *os.Root, name string) error {
	directory, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("workspace: open directory %q for sync: %w", name, err)
	}
	info, statErr := directory.Stat()
	if statErr == nil && !info.IsDir() {
		statErr = fmt.Errorf("not a directory")
	}
	syncErr := error(nil)
	if statErr == nil {
		syncErr = directory.Sync()
	}
	closeErr := directory.Close()
	if statErr != nil || syncErr != nil || closeErr != nil {
		return errors.Join(
			wrapOptional(fmt.Sprintf("workspace: inspect directory %q for sync", name), statErr),
			wrapOptional(fmt.Sprintf("workspace: sync directory %q", name), syncErr),
			wrapOptional(fmt.Sprintf("workspace: close directory %q after sync", name), closeErr),
		)
	}
	return nil
}

func writeAll(writer io.Writer, content []byte) error {
	for len(content) != 0 {
		written, err := writer.Write(content)
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

func wrapOptional(message string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", message, err)
}
