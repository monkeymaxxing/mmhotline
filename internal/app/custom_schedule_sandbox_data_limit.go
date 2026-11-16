//go:build !race

package app

// customScheduleDataLimitBytes is the child's RLIMIT_DATA, the hard memory
// limit behind the Go memory limit.
const customScheduleDataLimitBytes = 256 << 20

const raceDetectorEnabled = false
