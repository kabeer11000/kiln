package kiln

import (
	"math"
	"time"
)

// Check reports spend so far, whether the run must stop (BudgetExceeded or TimedOut),
// and whether it has crossed the warn threshold.
func (b Budget) Check(elapsed time.Duration, pricePerHour float64) (spent float64, stop State, warn bool) {
	hours := elapsed.Hours()
	spent = hours * pricePerHour
	switch {
	case spent >= b.MaxUSD:
		return spent, BudgetExceeded, false
	case hours >= b.MaxHours:
		return spent, TimedOut, false
	}
	warn = (pricePerHour > 0 && spent >= b.WarnAt*b.MaxUSD) || hours >= b.WarnAt*b.MaxHours
	return spent, "", warn
}

// Remaining is the time until the first cap is reached. Never below 100ms, so a supervisor
// sleeping on it always makes progress even with float rounding at the boundary.
func (b Budget) Remaining(elapsed time.Duration, pricePerHour float64) time.Duration {
	left := b.MaxHours * float64(time.Hour)
	if pricePerHour > 0 {
		left = min(left, b.MaxUSD/pricePerHour*float64(time.Hour))
	}
	return max(time.Duration(math.Ceil(left))-elapsed, 100*time.Millisecond)
}

// MaxSpend is the most a run can cost: the dollar cap or price x hour cap, whichever is lower.
func (b Budget) MaxSpend(pricePerHour float64) float64 {
	return min(b.MaxUSD, pricePerHour*b.MaxHours)
}
