package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestFolderCycleTriggerRejectsUpdate(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, `INSERT INTO users(id, username, password_hash, created_at) VALUES('u1', 'u1', 'h', 1)`); err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct{ id, parent any }{
		{"a", nil},
		{"b", "a"},
		{"c", nil},
	} {
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO folders(id, user_id, name, parent_id, updated_at, synced_at) VALUES(?, 'u1', ?, ?, 10, 10)`,
			f.id, f.id, f.parent,
		); err != nil {
			t.Fatalf("insert %s: %v", f.id, err)
		}
	}

	if _, err := conn.ExecContext(ctx, `UPDATE folders SET parent_id = 'b' WHERE id = 'a'`); err == nil {
		t.Fatal("spostamento a sotto un proprio discendente accettato")
	} else if !strings.Contains(err.Error(), FolderCycleViolationMarker) {
		t.Fatalf("errore inatteso: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE folders SET parent_id = 'a' WHERE id = 'a'`); err == nil {
		t.Fatal("self-parent accettato")
	}
	if _, err := conn.ExecContext(ctx, `UPDATE folders SET parent_id = 'c' WHERE id = 'b'`); err != nil {
		t.Fatalf("spostamento valido rifiutato: %v", err)
	}
}

func TestFolderCycleTriggerRejectsUpsertOfExistingRow(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, `INSERT INTO users(id, username, password_hash, created_at) VALUES('u1', 'u1', 'h', 1)`); err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct{ id, parent any }{
		{"a", nil},
		{"b", "a"},
	} {
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO folders(id, user_id, name, parent_id, updated_at, synced_at) VALUES(?, 'u1', ?, ?, 10, 10)`,
			f.id, f.id, f.parent,
		); err != nil {
			t.Fatal(err)
		}
	}

	_, err = conn.ExecContext(ctx,
		`INSERT INTO folders(id, user_id, name, parent_id, updated_at, synced_at) VALUES('a', 'u1', 'a', 'b', 20, 20)
		 ON CONFLICT(user_id, id) DO UPDATE SET parent_id = excluded.parent_id, updated_at = excluded.updated_at, synced_at = excluded.synced_at`,
	)
	if err == nil || !strings.Contains(err.Error(), FolderCycleViolationMarker) {
		t.Fatalf("upsert ciclico: err = %v, atteso %s", err, FolderCycleViolationMarker)
	}
}

func TestRepairFolderCyclesDetachesOneEdgeAndAdvancesCursor(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, `INSERT INTO users(id, username, password_hash, created_at) VALUES('u1', 'u1', 'h', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO folders(id, user_id, name, parent_id, updated_at, synced_at)
		 VALUES ('a', 'u1', 'a', NULL, 10, 100), ('b', 'u1', 'b', 'a', 20, 100)`,
	); err != nil {
		t.Fatal(err)
	}
	for _, trigger := range []string{"trg_folders_cycle_ins", "trg_folders_cycle_upd"} {
		if _, err := conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS `+trigger); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.ExecContext(ctx, `UPDATE folders SET parent_id = 'b' WHERE id = 'a'`); err != nil {
		t.Fatal(err)
	}

	if err := repairFolderCycles(conn); err != nil {
		t.Fatal(err)
	}

	var roots, total int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM folders WHERE parent_id IS NULL`).Scan(&roots); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM folders`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if roots != 1 || total != 2 {
		t.Fatalf("roots=%d total=%d, attesi 1 e 2", roots, total)
	}
	var updatedAt, syncedAt int64
	if err := conn.QueryRowContext(ctx, `SELECT updated_at, synced_at FROM folders WHERE parent_id IS NULL`).Scan(&updatedAt, &syncedAt); err != nil {
		t.Fatal(err)
	}
	if updatedAt != 10 && updatedAt != 20 {
		t.Fatalf("updated_at modificato dalla riparazione: %d", updatedAt)
	}
	if syncedAt <= 100 {
		t.Fatalf("synced_at non avanzato: %d", syncedAt)
	}
	hwm, err := NewSyncStateRepo(conn).GetInt(ctx, StampHWMKey)
	if err != nil {
		t.Fatal(err)
	}
	if hwm < syncedAt {
		t.Fatalf("hwm=%d < synced_at=%d", hwm, syncedAt)
	}
	if err := repairFolderCycles(conn); err != nil {
		t.Fatal(err)
	}
}

func TestRepairFolderCyclesKeepsTenantsSeparate(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx := context.Background()
	for _, id := range []string{"u1", "u2"} {
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO users(id, username, password_hash, created_at) VALUES(?, ?, 'h', 1)`,
			id, id,
		); err != nil {
			t.Fatal(err)
		}
	}
	// Il trigger impedisce di creare il ciclo: si droppano solo per costruire
	// il fixture storico che la riparazione deve saper gestire.
	for _, trigger := range []string{"trg_folders_cycle_ins", "trg_folders_cycle_upd"} {
		if _, err := conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS `+trigger); err != nil {
			t.Fatal(err)
		}
	}
	// u1 ha un ciclo reale tra C e P. u2 riusa gli STESSI id con una gerarchia
	// perfettamente valida: il vecchio algoritmo, fondendo i grafi per id,
	// poteva staccare la cartella di u2.
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO folders(id, user_id, name, parent_id, updated_at, synced_at)
		 VALUES ('C', 'u1', 'C', NULL, 10, 100),
		        ('P', 'u1', 'P', 'C', 10, 100),
		        ('C', 'u2', 'C', NULL, 10, 100),
		        ('P', 'u2', 'P', 'C', 10, 100)`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE folders SET parent_id = 'P' WHERE user_id = 'u1' AND id = 'C'`); err != nil {
		t.Fatal(err)
	}

	if err := repairFolderCycles(conn); err != nil {
		t.Fatal(err)
	}

	var u1Cycles int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM folders WHERE user_id = 'u1' AND parent_id IS NOT NULL`,
	).Scan(&u1Cycles); err != nil {
		t.Fatal(err)
	}
	if u1Cycles != 1 {
		t.Fatalf("u1: %d cartelle ancora con parent, attesa 1 (ciclo spezzato)", u1Cycles)
	}
	var u2Parent string
	if err := conn.QueryRowContext(ctx,
		`SELECT parent_id FROM folders WHERE user_id = 'u2' AND id = 'P'`,
	).Scan(&u2Parent); err != nil {
		t.Fatal(err)
	}
	if u2Parent != "C" {
		t.Fatalf("u2: parent di P = %q, atteso C (account estraneo modificato)", u2Parent)
	}
}

func TestRedundantIndexesAreDropped(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	for _, index := range []string{"idx_folders_user", "idx_folders_updated", "idx_notes_user", "idx_notes_updated"} {
		var n int
		if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, index).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("indice ridondante ancora presente: %s", index)
		}
	}
}
