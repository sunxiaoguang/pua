package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

var schedulerV1MigrationTestData = []byte("{\n" +
	"  \"schedules\" : [ { \"updatedAt\": \"2026-08-02T00:00:00Z\", \"target\": \"workspace\", \"condition\": \"tomorrow when green\", \"id\": \"schedule-0123456789abcdef01234567\", \"description\": \"Keep action\", \"createdAt\": \"2026-08-01T00:00:00Z\" } ],\n" +
	"  \"wakeIntervalMinutes\" : 45,\n" +
	"  \"agentBinding\" : { \"name\" : \"default\", \"kind\" : \"profile\" },\n" +
	"  \"schemaVersion\" : 1\n" +
	"}\n")

func writeSchedulerMigrationFixture(t *testing.T, data []byte) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	dir := schedulerPath(root)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := schedulerJSONPath(root)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return root, path, filepath.Join(dir, schedulerV1JSONFile)
}

func requireFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", filepath.Base(path), got, want)
	}
}

func requireMissingFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s exists or cannot be inspected: %v", filepath.Base(path), err)
	}
}

func TestMigrateSchedulerV1PreservesExactRollbackEvidence(t *testing.T) {
	root, path, evidencePath := writeSchedulerMigrationFixture(t, schedulerV1MigrationTestData)

	if err := migrateSchedulerJSONLocked(root); err != nil {
		t.Fatal(err)
	}
	evidence, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(evidence, schedulerV1MigrationTestData) {
		t.Fatalf("rollback evidence did not preserve exact v1 bytes:\nwant=%q\n got=%q", schedulerV1MigrationTestData, evidence)
	}
	requireFileMode(t, evidencePath, 0o644)
	requireFileMode(t, path, 0o644)

	config, err := readSchedulerJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.SchemaVersion != schedulerSchemaVersion || config.AgentBinding != (AgentBinding{Kind: "profile", Name: "default"}) || len(config.Schedules) != 1 {
		t.Fatalf("migrated Scheduler configuration = %#v", config)
	}
	schedule := config.Schedules[0]
	if schedule.ID != "schedule-0123456789abcdef01234567" || schedule.Revision != 1 || schedule.Description != "Keep action" ||
		schedule.Condition != "tomorrow when green" || schedule.Target != "workspace" || schedule.State != ScheduleStateNeedsCompilation || schedule.Trigger != nil ||
		schedule.CreatedAt != "2026-08-01T00:00:00Z" || schedule.UpdatedAt != "2026-08-02T00:00:00Z" {
		t.Fatalf("migrated schedule = %#v", schedule)
	}
}

func TestMigrateSchedulerV1RestartDoesNotMutateEvidence(t *testing.T) {
	root, _, evidencePath := writeSchedulerMigrationFixture(t, schedulerV1MigrationTestData)
	if err := migrateSchedulerJSONLocked(root); err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(evidencePath)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatal(err)
	}

	if err := migrateSchedulerJSONLocked(root); err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(evidencePath)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(beforeInfo, afterInfo) || !bytes.Equal(before, after) || !bytes.Equal(after, schedulerV1MigrationTestData) {
		t.Fatal("repeated migration replaced or mutated matching rollback evidence")
	}
}

