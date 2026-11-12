package deploy

import (
	"context"
	"testing"
	"weknora-tokendance/pkg/model"
)

func TestUnavailableStorageIsNotEmptySuccess(t *testing.T) {
	si := NewStorageInspector(t.TempDir() + "/missing.sock")
	_, err := si.InspectAppMountsChecked(context.Background(), "demo", []model.AppStorageMount{{ContainerPath: "/app/data"}})
	if err == nil {
		t.Fatal("must expose inspection failure")
	}
}
