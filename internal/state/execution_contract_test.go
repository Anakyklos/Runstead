package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestExecutionContractPersistsAtomicallyAndLoadsExactly(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	material := []byte(`{"contract_version":1,"profile":{"id":"audit","version":"1.0.0"}}`)
	hash := contractTestHash(material)
	if err := store.CreateTask(ctx, TaskRecord{TaskID: "contract-task", Objective: "o", Workspace: "/ws", ExecutionContractJSON: material, ExecutionContractHash: hash}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	gotJSON, gotHash, err := store.LoadExecutionContract(ctx, "contract-task")
	if err != nil {
		t.Fatalf("LoadExecutionContract() error = %v", err)
	}
	if !bytes.Equal(gotJSON, material) || gotHash != hash {
		t.Fatalf("loaded contract = %q, %q; want %q, %q", gotJSON, gotHash, material, hash)
	}
}

func TestExecutionContractRejectsIncompleteOrCorruptPairs(t *testing.T) {
	ctx := context.Background()
	cases := []TaskRecord{
		{TaskID: "one-sided", Objective: "o", Workspace: "/ws", ExecutionContractJSON: []byte(`{}`)},
		{TaskID: "bad-hash", Objective: "o", Workspace: "/ws", ExecutionContractJSON: []byte(`{}`), ExecutionContractHash: "sha256:bad"},
		{TaskID: "bad-json", Objective: "o", Workspace: "/ws", ExecutionContractJSON: []byte(`not-json`), ExecutionContractHash: contractTestHash([]byte(`not-json`))},
		{TaskID: "nested-duplicate", Objective: "o", Workspace: "/ws", ExecutionContractJSON: []byte(`{"contract_version":1,"profile":{"id":"a","id":"b"}}`), ExecutionContractHash: contractTestHash([]byte(`{"contract_version":1,"profile":{"id":"a","id":"b"}}`))},
	}
	for _, record := range cases {
		store := openTestStore(t)
		if err := store.CreateTask(ctx, record); err == nil {
			t.Fatalf("CreateTask(%s) unexpectedly accepted corrupt contract", record.TaskID)
		}
		store.Close()
	}
}

func TestExecutionContractHashBindsExactPersistedBytes(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	material := []byte(`{"contract_version":1,"profile":{"id":"audit","version":"1.0.0"}}`)
	hash := contractTestHash(material)
	if err := store.CreateTask(ctx, TaskRecord{TaskID: "exact-bytes", Objective: "o", Workspace: "/ws", ExecutionContractJSON: material, ExecutionContractHash: hash}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE tasks SET execution_contract_json = ? WHERE task_id = ?`, string(material)+" ", "exact-bytes"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.LoadExecutionContract(ctx, "exact-bytes"); err == nil || !errors.Is(err, ErrExecutionContractCorrupt) {
		t.Fatalf("LoadExecutionContract() error = %v, want ErrExecutionContractCorrupt after byte tamper", err)
	}
}

func TestLegacyTaskWithoutExecutionContractRemainsCompatible(t *testing.T) {
	store := openTestStore(t)
	mustTask(t, store, "legacy")
	data, hash, err := store.LoadExecutionContract(context.Background(), "legacy")
	if err != nil {
		t.Fatalf("LoadExecutionContract() error = %v", err)
	}
	if data != nil || hash != "" {
		t.Fatalf("legacy contract = %q, %q; want empty", data, hash)
	}
}

func TestExecutionContractIntegrityFailureIsTyped(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	material := []byte(`{"contract_version":1}`)
	if err := store.CreateTask(ctx, TaskRecord{TaskID: "tamper", Objective: "o", Workspace: "/ws", ExecutionContractJSON: material, ExecutionContractHash: contractTestHash(material)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE tasks SET execution_contract_hash = ? WHERE task_id = ?`, "sha256:"+strings.Repeat("0", 64), "tamper"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.LoadExecutionContract(ctx, "tamper"); err == nil || !errors.Is(err, ErrExecutionContractCorrupt) {
		t.Fatalf("LoadExecutionContract() error = %v, want ErrExecutionContractCorrupt", err)
	}
}

func contractTestHash(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