func TestMigrateSchedulerV1PersistsEvidenceBeforeUpgradeAndRetries(t *testing.T) {
	root, path, evidencePath := writeSchedulerMigrationFixture(t, schedulerV1MigrationTestData)
	beforeSchedulerInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	upgradeFailure := errors.New("injected v2 write failure")
	err = migrateSchedulerJSONLockedWithWriter(root, func(gotPath string, _ SchedulerConfig) error {
		if gotPath != path {
			t.Fatalf("v2 write path = %q, want %q", gotPath, path)
		}
		evidence, readErr := os.ReadFile(evidencePath)
		if readErr != nil {
			t.Fatalf("read evidence before v2 write: %v", readErr)
		}
		if !bytes.Equal(evidence, schedulerV1MigrationTestData) {
			t.Fatalf("evidence before v2 write = %q", evidence)
		}
		current, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read current definition before v2 write: %v", readErr)
		}
		if !bytes.Equal(current, schedulerV1MigrationTestData) {
			t.Fatalf("scheduler.json changed before v2 write: %q", current)
		}
		return upgradeFailure
	})
	if !errors.Is(err, upgradeFailure) {
		t.Fatalf("injected migration error = %v", err)
	}
	afterFailureInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(beforeSchedulerInfo, afterFailureInfo) {
		t.Fatal("failed v2 write replaced scheduler.json")
	}
	evidenceBeforeRetryInfo, err := os.Stat(evidencePath)
	if err != nil {
		t.Fatal(err)
	}

	if err := migrateSchedulerJSONLocked(root); err != nil {
		t.Fatal(err)
	}
	evidenceAfterRetryInfo, err := os.Stat(evidencePath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(evidenceBeforeRetryInfo, evidenceAfterRetryInfo) {
		t.Fatal("migration retry replaced matching rollback evidence")
	}
	config, err := readSchedulerJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.SchemaVersion != schedulerSchemaVersion || len(config.Schedules) != 1 {
		t.Fatalf("retry result = %#v", config)
	}
}

func TestMigrateSchedulerInvalidV1CreatesNoEvidence(t *testing.T) {
	unknownField := append(bytes.TrimSuffix(schedulerV1MigrationTestData, []byte("}\n")), []byte(",\n  \"unexpected\": true\n}\n")...)
	invalidWakeInterval := bytes.Replace(schedulerV1MigrationTestData, []byte("\"wakeIntervalMinutes\" : 45"), []byte("\"wakeIntervalMinutes\" : 0"), 1)
	for name, invalid := range map[string][]byte{
		"unknown field":            unknownField,
		"invalid v1 wake interval": invalidWakeInterval,
	} {
		t.Run(name, func(t *testing.T) {
			root, path, evidencePath := writeSchedulerMigrationFixture(t, invalid)
			beforeInfo, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}

			if err := migrateSchedulerJSONLocked(root); err == nil {
				t.Fatal("invalid v1 Scheduler configuration unexpectedly migrated")
			}
			afterInfo, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(beforeInfo, afterInfo) || !bytes.Equal(after, invalid) {
				t.Fatal("invalid v1 migration mutated scheduler.json")
			}
			requireMissingFile(t, evidencePath)
		})
	}
}

func TestNativeSchedulerV2InitializationCreatesNoV1Evidence(t *testing.T) {
	workspace, err := Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	evidencePath := filepath.Join(schedulerPath(workspace.Root()), schedulerV1JSONFile)
	requireMissingFile(t, evidencePath)
	if err := migrateSchedulerJSONLocked(workspace.Root()); err != nil {
		t.Fatal(err)
	}
	requireMissingFile(t, evidencePath)
}

func TestMigrateSchedulerV1ConflictingEvidenceFailsWithoutMutation(t *testing.T) {
	root, path, evidencePath := writeSchedulerMigrationFixture(t, schedulerV1MigrationTestData)
	conflict := []byte("{\"schemaVersion\":1,\"conflict\":true}\n")
	if err := os.WriteFile(evidencePath, conflict, 0o644); err != nil {
		t.Fatal(err)
	}
	beforeSchedulerInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeEvidenceInfo, err := os.Stat(evidencePath)
	if err != nil {
		t.Fatal(err)
	}

	if err := migrateSchedulerJSONLocked(root); err == nil {
		t.Fatal("conflicting rollback evidence unexpectedly allowed migration")
	}
	afterSchedulerInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	afterEvidenceInfo, err := os.Stat(evidencePath)
	if err != nil {
		t.Fatal(err)
	}
	afterScheduler, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterEvidence, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(beforeSchedulerInfo, afterSchedulerInfo) || !bytes.Equal(afterScheduler, schedulerV1MigrationTestData) {
		t.Fatal("conflicting rollback evidence mutated scheduler.json")
	}
	if !os.SameFile(beforeEvidenceInfo, afterEvidenceInfo) || !bytes.Equal(afterEvidence, conflict) {
		t.Fatal("conflicting rollback evidence was overwritten or mutated")
	}
}
