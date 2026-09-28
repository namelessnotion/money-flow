package kafkareader

import "testing"

// Lag is what the group still owes: messages up to the partition's end that it
// has not committed. A group that has committed nothing on a partition starts
// at the partition's first offset (StartOffset is FirstOffset), so everything
// retained there is owed, which is exactly what a fresh orchestrator replays.
func TestPartitionLag(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                   string
		committed, first, last int64
		want                   int64
	}{
		{"caught up", 10, 0, 10, 0},
		{"behind", 7, 0, 10, 3},
		{"never committed", -1, 0, 10, 10},
		{"never committed, head truncated by retention", -1, 4, 10, 6},
		{"empty partition, never committed", -1, 0, 0, 0},
		{"committed past a truncated head", 2, 4, 10, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := partitionLag(tc.committed, tc.first, tc.last); got != tc.want {
				t.Errorf("partitionLag(%d, %d, %d) = %d, want %d", tc.committed, tc.first, tc.last, got, tc.want)
			}
		})
	}
}
