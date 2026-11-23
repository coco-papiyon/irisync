package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectConfig(t *testing.T) {
	for _, tc := range []struct {
		name, settings, owner, id string
		number                    int32
		wantErr                   bool
	}{
		{name: "number with repository owner", settings: "project_number: 4\n", owner: "owner", number: 4},
		{name: "number with explicit owner", settings: "project_number: 6\nproject_owner: other\n", owner: "other", number: 6},
		{name: "legacy ID", settings: "project_id: PVT_example\n", id: "PVT_example"},
		{name: "missing project", wantErr: true},
		{name: "invalid number", settings: "project_number: -4\n", wantErr: true},
		{name: "ambiguous project", settings: "project_number: 4\nproject_id: PVT_example\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "poller.yaml")
			config := "repository: owner/irisync\ngithub_login: owner\n" + tc.settings
			if err := os.WriteFile(path, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfig(path)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected a Project configuration error")
				}
				return
			}
			if err != nil || cfg.ProjectNumber != tc.number || cfg.ProjectOwner != tc.owner || cfg.ProjectID != tc.id {
				t.Fatalf("unexpected Project number=%d owner=%q id=%q err=%v", cfg.ProjectNumber, cfg.ProjectOwner, cfg.ProjectID, err)
			}
		})
	}
}
