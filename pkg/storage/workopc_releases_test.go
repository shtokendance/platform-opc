package storage

import (
	"context"
	"testing"

	"weknora-tokendance/pkg/model"
)

func TestWorkOPCReleaseLifecycle(t *testing.T) {
	s, _, stateDB := setupStateTestStorage(t)
	ctx := context.Background()
	if err := stateDB.AutoMigrate(&model.WorkOPCRelease{}, &model.WorkOPCReleaseArtifact{}, &model.WorkOPCReleaseAuditLog{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}

	r1 := &model.WorkOPCRelease{
		Version:           "0.2.3-alpha.2",
		Channel:           "stable",
		ReleaseNotes:      "initial release",
		RolloutPercentage: 100,
	}
	if err := s.CreateWorkOPCRelease(ctx, r1); err != nil {
		t.Fatalf("create r1: %v", err)
	}

	art := &model.WorkOPCReleaseArtifact{
		ReleaseID: r1.ID,
		Target:    "mac-arm64",
		Kind:      "dmg",
		Filename:  "WorkOPC-0.2.3-alpha.2-mac-arm64.dmg",
		ObjectKey: "releases/stable/0.2.3-alpha.2/mac-arm64/WorkOPC-0.2.3-alpha.2-mac-arm64.dmg",
		SizeBytes: 1024,
		SHA512:    "abc512==",
		SHA256:    "abc256",
		Signed:    true,
		Notarized: true,
	}
	if err := s.UpsertWorkOPCArtifact(ctx, art); err != nil {
		t.Fatalf("upsert artifact: %v", err)
	}

	pub1, err := s.PublishWorkOPCRelease(ctx, r1.ID, "admin")
	if err != nil {
		t.Fatalf("publish r1: %v", err)
	}
	if pub1.Status != "published" || pub1.PublishedAt == nil || len(pub1.Artifacts) != 1 {
		t.Fatalf("unexpected published r1: %+v", pub1)
	}

	r2 := &model.WorkOPCRelease{
		Version:           "0.2.4",
		Channel:           "stable",
		ReleaseNotes:      "second release",
		RolloutPercentage: 50,
	}
	if err := s.CreateWorkOPCRelease(ctx, r2); err != nil {
		t.Fatalf("create r2: %v", err)
	}
	if _, err := s.PublishWorkOPCRelease(ctx, r2.ID, "admin"); err != nil {
		t.Fatalf("publish r2: %v", err)
	}

	gotR1, err := s.GetWorkOPCRelease(ctx, r1.ID)
	if err != nil {
		t.Fatalf("get r1: %v", err)
	}
	if gotR1.Status != "archived" {
		t.Fatalf("expected r1 archived after r2 publish, got %s", gotR1.Status)
	}

	active, err := s.GetPublishedWorkOPCRelease(ctx, "stable")
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	if active.ID != r2.ID {
		t.Fatalf("expected r2 active, got %s", active.ID)
	}

	if _, err := s.PublishWorkOPCRelease(ctx, r1.ID, "admin"); err != nil {
		t.Fatalf("rollback to r1: %v", err)
	}
	active, err = s.GetPublishedWorkOPCRelease(ctx, "stable")
	if err != nil || active.ID != r1.ID {
		t.Fatalf("expected r1 active after rollback, got %+v err=%v", active, err)
	}

	if err := s.IncrementWorkOPCArtifactDownload(ctx, art.ID); err != nil {
		t.Fatalf("increment download: %v", err)
	}
	total, err := s.GetWorkOPCTotalDownloads(ctx)
	if err != nil || total != 1 {
		t.Fatalf("expected total downloads 1, got %d err=%v", total, err)
	}

	if err := s.InsertWorkOPCAuditLog(ctx, &model.WorkOPCReleaseAuditLog{
		ReleaseID: r1.ID,
		Actor:     "admin",
		Action:    "release.publish",
		Detail:    "published 0.2.3-alpha.2",
	}); err != nil {
		t.Fatalf("insert audit: %v", err)
	}
	logs, err := s.ListWorkOPCAuditLogs(ctx, 10)
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected 1 audit log, got %d err=%v", len(logs), err)
	}
}
