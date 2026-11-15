package domain

import "testing"

func TestDeliveryPositions(t *testing.T) {
	t.Run("a batch that starts after the newest reply stays put", func(t *testing.T) {
		if got := DeliveryPositions(4.5, 4.5, []float64{6, 7}); got != nil {
			t.Fatalf("positions = %v, want nil", got)
		}
	})
	t.Run("an empty batch has no positions", func(t *testing.T) {
		if got := DeliveryPositions(3, 3, nil); got != nil {
			t.Fatalf("positions = %v, want nil", got)
		}
	})
	t.Run("a batch queued ahead of the reply moves behind every delivered row", func(t *testing.T) {
		// Queued at ids 2 and 3 while the turn that wrote reply 4 and its
		// result at 4.5 streamed; a delivered note took 6 since.
		got := DeliveryPositions(4.5, 6, []float64{2, 3})
		if len(got) != 2 {
			t.Fatalf("positions = %v, want two", got)
		}
		if !(6 < got[0] && got[0] < got[1] && got[1] < 7) {
			t.Fatalf("positions = %v, want ascending strictly between the tail (6) and the next id (7)", got)
		}
	})
	t.Run("a fractional tail keeps the batch below the next whole number", func(t *testing.T) {
		got := DeliveryPositions(4.75, 4.75, []float64{1, 2, 3})
		for i, p := range got {
			if !(4.75 < p && p < 5) || (i > 0 && !(got[i-1] < p)) {
				t.Fatalf("positions = %v, want ascending strictly between 4.75 and 5", got)
			}
		}
	})
}
