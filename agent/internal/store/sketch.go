package store

import (
	"fmt"
	"math"

	"github.com/DataDog/sketches-go/ddsketch"
)

// SketchAccuracy is the relative error bound of every stored percentile.
const SketchAccuracy = 0.01

func newSketch() *ddsketch.DDSketch {
	s, err := ddsketch.NewDefaultDDSketch(SketchAccuracy)
	if err != nil {
		panic(fmt.Sprintf("store: a sketch with accuracy %v is invalid: %v", SketchAccuracy, err))
	}
	return s
}

// maxTracked is far above any byte count or core count the store sees, and
// well inside what a sketch can index, so Add can't fail on what passes it.
const maxTracked = 1e30

func addValue(s *ddsketch.DDSketch, v float64) {
	if v < 0 || v > maxTracked || math.IsNaN(v) {
		return
	}
	if err := s.Add(v); err != nil {
		panic(fmt.Sprintf("store: adding %v to a sketch: %v", v, err))
	}
}

func encodeSketch(s *ddsketch.DDSketch) []byte {
	var b []byte
	s.Encode(&b, false)
	return b
}

// upperQuantile returns an upper bound on the q-quantile: the sketch's answer
// divided by one minus the error bound, so a percentile read back from the
// store is never below the true one.
func upperQuantile(s *ddsketch.DDSketch, q float64) uint64 {
	if s == nil || s.IsEmpty() {
		return 0
	}
	v, err := s.GetValueAtQuantile(q)
	if err != nil || v <= 0 {
		return 0
	}
	return uint64(math.Ceil(v / (1 - SketchAccuracy)))
}

func upperQuantileFloat(s *ddsketch.DDSketch, q float64) float64 {
	if s == nil || s.IsEmpty() {
		return 0
	}
	v, err := s.GetValueAtQuantile(q)
	if err != nil || v <= 0 {
		return 0
	}
	return v / (1 - SketchAccuracy)
}
