package main

import (
	"os"
	"path/filepath"
	"testing"

	exactonline "github.com/gwillem/exactonline-go"
)

func TestMoveUploadedFilesMovesSuccessfulUpload(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "invoice.pdf")
	if err := os.WriteFile(file, []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}

	moved := moveUploadedFiles([]exactonline.UploadResult{{File: file}})

	want := filepath.Join(dir, "uploaded", "invoice.pdf")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("uploaded file not moved to %s: %v", want, err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("source file still exists or stat failed unexpectedly: %v", err)
	}
	if moved != 1 {
		t.Fatalf("moved = %d, want 1", moved)
	}
}

func TestMoveUploadedFilesMovesAlreadyUploaded(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "invoice.pdf")
	if err := os.WriteFile(file, []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}

	moved := moveUploadedFiles([]exactonline.UploadResult{{File: file, AlreadyUploaded: true}})

	want := filepath.Join(dir, "uploaded", "invoice.pdf")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("already-uploaded file not moved to %s: %v", want, err)
	}
	if moved != 1 {
		t.Fatalf("moved = %d, want 1", moved)
	}
}

func TestMoveUploadedFilesSkipsFailedUpload(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "invoice.pdf")
	if err := os.WriteFile(file, []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}

	moved := moveUploadedFiles([]exactonline.UploadResult{{File: file, Err: os.ErrPermission}})

	if _, err := os.Stat(file); err != nil {
		t.Fatalf("failed upload source file was moved: %v", err)
	}
	if moved != 0 {
		t.Fatalf("moved = %d, want 0", moved)
	}
}
