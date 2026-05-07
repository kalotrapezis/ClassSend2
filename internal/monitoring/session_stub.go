//go:build !windows

package monitoring

import "fmt"

// StartSession is a no-op on non-Windows platforms.
func StartSession(
	getStudents func() []StudentInfo,
	sendCmd func(studentID, param string) error,
	shotCh <-chan ShotMsg,
	exePath string,
	onEnded func(),
) (stop func(), nudge func(), err error) {
	return nil, nil, fmt.Errorf("monitoring is only supported on Windows")
}
