//go:build !windows

package monitoring

import "fmt"

// StartSession is a no-op on non-Windows platforms.
func StartSession(
	getStudents func() []StudentInfo,
	sendCmd func(studentID string) error,
	shotCh <-chan ShotMsg,
	exePath string,
) (stop func(), err error) {
	return nil, fmt.Errorf("monitoring is only supported on Windows")
}
