package metrics

import "math"

// Wilson returns the Wilson score interval (lower, point, upper) for `success` out of `n`, at 95% confidence. It is how
// the autonomy floor is read: the lower bound is the error rate the evidence actually proves, not the flattering point
// estimate. With n == 0 the point is NaN and the interval spans the whole range.
func Wilson(success, n int) (lower, point, upper float64) {
	const z = 1.96
	if n == 0 {
		return 0, math.NaN(), 1
	}
	fn := float64(n)
	p := float64(success) / fn
	d := 1 + z*z/fn
	centre := (p + z*z/(2*fn)) / d
	half := (z / d) * math.Sqrt(p*(1-p)/fn+z*z/(4*fn*fn))
	return math.Max(0, centre-half), p, math.Min(1, centre+half)
}

// CleanPostsFor is the fewest clean auto-posts (none wrong, none disputed) whose Wilson 95% lower bound clears target —
// the "you need about N in this cell" the dashboard quotes. 0 means no reachable n within the search does.
func CleanPostsFor(target float64) int {
	for n := 1; n <= 5000; n++ {
		if lo, _, _ := Wilson(n, n); lo >= target {
			return n
		}
	}
	return 0
}
