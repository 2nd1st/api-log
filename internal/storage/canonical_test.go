package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// FileID's CanonicalPath / MediaSubtree are pure string builders.
// FSExists touches the disk. FileIDFromPath is the inverse of
// CanonicalPath and is the actual correctness-sensitive helper —
// everything else in the package relies on its parsing being right.

func TestCanonicalPath(t *testing.T) {
	cases := []struct {
		name string
		fid  FileID
		want string
	}{
		{
			name: "typical",
			fid:  FileID{DataDir: "/data", Date: "2026-06-01", KeyHash8: "a1b2c3d4"},
			want: "/data/2026-06-01/a1b2c3d4.jsonl",
		},
		{
			name: "datadir with trailing slash gets cleaned by filepath.Join",
			fid:  FileID{DataDir: "/data/", Date: "2026-06-01", KeyHash8: "a1b2c3d4"},
			want: "/data/2026-06-01/a1b2c3d4.jsonl",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.fid.CanonicalPath()
			if got != tc.want {
				t.Errorf("CanonicalPath() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMediaSubtree(t *testing.T) {
	fid := FileID{DataDir: "/data", Date: "2026-06-01", KeyHash8: "a1b2c3d4"}
	want := "/data/2026-06-01/a1b2c3d4/media"
	if got := fid.MediaSubtree(); got != want {
		t.Errorf("MediaSubtree() = %q, want %q", got, want)
	}
}

func TestFSExists(t *testing.T) {
	tmp := t.TempDir()
	fid := FileID{DataDir: tmp, Date: "2026-06-01", KeyHash8: "a1b2c3d4"}

	t.Run("neither form exists", func(t *testing.T) {
		path, err := fid.FSExists()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if path != "" {
			t.Errorf("FSExists() = %q, want empty (neither form on disk)", path)
		}
	})

	t.Run("plain .jsonl only", func(t *testing.T) {
		plain := fid.CanonicalPath()
		if err := os.MkdirAll(filepath.Dir(plain), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(plain, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(plain)
		path, err := fid.FSExists()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if path != plain {
			t.Errorf("FSExists() = %q, want %q", path, plain)
		}
	})

	t.Run("gz only", func(t *testing.T) {
		gz := fid.gzPath()
		if err := os.MkdirAll(filepath.Dir(gz), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(gz, []byte{}, 0o600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(gz)
		path, err := fid.FSExists()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if path != gz {
			t.Errorf("FSExists() = %q, want %q", path, gz)
		}
	})

	t.Run("both forms exist — plain wins (mid-rotation)", func(t *testing.T) {
		plain := fid.CanonicalPath()
		gz := fid.gzPath()
		if err := os.MkdirAll(filepath.Dir(plain), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(plain, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(gz, []byte{}, 0o600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(plain)
		defer os.Remove(gz)
		path, err := fid.FSExists()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if path != plain {
			t.Errorf("FSExists() = %q, want %q (plain wins over .gz during rotation)", path, plain)
		}
	})
}

func TestFileIDFromPath(t *testing.T) {
	cases := []struct {
		name    string
		dataDir string
		path    string
		want    FileID
		wantErr bool
	}{
		{
			name:    "round-trip plain .jsonl",
			dataDir: "/data",
			path:    "/data/2026-06-01/a1b2c3d4.jsonl",
			want:    FileID{DataDir: "/data", Date: "2026-06-01", KeyHash8: "a1b2c3d4"},
		},
		{
			name:    "round-trip .jsonl.gz",
			dataDir: "/data",
			path:    "/data/2026-06-01/a1b2c3d4.jsonl.gz",
			want:    FileID{DataDir: "/data", Date: "2026-06-01", KeyHash8: "a1b2c3d4"},
		},
		{
			name:    "trailing slash on dataDir",
			dataDir: "/data/",
			path:    "/data/2026-06-01/a1b2c3d4.jsonl",
			want:    FileID{DataDir: "/data", Date: "2026-06-01", KeyHash8: "a1b2c3d4"},
		},
		{
			name:    "path not under dataDir",
			dataDir: "/data",
			path:    "/other/2026-06-01/a1b2c3d4.jsonl",
			wantErr: true,
		},
		{
			name:    "missing date segment",
			dataDir: "/data",
			path:    "/data/a1b2c3d4.jsonl",
			wantErr: true,
		},
		{
			name:    "bad date format",
			dataDir: "/data",
			path:    "/data/2026-6-1/a1b2c3d4.jsonl",
			wantErr: true,
		},
		{
			name:    "bad key_hash length",
			dataDir: "/data",
			path:    "/data/2026-06-01/a1b2c3.jsonl",
			wantErr: true,
		},
		{
			name:    "bad key_hash characters",
			dataDir: "/data",
			path:    "/data/2026-06-01/A1B2C3D4.jsonl",
			wantErr: true,
		},
		{
			name:    "missing suffix",
			dataDir: "/data",
			path:    "/data/2026-06-01/a1b2c3d4",
			wantErr: true,
		},
		{
			name:    "empty dataDir",
			dataDir: "",
			path:    "/data/2026-06-01/a1b2c3d4.jsonl",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FileIDFromPath(tc.dataDir, tc.path)
			if tc.wantErr {
				if err == nil {
					t.Errorf("FileIDFromPath() expected error, got nil; result=%+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("FileIDFromPath() unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("FileIDFromPath() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCanonicalPathRoundtripWithFileIDFromPath(t *testing.T) {
	// The contract that ties FileID to its CanonicalPath:
	// for any well-formed FileID, FileIDFromPath(dataDir, CanonicalPath()) == FileID.
	fid := FileID{DataDir: "/data", Date: "2026-06-01", KeyHash8: "a1b2c3d4"}
	got, err := FileIDFromPath(fid.DataDir, fid.CanonicalPath())
	if err != nil {
		t.Fatalf("round-trip failed: %v", err)
	}
	if got != fid {
		t.Errorf("round-trip: got %+v, want %+v", got, fid)
	}
}
