package model

import "time"

// Sizing rules from blueprint section 3.6. Each is a starting value for the
// outside review the blueprint asks for, so they live here, named, rather than
// inline in the arithmetic.
const (
	NeedQuantile      = 0.999
	HotFileQuantile   = 0.99
	HeadroomShare     = 0.10
	MinHeadroom       = 256 * MiB
	RiseFactor        = 1.2
	MinCache          = 256 * MiB
	CacheShare        = 0.5
	PeakCheckRatio    = 1.2
	UpwardPressure    = 0.01
	KillLookback      = 7 * 24 * time.Hour
	MediumWindow      = 7 * 24 * time.Hour
	HighWindow        = 14 * 24 * time.Hour
	MinCoverage       = 0.9
	UncalibratedLimit = 0.25
)

// Confidence says how far a snug size can be trusted, from how long and how
// completely Snugbox watched.
type Confidence int

// Confidence levels of blueprint section 3.6.
const (
	ConfidenceLow Confidence = iota
	ConfidenceMedium
	ConfidenceHigh
)

func (c Confidence) String() string {
	switch c {
	case ConfidenceHigh:
		return "high"
	case ConfidenceMedium:
		return "medium"
	default:
		return "low"
	}
}

// HeadroomSource names the rule that set the headroom.
type HeadroomSource string

// The three headroom rules; the largest wins.
const (
	HeadroomShareOfNeed HeadroomSource = "share"
	HeadroomMinimum     HeadroomSource = "minimum"
	HeadroomBurst       HeadroomSource = "burst"
)

// History is what the kernel remembers from before the window. A one-minute
// audit leans on it; a watched box mostly doesn't need it.
type History struct {
	Now    time.Time
	Uptime time.Duration
	// PressureFull is /proc/pressure/memory's full total since boot.
	PressureFull time.Duration
	HasPressure  bool
	// OOMKills is the vmstat oom_kill counter since boot.
	OOMKills uint64
	// Kills holds the times of kills found in the kernel log.
	Kills []time.Time
	// LogRead is true when the kernel log could be read. LogSince is the time
	// of its oldest record; kills older than that are lost from the log.
	LogRead  bool
	LogSince time.Time
}

// Upward collects the signs that a box needs more memory or a fix. When any is
// present, Snugbox never suggests a smaller plan.
type Upward struct {
	// PressureShare is the share of time every task stalled on memory, 0 to 1,
	// measured over PressureBasis.
	PressureShare float64
	PressureBasis time.Duration
	// PressureSinceBoot is true when the share comes from boot-time totals
	// because the window was too short to judge.
	PressureSinceBoot bool
	// RecentKills counts out-of-memory kills in the last seven days.
	// UndatedKills counts kills the kernel counted but whose time is unknown,
	// treated as recent.
	RecentKills  int
	UndatedKills int
	// SwapInIntervals of Intervals had pages coming back from swap.
	SwapInIntervals int
	Intervals       int
}

// Pressure reports whether full pressure averaged above the limit.
func (u Upward) Pressure() bool { return u.PressureShare > UpwardPressure }

// Kills reports whether there were kills in the last seven days, dated or not.
func (u Upward) Kills() bool { return u.RecentKills+u.UndatedKills > 0 }

// Swapping reports whether pages kept coming back from swap: in at least half
// of the sampling intervals.
func (u Upward) Swapping() bool {
	return u.Intervals >= 2 && 2*u.SwapInIntervals >= u.Intervals
}

// Any reports whether any sign is present.
func (u Upward) Any() bool { return u.Pressure() || u.Kills() || u.Swapping() }

// Estimate is the answer of the memory model for one box.
type Estimate struct {
	Need         uint64
	NeedFromHard bool
	Headroom     uint64
	HeadroomFrom HeadroomSource
	// PeakAddition is headroom added because memory.peak shows the workloads
	// used more than Snugbox saw.
	PeakAddition uint64
	PeakSum      uint64
	PeakSampled  uint64
	Cache        uint64
	Snug         uint64
	Confidence   Confidence
	// CacheOverestimate is true when file cache that never came under pressure
	// was counted in full and is a large part of the need, which lowers the
	// confidence one level.
	CacheOverestimate bool
	Upward            Upward
	Window            time.Duration
	Coverage          float64
}

