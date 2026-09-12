# prqueue integration fixture

This small Go package exists only to exercise prqueue against a GitHub PR.
It is separate from the prqueue implementation and must not be merged into main.

`DiscountedTotal` applies an integer percentage discount to a price in cents.
For example, 25% off 1000 cents costs 750 cents.
