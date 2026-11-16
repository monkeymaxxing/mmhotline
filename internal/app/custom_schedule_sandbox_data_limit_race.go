//go:build race

package app

// The race detector's shadow memory does not fit in the sandbox child's usual
// 256 MiB RLIMIT_DATA, so race builds, which are only for tests, give the
// child more. Production builds never use the race detector.
const customScheduleDataLimitBytes = 8 << 30

const raceDetectorEnabled = true