// Size applies blueprint section 3.6 to a summary:
//
//	need     = max(highest hard_total, 99.9th percentile of total)
//	headroom = max(10% of need, 256 MiB, 1.2 x largest 60-second rise)
//	cache    = max(256 MiB, 0.5 x 99th percentile of hot file cache)
//	snug     = need + headroom + cache
//
// plus the memory.peak check, the confidence level and the upward signs.
func Size(s Summary, h History) Estimate {
	e := Estimate{Window: s.Window(), Coverage: s.Coverage()}

	e.Need = max(s.HardTotalMax, s.TotalQ999)
	e.NeedFromHard = s.HardTotalMax >= s.TotalQ999

	e.Headroom, e.HeadroomFrom = scale(e.Need, HeadroomShare), HeadroomShareOfNeed
	if MinHeadroom > e.Headroom {
		e.Headroom, e.HeadroomFrom = MinHeadroom, HeadroomMinimum
	}
	if burst := scale(s.MaxRise, RiseFactor); burst > e.Headroom {
		e.Headroom, e.HeadroomFrom = burst, HeadroomBurst
	}

	e.PeakSum, e.PeakSampled = peakCheck(s)
	if e.PeakSampled > 0 && float64(e.PeakSum) > PeakCheckRatio*float64(e.PeakSampled) {
		e.PeakAddition = e.PeakSum - e.PeakSampled
		e.Headroom += e.PeakAddition
	}

	e.Cache = max(MinCache, scale(s.HotFileQ99, CacheShare))
	e.Snug = e.Need + e.Headroom + e.Cache

	e.Confidence = windowConfidence(e.Window, e.Coverage)
	e.CacheOverestimate = float64(s.UncalibratedHotQ99) > UncalibratedLimit*float64(e.Need)
	if e.CacheOverestimate && e.Confidence > ConfidenceLow {
		e.Confidence--
	}
	e.Upward = upward(s, h)
	return e
}

func windowConfidence(window time.Duration, coverage float64) Confidence {
	switch {
	case coverage < MinCoverage:
		return ConfidenceLow
	case window >= HighWindow:
		return ConfidenceHigh
	case window >= MediumWindow:
		return ConfidenceMedium
	default:
		return ConfidenceLow
	}
}

// Compare only peaks of cgroups present together at the end. A retired
// workload can contribute to the box's historical maximum, so also bound
// the comparison by what the live cgroups individually reached.
func peakCheck(s Summary) (peakSum, sampled uint64) {
	for _, w := range s.Workloads {
		if w.PeakAtEnd {
			peakSum += w.Peak
			sampled += w.CurrentMax
		}
	}
	return peakSum, min(sampled, s.PeakCurrentMax)
}

func upward(s Summary, h History) Upward {
	u := Upward{SwapInIntervals: s.SwapInIntervals, Intervals: s.Intervals}

	window := s.Window()
	switch {
	case window >= 24*time.Hour && s.HasPressure:
		u.PressureShare, u.PressureBasis = share(s.PressureFull, window), window
	case h.HasPressure && h.Uptime >= time.Hour:
		u.PressureShare, u.PressureBasis = share(h.PressureFull, h.Uptime), h.Uptime
		u.PressureSinceBoot = true
	case s.HasPressure && window > 0:
		u.PressureShare, u.PressureBasis = share(s.PressureFull, window), window
	}

	cutoff := h.Now.Add(-KillLookback)
	for _, k := range h.Kills {
		if !k.Before(cutoff) {
			u.RecentKills++
		}
	}
	if logged := uint64(len(h.Kills)); h.OOMKills > logged {
		lost := clampInt(h.OOMKills - logged)
		switch {
		case h.Uptime <= KillLookback:
			u.RecentKills += lost
		case !h.LogRead || h.LogSince.After(cutoff):
			u.UndatedKills += lost
		}
	}
	if windowKills := clampInt(s.OOMKills); windowKills > u.RecentKills+u.UndatedKills {
		u.RecentKills = windowKills - u.UndatedKills
	}
	return u
}

// clampInt converts a counter to int, saturating at a value no kernel counter
// reaches in practice.
func clampInt(n uint64) int {
	const limit = 1 << 31
	if n > limit {
		return limit
	}
	return int(n)
}

func share(part, whole time.Duration) float64 {
	if whole <= 0 {
		return 0
	}
	return float64(part) / float64(whole)
}

// scale multiplies a byte count by f, rounding up.
func scale(b uint64, f float64) uint64 {
	v := float64(b) * f
	n := uint64(v)
	if float64(n) < v {
		n++
	}
	return n
}
