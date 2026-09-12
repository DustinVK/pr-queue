package fixture

import "testing"

func TestDiscountEndpoints(t *testing.T) {
	for _, test := range []struct{ percent, want int }{{0, 1000}, {100, 0}} {
		if got := DiscountedTotal(1000, test.percent); got != test.want {
			t.Fatalf("discount %d: got %d, want %d", test.percent, got, test.want)
		}
	}
}
