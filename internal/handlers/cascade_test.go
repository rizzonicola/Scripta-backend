package handlers

import (
	"context"
	"testing"

	"notes-server/internal/db"
	"notes-server/internal/models"
)

func TestCascadeReachesActiveDescendantsBelowTombstonedFolder(t *testing.T) {
	sqlDB, userID := newStampTestDB(t)
	folders := db.NewFoldersRepo(sqlDB)
	notes := db.NewNotesRepo(sqlDB)
	ctx := context.Background()

	a, b, c := "a", "b", "c"
	bDeletedAt := int64(100)
	rows := []models.Folder{
		{ID: a, UserID: userID, Name: "A", UpdatedAt: 300, SyncedAt: 300},
		{ID: b, UserID: userID, Name: "B", ParentID: &a, UpdatedAt: 100, SyncedAt: 100, DeletedAt: &bDeletedAt},
		{ID: c, UserID: userID, Name: "C", ParentID: &b, UpdatedAt: 300, SyncedAt: 300},
	}
	for _, row := range rows {
		row := row
		if _, err := folders.UpsertLWW(ctx, &row); err != nil {
			t.Fatalf("upsert cartella %s: %v", row.ID, err)
		}
	}

	notesToDelete := []*models.Note{
		{ID: "note-in-b", UserID: userID, Title: "nota B", FolderID: &b, UpdatedAt: 300, SyncedAt: 300},
		{ID: "note-in-c", UserID: userID, Title: "nota C", FolderID: &c, UpdatedAt: 300, SyncedAt: 300},
	}
	for _, note := range notesToDelete {
		if _, err := notes.UpsertLWW(ctx, note); err != nil {
			t.Fatalf("upsert nota %s: %v", note.ID, err)
		}
	}

	if err := cascadeSoftDeleteFolder(ctx, folders, notes, userID, a, 500); err != nil {
		t.Fatalf("cascade: %v", err)
	}

	gotC, err := folders.Get(ctx, userID, c)
	if err != nil {
		t.Fatalf("get C: %v", err)
	}
	if gotC == nil || gotC.DeletedAt == nil {
		t.Fatal("C è ancora attiva sotto il ramo tombstonato")
	}

	for _, note := range notesToDelete {
		gotNote, err := notes.Get(ctx, userID, note.ID)
		if err != nil {
			t.Fatalf("get nota %s: %v", note.ID, err)
		}
		if gotNote == nil || gotNote.DeletedAt == nil {
			t.Fatalf("la nota attiva %s sotto il ramo tombstonato non è stata cancellata", note.ID)
		}
	}
}
