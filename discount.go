package fixture

// DiscountedTotal returns a price in cents after a percentage discount.
// The discount is an integer from 0 through 100; fractional cents round down.
func DiscountedTotal(cents, percent int) int {
	return cents - cents*percent/100
}
