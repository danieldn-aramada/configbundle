package backupconfig

import (
	"testing"
	"time"
)

func TestFoldBlobInventory(t *testing.T) {
	t1 := time.Date(2026, 7, 8, 3, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 9, 3, 0, 0, 0, time.UTC) // newest
	t3 := time.Date(2026, 7, 7, 3, 0, 0, 0, time.UTC)

	t.Run("empty", func(t *testing.T) {
		inv := foldBlobInventory(nil)
		if inv.Count != 0 || !inv.LatestModified.IsZero() || inv.LatestBytes != 0 {
			t.Errorf("empty fold = %+v, want zero inventory", inv)
		}
	})

	t.Run("picks newest regardless of order", func(t *testing.T) {
		// newest (t2, 200 bytes) is in the middle — result must not depend on order.
		inv := foldBlobInventory([]blobMeta{
			{modified: t1, bytes: 100},
			{modified: t2, bytes: 200},
			{modified: t3, bytes: 50},
		})
		if inv.Count != 3 {
			t.Errorf("count = %d, want 3", inv.Count)
		}
		if !inv.LatestModified.Equal(t2) {
			t.Errorf("latestModified = %v, want %v", inv.LatestModified, t2)
		}
		if inv.LatestBytes != 200 {
			t.Errorf("latestBytes = %d, want 200 (the newest object's size)", inv.LatestBytes)
		}
	})
}
