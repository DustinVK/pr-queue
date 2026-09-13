package store

import "testing"

func TestLastRunsUsesTimestampInstants(t *testing.T) {
	for _, timestamps := range [][2]string{
		{"2026-09-12T12:00:00Z", "2026-09-12T12:00:00.1Z"},
		{"2026-09-12T12:00:00.1Z", "2026-09-12T12:00:00.100000001Z"},
		{"2026-09-12T13:00:00+01:00", "2026-09-12T12:00:00.1Z"},
	} {
		t.Run(timestamps[0], func(t *testing.T) {
			s, _ := testStore(t)
			seedPR(t, s)
			for i, id := range []string{"older", "newer"} {
				if _, err := s.DB.Exec(`INSERT INTO review_runs(id,pr_id,head_sha,comparison_key,status,started_at) VALUES (?,1,'h','k','succeeded',?)`, id, timestamps[i]); err != nil {
					t.Fatal(err)
				}
			}
			runs, err := s.LastRuns(t.Context())
			if err != nil || len(runs) != 1 || runs[0].ID != "newer" {
				t.Fatalf("latest run: %+v, %v", runs, err)
			}
		})
	}
}
